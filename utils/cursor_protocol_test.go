package utils

import (
	"cursor2api-go/models"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCursorProtocolParserParsesThinkingAndToolCallsAcrossChunks(t *testing.T) {
	parser := NewCursorProtocolParser(models.CursorParseConfig{
		TriggerSignal:   "<<CALL_test>>",
		ThinkingEnabled: true,
	})

	var events []models.AssistantEvent
	events = append(events, parser.Feed("Hello <think")...)
	events = append(events, parser.Feed("ing>draft</thinking> world ")...)
	events = append(events, parser.Feed("<<CALL_test>>\n<invoke name=\"lookup\">{\"q\":\"hel")...)
	events = append(events, parser.Feed("lo\"}</invoke>!")...)
	events = append(events, parser.Finish()...)

	// 收集所有 thinking 内容（现在可能是多个增量事件）
	var thinkingContent string
	var textEvents []models.AssistantEvent
	var toolCallEvent *models.AssistantEvent

	for i := range events {
		switch events[i].Kind {
		case models.AssistantEventThinking:
			thinkingContent += events[i].Thinking
		case models.AssistantEventText:
			textEvents = append(textEvents, events[i])
		case models.AssistantEventToolCall:
			toolCallEvent = &events[i]
		}
	}

	// 验证 thinking 完整内容
	if thinkingContent != "draft" {
		t.Fatalf("thinking content = %q, want %q", thinkingContent, "draft")
	}

	// 验证文本事件：至少包含 "Hello " 和 " world " 以及 "!"
	var allText string
	for _, e := range textEvents {
		allText += e.Text
	}
	if !strings.Contains(allText, "Hello ") || !strings.Contains(allText, " world ") || !strings.Contains(allText, "!") {
		t.Fatalf("text content = %q, want to contain 'Hello ', ' world ', '!'", allText)
	}

	// 验证工具调用
	if toolCallEvent == nil || toolCallEvent.ToolCall == nil {
		t.Fatalf("expected a tool call event")
	}
	if toolCallEvent.ToolCall.Function.Name != "lookup" {
		t.Fatalf("tool name = %v, want lookup", toolCallEvent.ToolCall.Function.Name)
	}
	if toolCallEvent.ToolCall.Function.Arguments != `{"q":"hello"}` {
		t.Fatalf("tool arguments = %v, want compact json", toolCallEvent.ToolCall.Function.Arguments)
	}
}

// TestStreamingThinkingIncrementalOutput 验证 thinking 内容跨多个 chunk 时的增量输出行为
func TestStreamingThinkingIncrementalOutput(t *testing.T) {
	parser := NewCursorProtocolParser(models.CursorParseConfig{
		ThinkingEnabled: true,
	})

	// 第一个 chunk：开标签 + 部分内容
	events1 := parser.Feed("<thinking>This is a long thought")
	// 应该已经产出了 thinking 增量（扣除尾部缓冲）
	var thinking1 string
	for _, e := range events1 {
		if e.Kind != models.AssistantEventThinking {
			t.Fatalf("expected thinking event, got %v", e.Kind)
		}
		thinking1 += e.Thinking
	}
	if thinking1 == "" {
		t.Fatalf("first chunk should produce incremental thinking output")
	}

	// 第二个 chunk：更多内容
	events2 := parser.Feed(" that continues across chunks")
	var thinking2 string
	for _, e := range events2 {
		if e.Kind != models.AssistantEventThinking {
			t.Fatalf("expected thinking event, got %v", e.Kind)
		}
		thinking2 += e.Thinking
	}

	// 第三个 chunk：闭合标签 + 后续文本
	events3 := parser.Feed("</thinking>Final answer.")
	var thinking3 string
	var finalText string
	for _, e := range events3 {
		switch e.Kind {
		case models.AssistantEventThinking:
			thinking3 += e.Thinking
		case models.AssistantEventText:
			finalText += e.Text
		}
	}

	events4 := parser.Finish()
	for _, e := range events4 {
		if e.Kind == models.AssistantEventText {
			finalText += e.Text
		}
	}

	// 验证完整 thinking 内容一致
	fullThinking := thinking1 + thinking2 + thinking3
	expected := "This is a long thought that continues across chunks"
	if fullThinking != expected {
		t.Fatalf("full thinking = %q, want %q", fullThinking, expected)
	}

	// 验证后续文本
	if finalText != "Final answer." {
		t.Fatalf("final text = %q, want %q", finalText, "Final answer.")
	}
}

// TestStreamingThinkingPartialEndTag 验证 </thinking> 标签拆分到两个 chunk 时不会误截
func TestStreamingThinkingPartialEndTag(t *testing.T) {
	parser := NewCursorProtocolParser(models.CursorParseConfig{
		ThinkingEnabled: true,
	})

	var allEvents []models.AssistantEvent
	allEvents = append(allEvents, parser.Feed("<thinking>Content</")...)   // "</" 是 "</thinking>" 的前缀
	allEvents = append(allEvents, parser.Feed("thinking>Done")...)         // 补全闭合标签
	allEvents = append(allEvents, parser.Finish()...)

	var thinking string
	var text string
	for _, e := range allEvents {
		switch e.Kind {
		case models.AssistantEventThinking:
			thinking += e.Thinking
		case models.AssistantEventText:
			text += e.Text
		}
	}

	if thinking != "Content" {
		t.Fatalf("thinking = %q, want %q", thinking, "Content")
	}
	if text != "Done" {
		t.Fatalf("text = %q, want %q", text, "Done")
	}
}


func TestNonStreamChatCompletionReturnsToolCalls(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)

	ch := make(chan interface{}, 4)
	ch <- models.AssistantEvent{Kind: models.AssistantEventText, Text: "Let me check."}
	ch <- models.AssistantEvent{
		Kind: models.AssistantEventToolCall,
		ToolCall: &models.ToolCall{
			ID:   "call_1",
			Type: "function",
			Function: models.FunctionCall{
				Name:      "lookup",
				Arguments: `{"q":"revivalquant"}`,
			},
		},
	}
	ch <- models.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	close(ch)

	NonStreamChatCompletion(ctx, ch, "claude-sonnet-4.6")

	var response models.ChatCompletionResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if response.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %v, want tool_calls", response.Choices[0].FinishReason)
	}
	if response.Choices[0].Message.ToolCalls[0].Function.Name != "lookup" {
		t.Fatalf("tool call name = %v, want lookup", response.Choices[0].Message.ToolCalls[0].Function.Name)
	}
	if response.Choices[0].Message.Content != "Let me check." {
		t.Fatalf("message content = %#v, want Let me check.", response.Choices[0].Message.Content)
	}
}

func TestStreamChatCompletionEmitsToolCallChunks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)

	ch := make(chan interface{}, 2)
	ch <- models.AssistantEvent{
		Kind: models.AssistantEventToolCall,
		ToolCall: &models.ToolCall{
			ID:   "call_1",
			Type: "function",
			Function: models.FunctionCall{
				Name:      "lookup",
				Arguments: `{"q":"revivalquant"}`,
			},
		},
	}
	close(ch)

	StreamChatCompletion(ctx, ch, "claude-sonnet-4.6")

	body := recorder.Body.String()
	if !strings.Contains(body, `"tool_calls":[{"index":0,"id":"call_1","type":"function"`) {
		t.Fatalf("stream body missing tool_calls delta: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("stream body missing tool_calls finish reason: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream body missing DONE marker: %s", body)
	}
}
