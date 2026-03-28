package handlers

import (
	"cursor2api-go/models"
	"cursor2api-go/services"
	"cursor2api-go/utils"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

func (h *Handler) AnthropicMessages(c *gin.Context) {
	cfg := h.config
	var req models.AnthropicMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{"type": "invalid_request_error", "message": err.Error()}})
		return
	}

		// 打印前端传入的请求参数 JSON
	// if reqJSON, err := json.Marshal(req); err == nil {
	// 	fmt.Println("Request JSON:", string(reqJSON))
	// }

	// 将 Anthropic Request 转换为内置的 ChatCompletionRequest 以复用强大的 CursorService
	openAIReq := models.ChatCompletionRequest{
		Model:           req.Model,
		Stream:          req.Stream,
		Messages:        make([]models.Message, 0, len(req.Messages)+1),
		Tools:           make([]models.Tool, 0, len(req.Tools)),
		IsAnthropicMode: true,
	}

	// 转换 System 为 Message
	if req.System != nil {
		sysText := ""
		switch v := req.System.(type) {
		case string:
			sysText = v
		case []interface{}:
			for _, item := range v {
				if m, ok := item.(map[string]interface{}); ok {
					if txt, exists := m["text"].(string); exists {
						sysText += txt + "\n"
					}
				}
			}
		}
		if sysText != "" {
			openAIReq.Messages = append(openAIReq.Messages, models.Message{
				Role:    "system",
				Content: sysText,
			})
		}
	}

	// 深度转换 Message，处理 tool_use 和 tool_result 历史
	for _, m := range req.Messages {
		if contentStr, ok := m.Content.(string); ok {
			openAIReq.Messages = append(openAIReq.Messages, models.Message{
				Role:    m.Role,
				Content: contentStr,
			})
			continue
		}

		// 处理 []AnthropicContentBlock 格式
		var blocks []models.AnthropicContentBlock
		b, _ := json.Marshal(m.Content)
		_ = json.Unmarshal(b, &blocks)

		var textContent string
		var toolCalls []models.ToolCall

		for _, block := range blocks {
			switch block.Type {
			case "text":
				textContent += block.Text
			case "tool_use":
				argsStr := "{}"
				if len(block.Input) > 0 {
					argsStr = string(block.Input)
				}
				toolCalls = append(toolCalls, models.ToolCall{
					ID:   block.ID,
					Type: "function",
					Function: models.FunctionCall{
						Name:      block.Name,
						Arguments: argsStr,
					},
				})
			case "tool_result":
				// tool_result 在 Anthropic 里是在 User 角色下。我们需要在 openai 格式中独立出一个 tool message
				resultStr := ""
				if bs, ok := block.Content.(string); ok {
					resultStr = bs
				} else {
					bb, _ := json.Marshal(block.Content)
					resultStr = string(bb)
				}
				openAIReq.Messages = append(openAIReq.Messages, models.Message{
					Role:       "tool",
					Content:    resultStr,
					ToolCallID: block.ToolUseID,
				})
			}
		}

		// 如果当前有提取出正常的文本或 toolCalls，塞入 assistant 消息
		if len(toolCalls) > 0 || textContent != "" {
			role := m.Role
			// 注意：有历史 tool_result 的那条如果只携带了 text，刚才没塞进列表。为了兜底，只对非 user且含有内容的塞
			if role == "assistant" || textContent != "" {
				openAIReq.Messages = append(openAIReq.Messages, models.Message{
					Role:      role,
					Content:   textContent,
					ToolCalls: toolCalls,
				})
			}
		}
	}

	// 简单转换 Tools
	for _, t := range req.Tools {
        var param map[string]interface{}
        if t.InputSchema != nil {
            b, _ := json.Marshal(t.InputSchema)
            _ = json.Unmarshal(b, &param)
        }
		openAIReq.Tools = append(openAIReq.Tools, models.Tool{
			Type: "function",
			Function: models.FunctionDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  param,
			},
		})
	}

	// 构建复用服务
	cursorService := services.NewCursorService(cfg)
	ctx := c.Request.Context()

	resultChan, err := cursorService.ChatCompletion(ctx, &openAIReq)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"type": "error", "error": gin.H{"type": "api_error", "message": err.Error()}})
		return
	}

	if !req.Stream {
		// 收集 resultChan 中的所有事件，聚合为非流式 Anthropic 响应
		var contentBlocks []interface{}
		hasToolUse := false
		var pendingThinking string // 积累多段 thinking 片段

		for ev := range resultChan {
			switch v := ev.(type) {
			case models.AssistantEvent:
				switch v.Kind {
				case models.AssistantEventThinking:
					pendingThinking += v.Thinking
				case models.AssistantEventText:
					// 如果之前有积累的 thinking，先落地为 thinking 块
					if pendingThinking != "" {
						contentBlocks = append(contentBlocks, gin.H{"type": "thinking", "thinking": pendingThinking})
						pendingThinking = ""
					}
					// 将文本追加到最后一个 text 块，或新建
					if len(contentBlocks) > 0 {
						if last, ok := contentBlocks[len(contentBlocks)-1].(gin.H); ok && last["type"] == "text" {
							last["text"] = last["text"].(string) + v.Text
							break
						}
					}
					contentBlocks = append(contentBlocks, gin.H{"type": "text", "text": v.Text})
				case models.AssistantEventToolCall:
					hasToolUse = true
					var inputRaw interface{}
					if err := json.Unmarshal([]byte(v.ToolCall.Function.Arguments), &inputRaw); err != nil {
						inputRaw = json.RawMessage(v.ToolCall.Function.Arguments)
					}
					contentBlocks = append(contentBlocks, gin.H{
						"type":  "tool_use",
						"id":    v.ToolCall.ID,
						"name":  v.ToolCall.Function.Name,
						"input": inputRaw,
					})
				}
			case error:
				c.JSON(http.StatusInternalServerError, gin.H{"type": "error", "error": gin.H{"type": "api_error", "message": v.Error()}})
				return
			}
		}
		// 剩余 thinking
		if pendingThinking != "" {
			contentBlocks = append(contentBlocks, gin.H{"type": "thinking", "thinking": pendingThinking})
		}
		if contentBlocks == nil {
			contentBlocks = []interface{}{}
		}

		stopReason := "end_turn"
		if hasToolUse {
			stopReason = "tool_use"
		}

		c.JSON(http.StatusOK, gin.H{
			"id":            "msg_cursor_" + utilsGenerateRandomId(),
			"type":          "message",
			"role":          "assistant",
			"model":         req.Model,
			"content":       contentBlocks,
			"stop_reason":   stopReason,
			"stop_sequence": nil,
			"usage": gin.H{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		})
		return
	}

	// === 流式响应：绕过 c.Stream()，直接操控 http.Flusher 实现真·实时推送 ===
	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, hasFlusher := c.Writer.(http.Flusher)

	// 发送 message_start
	initEvt := gin.H{
		"type": "message_start",
		"message": gin.H{
			"id":            "msg_cursor_" + utilsGenerateRandomId(),
			"type":          "message",
			"role":          "assistant",
			"model":         req.Model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         gin.H{"input_tokens": 0, "output_tokens": 0},
		},
	}
	writeSSEFlush(w, flusher, hasFlusher, "message_start", initEvt)

	contentBlockIdx := 0
	inTextBlock := false
	inThinkingBlock := false
	hasToolCall := false

StreamLoop:
	for ev := range resultChan {
		switch v := ev.(type) {
		case models.AssistantEvent:
			switch v.Kind {
			case models.AssistantEventThinking:
				if hasToolCall {
					break StreamLoop
				}
				if v.Thinking == "" {
					continue
				}
				// 关闭 text 块（如果有）
				if inTextBlock {
					writeSSEFlush(w, flusher, hasFlusher, "content_block_stop", gin.H{"type": "content_block_stop", "index": contentBlockIdx})
					contentBlockIdx++
					inTextBlock = false
				}
				// 开启 thinking 块（如果未开启）
				if !inThinkingBlock {
					writeSSEFlush(w, flusher, hasFlusher, "content_block_start", gin.H{
						"type":          "content_block_start",
						"index":         contentBlockIdx,
						"content_block": gin.H{"type": "thinking", "thinking": ""},
					})
					inThinkingBlock = true
				}
				writeSSEFlush(w, flusher, hasFlusher, "content_block_delta", gin.H{
					"type":  "content_block_delta",
					"index": contentBlockIdx,
					"delta": gin.H{"type": "thinking_delta", "thinking": v.Thinking},
				})
			case models.AssistantEventText:
				if hasToolCall {
					break StreamLoop
				}
				if v.Text == "" {
					continue
				}
				// 关闭 thinking 块（如果有）
				if inThinkingBlock {
					writeSSEFlush(w, flusher, hasFlusher, "content_block_stop", gin.H{"type": "content_block_stop", "index": contentBlockIdx})
					contentBlockIdx++
					inThinkingBlock = false
				}
				// 开启 text 块（如果未开启）
				if !inTextBlock {
					writeSSEFlush(w, flusher, hasFlusher, "content_block_start", gin.H{
						"type":          "content_block_start",
						"index":         contentBlockIdx,
						"content_block": gin.H{"type": "text", "text": ""},
					})
					inTextBlock = true
				}
				writeSSEFlush(w, flusher, hasFlusher, "content_block_delta", gin.H{
					"type":  "content_block_delta",
					"index": contentBlockIdx,
					"delta": gin.H{"type": "text_delta", "text": v.Text},
				})
			case models.AssistantEventToolCall:
				hasToolCall = true
				// 关闭 thinking 块
				if inThinkingBlock {
					writeSSEFlush(w, flusher, hasFlusher, "content_block_stop", gin.H{"type": "content_block_stop", "index": contentBlockIdx})
					contentBlockIdx++
					inThinkingBlock = false
				}
				// 关闭 text 块
				if inTextBlock {
					writeSSEFlush(w, flusher, hasFlusher, "content_block_stop", gin.H{"type": "content_block_stop", "index": contentBlockIdx})
					contentBlockIdx++
					inTextBlock = false
				}
				// 发送 tool_use 块
				writeSSEFlush(w, flusher, hasFlusher, "content_block_start", gin.H{
					"type":  "content_block_start",
					"index": contentBlockIdx,
					"content_block": gin.H{
						"type":  "tool_use",
						"id":    v.ToolCall.ID,
						"name":  v.ToolCall.Function.Name,
						"input": gin.H{},
					},
				})
				writeSSEFlush(w, flusher, hasFlusher, "content_block_delta", gin.H{
					"type":  "content_block_delta",
					"index": contentBlockIdx,
					"delta": gin.H{"type": "input_json_delta", "partial_json": v.ToolCall.Function.Arguments},
				})
				writeSSEFlush(w, flusher, hasFlusher, "content_block_stop", gin.H{"type": "content_block_stop", "index": contentBlockIdx})
				contentBlockIdx++
			}
		case error:
			writeSSEFlush(w, flusher, hasFlusher, "error", gin.H{"type": "error", "error": gin.H{"type": "api_error", "message": v.Error()}})
			return
		}
	}

	// 关闭尚未关闭的块
	if inThinkingBlock || inTextBlock {
		writeSSEFlush(w, flusher, hasFlusher, "content_block_stop", gin.H{"type": "content_block_stop", "index": contentBlockIdx})
	}

	stopReason := "end_turn"
	if hasToolCall {
		stopReason = "tool_use"
	}
	writeSSEFlush(w, flusher, hasFlusher, "message_delta", gin.H{
		"type":  "message_delta",
		"delta": gin.H{"stop_reason": stopReason},
		"usage": gin.H{"output_tokens": 0},
	})
	writeSSEFlush(w, flusher, hasFlusher, "message_stop", gin.H{"type": "message_stop"})
}

func writeSSE(w io.Writer, eventName string, data interface{}) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, b)
}

func writeSSEFlush(w http.ResponseWriter, flusher http.Flusher, hasFlusher bool, eventName string, data interface{}) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, b)
	if hasFlusher {
		flusher.Flush()
	}
}

func utilsGenerateRandomId() string {
	return utils.GenerateRandomString(24)
}
