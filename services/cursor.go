// Copyright (c) 2025-2026 libaxuan
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package services

import (
	"bufio"
	"context"
	"cursor2api-go/config"
	"cursor2api-go/middleware"
	"cursor2api-go/models"
	"cursor2api-go/utils"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/imroc/req/v3"
	"github.com/sirupsen/logrus"
)

const cursorAPIURL = "https://cursor.com/api/chat"

// CursorService handles interactions with Cursor API.
type CursorService struct {
	config          *config.Config
	client          *req.Client
	mainJS          string
	envJS           string
	scriptCache     string
	scriptCacheTime time.Time
	scriptMutex     sync.RWMutex
	headerGenerator *utils.HeaderGenerator
}

// NewCursorService creates a new service instance.
func NewCursorService(cfg *config.Config) *CursorService {
	mainJS, err := os.ReadFile(filepath.Join("jscode", "main.js"))
	if err != nil {
		logrus.Fatalf("failed to read jscode/main.js: %v", err)
	}

	envJS, err := os.ReadFile(filepath.Join("jscode", "env.js"))
	if err != nil {
		logrus.Fatalf("failed to read jscode/env.js: %v", err)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		logrus.Warnf("failed to create cookie jar: %v", err)
	}

	client := req.C()
	// 禁用全局超时，以支持长耗时的流式响应
	client.SetTimeout(0)

	// 设置响应头超时，确保在指定时间内能收到服务器响应
	// req/v3 的 Transport 包装了 http.Transport
	client.GetTransport().SetResponseHeaderTimeout(time.Duration(cfg.Timeout) * time.Second)

	client.ImpersonateChrome()
	if jar != nil {
		client.SetCookieJar(jar)
	}

	return &CursorService{
		config:          cfg,
		client:          client,
		mainJS:          string(mainJS),
		envJS:           string(envJS),
		headerGenerator: utils.NewHeaderGenerator(),
	}
}

// ChatCompletion creates a chat completion stream for the given request.
func (s *CursorService) ChatCompletion(ctx context.Context, request *models.ChatCompletionRequest) (<-chan interface{}, error) {
	buildResult, err := s.buildCursorRequest(request)
	if err != nil {
		return nil, err
	}

	jsonPayload, err := json.Marshal(buildResult.Payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal cursor payload: %w", err)
	}

	// 尝试最多2次
	maxRetries := 2
	for attempt := 1; attempt <= maxRetries; attempt++ {
		xIsHuman, err := s.fetchXIsHuman(ctx)
		if err != nil {
			if attempt < maxRetries {
				logrus.WithError(err).Warnf("Failed to fetch x-is-human token (attempt %d/%d), retrying...", attempt, maxRetries)
				time.Sleep(time.Second * time.Duration(attempt)) // 指数退避
				continue
			}
			return nil, err
		}

		// 添加详细的调试日志
		headers := s.chatHeaders(xIsHuman)
		
		// DUMP FULL REQUEST FOR USER DEBUGGING
		logrus.Debugf("================== FULL CURSOR REQUEST FOR POSTMAN ==================")
		logrus.Debugf("URL: POST %s", cursorAPIURL)
		logrus.Debugf("Headers:")
		for k, v := range headers {
			logrus.Debugf("  %s: %s", k, v)
		}
		logrus.Debugf("Body Payload (JSON):")
		logrus.Debugf("%s", string(jsonPayload))
		logrus.Debugf("=====================================================================")

		logrus.WithFields(logrus.Fields{
			"url":            cursorAPIURL,
			"x-is-human":     xIsHuman[:50] + "...", // 只显示前50个字符
			"payload_length": len(jsonPayload),
			"model":          request.Model,
			"attempt":        attempt,
		}).Debug("Sending request to Cursor API")

		resp, err := s.client.R().
			SetContext(ctx).
			SetHeaders(headers).
			SetBody(jsonPayload).
			DisableAutoReadResponse().
			Post(cursorAPIURL)
		if err != nil {
			if attempt < maxRetries {
				logrus.WithError(err).Warnf("Cursor request failed (attempt %d/%d), retrying...", attempt, maxRetries)
				time.Sleep(time.Second * time.Duration(attempt))
				continue
			}
			return nil, fmt.Errorf("cursor request failed: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Response.Body)
			resp.Response.Body.Close()
			message := strings.TrimSpace(string(body))

			// 记录详细的错误信息
			logrus.WithFields(logrus.Fields{
				"status_code": resp.StatusCode,
				"response":    message,
				"headers":     resp.Header,
				"attempt":     attempt,
			}).Error("Cursor API returned non-OK status")

			// 如果是 403 错误且还有重试机会,清除缓存并重试
			if resp.StatusCode == http.StatusForbidden && attempt < maxRetries {
				logrus.Warn("Received 403 Access Denied, refreshing browser fingerprint and clearing token cache...")

				// 刷新浏览器指纹
				s.headerGenerator.Refresh()
				logrus.WithFields(logrus.Fields{
					"platform":       s.headerGenerator.GetProfile().Platform,
					"chrome_version": s.headerGenerator.GetProfile().ChromeVersion,
				}).Debug("Refreshed browser fingerprint")

				// 清除 token 缓存
				s.scriptMutex.Lock()
				s.scriptCache = ""
				s.scriptCacheTime = time.Time{}
				s.scriptMutex.Unlock()

				time.Sleep(time.Second * time.Duration(attempt))
				continue
			}

			if strings.Contains(message, "Attention Required! | Cloudflare") {
				message = "Cloudflare 403"
			}
			return nil, middleware.NewCursorWebError(resp.StatusCode, message)
		}

		// 成功,返回结果
		output := make(chan interface{}, 32)
		go s.consumeSSE(ctx, resp.Response, output, buildResult.ParseConfig)
		return output, nil
	}

	return nil, fmt.Errorf("failed after %d attempts", maxRetries)
}

type nonStreamCollectResult struct {
	Message      models.Message
	FinishReason string
	Usage        models.Usage
	ToolCalls    []models.ToolCall
	Text         string
}

func (s *CursorService) collectNonStream(ctx context.Context, gen <-chan interface{}, modelName string) (nonStreamCollectResult, error) {
	var fullContent strings.Builder
	var usage models.Usage
	toolCalls := make([]models.ToolCall, 0, 2)
	finishReason := "stop"

	for {
		select {
		case <-ctx.Done():
			return nonStreamCollectResult{}, ctx.Err()
		case data, ok := <-gen:
			flushAndReturn := func() (nonStreamCollectResult, error) {
				msg := models.Message{Role: "assistant"}
				if fullContent.Len() > 0 || len(toolCalls) == 0 {
					msg.Content = fullContent.String()
				}
				if len(toolCalls) > 0 {
					msg.ToolCalls = toolCalls
					finishReason = "tool_calls"
				}
				return nonStreamCollectResult{
					Message:      msg,
					FinishReason: finishReason,
					Usage:        usage,
					ToolCalls:    toolCalls,
					Text:         fullContent.String(),
				}, nil
			}

			if !ok {
				return flushAndReturn()
			}

			switch v := data.(type) {
			case models.AssistantEvent:
				switch v.Kind {
				case models.AssistantEventText:
					if len(toolCalls) > 0 {
						if strings.TrimSpace(v.Text) != "" {
							return flushAndReturn()
						}
						continue
					}
					fullContent.WriteString(v.Text)
				case models.AssistantEventToolCall:
					if v.ToolCall != nil {
						toolCalls = append(toolCalls, *v.ToolCall)
					}
				case models.AssistantEventThinking:
					if len(toolCalls) > 0 {
						return flushAndReturn()
					}
					// thinking 对于 OpenAI chat.completion 的 message.content 不直接暴露
					continue
				}
			case string:
				if len(toolCalls) > 0 {
					if strings.TrimSpace(v) != "" {
						return flushAndReturn()
					}
					continue
				}
				fullContent.WriteString(v)
			case models.Usage:
				usage = v
			case error:
				return nonStreamCollectResult{}, v
			default:
				continue
			}
		}
	}
}

func (s *CursorService) toolCallRequiredForRequest(request *models.ChatCompletionRequest) (bool, toolChoiceSpec, error) {
	choice, err := parseToolChoice(request.ToolChoice)
	if err != nil {
		return false, toolChoiceSpec{}, err
	}
	if s.config != nil && s.config.KiloToolStrict && len(request.Tools) > 0 && choice.Mode == "auto" {
		choice.Mode = "required"
	}
	if len(request.Tools) == 0 {
		return false, choice, nil
	}
	return choice.Mode == "required" || choice.Mode == "function", choice, nil
}

func (s *CursorService) withToolRetrySystemMessage(request *models.ChatCompletionRequest, choice toolChoiceSpec) *models.ChatCompletionRequest {
	cloned := *request
	cloned.Messages = append([]models.Message(nil), request.Messages...)

	var b strings.Builder
	b.WriteString("TOOL USE REQUIRED.\n")
	b.WriteString("Your next assistant message MUST be a tool call and must contain only the tool call in the exact bridge format. Do not output any natural language.\n")
	if choice.Mode == "function" && strings.TrimSpace(choice.FunctionName) != "" {
		b.WriteString(fmt.Sprintf("You MUST call function %q.\n", strings.TrimSpace(choice.FunctionName)))
	} else {
		b.WriteString("You MUST call at least one tool.\n")
	}
	b.WriteString("After receiving the tool result, you will provide the final answer.\n")

	sys := models.Message{Role: "system", Content: b.String()}
	cloned.Messages = append([]models.Message{sys}, cloned.Messages...)
	return &cloned
}

// ChatCompletionNonStream runs a non-stream chat completion and returns a single OpenAI-compatible response.
// It includes a Kilo-compatibility retry: if tools are provided and tool use is required but no tool_calls
// are produced, it retries once with a stronger system instruction.
func (s *CursorService) ChatCompletionNonStream(ctx context.Context, request *models.ChatCompletionRequest) (*models.ChatCompletionResponse, error) {
	required, choice, err := s.toolCallRequiredForRequest(request)
	if err != nil {
		return nil, middleware.NewRequestValidationError(err.Error(), "invalid_tool_choice")
	}

	runOnce := func(req *models.ChatCompletionRequest) (nonStreamCollectResult, error) {
		gen, err := s.ChatCompletion(ctx, req)
		if err != nil {
			return nonStreamCollectResult{}, err
		}
		return s.collectNonStream(ctx, gen, req.Model)
	}

	result, err := runOnce(request)
	if err != nil {
		return nil, err
	}

	if required && len(result.ToolCalls) == 0 {
		retryReq := s.withToolRetrySystemMessage(request, choice)
		retryResult, retryErr := runOnce(retryReq)
		if retryErr == nil {
			result = retryResult
		} else {
			logrus.WithError(retryErr).Warn("tool-required retry failed; returning first attempt")
		}
	}

	respID := utils.GenerateChatCompletionID()
	return models.NewChatCompletionResponse(respID, request.Model, result.Message, result.FinishReason, result.Usage), nil
}

func (s *CursorService) consumeSSE(ctx context.Context, resp *http.Response, output chan interface{}, parseConfig models.CursorParseConfig) {
	defer close(output)
	defer resp.Body.Close()

	parser := utils.NewCursorProtocolParser(parseConfig)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	flushParser := func() {
		for _, event := range parser.Finish() {
			select {
			case output <- event:
			case <-ctx.Done():
				return
			}
		}
	}

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := scanner.Text()
		
		// 方便用于追踪 Cursor 原始底层的拦截或隐藏错误动作（例如 tool-input-error）
		if strings.HasPrefix(line, "data: ") {
			logrus.Debugf("[Raw SSE] %s", line)
		}

		data := utils.ParseSSELine(line)
		if data == "" {
			continue
		}

		if data == "[DONE]" {
			flushParser()
			return
		}

		var eventData models.CursorEventData
		if err := json.Unmarshal([]byte(data), &eventData); err != nil {
			logrus.WithError(err).Debugf("Failed to parse SSE data: %s", data)
			continue
		}

		switch eventData.Type {

        // 从拦截信息中转成OpenAI Function Calling
		case "tool-input-error":
			logrus.WithFields(logrus.Fields{
				"tool_name":    eventData.ToolName,
				"tool_call_id": eventData.ToolCallID,
			}).Info("Intercepted native tool-input-error! Converting to simulated tool_call event")

			argsStr := "{}"
			if len(eventData.Input) > 0 {
				argsStr = string(eventData.Input)
			}

			tc := models.ToolCall{
				ID:   eventData.ToolCallID,
				Type: "function",
				Function: models.FunctionCall{
					Name:      eventData.ToolName,
					Arguments: argsStr,
				},
			}

			select {
			case output <- models.AssistantEvent{
				Kind:     models.AssistantEventToolCall,
				ToolCall: &tc,
			}:
			case <-ctx.Done():
				return
			}
			continue
		case "error":
			if eventData.ErrorText != "" {
				errResp := middleware.NewCursorWebError(http.StatusBadGateway, "cursor API error: "+eventData.ErrorText)
				select {
				case output <- errResp:
				default:
					logrus.WithError(errResp).Warn("failed to push SSE error to channel")
				}
				return
			}
		case "finish":
			flushParser()
			if eventData.MessageMetadata != nil && eventData.MessageMetadata.Usage != nil {
				usage := models.Usage{
					PromptTokens:     eventData.MessageMetadata.Usage.InputTokens,
					CompletionTokens: eventData.MessageMetadata.Usage.OutputTokens,
					TotalTokens:      eventData.MessageMetadata.Usage.TotalTokens,
				}
				select {
				case output <- usage:
				case <-ctx.Done():
					return
				}
			}
			return
		default:
			if eventData.Delta == "" {
				continue
			}
			for _, event := range parser.Feed(eventData.Delta) {
				select {
				case output <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		errResp := middleware.NewCursorWebError(http.StatusBadGateway, err.Error())
		select {
		case output <- errResp:
		default:
			logrus.WithError(err).Warn("failed to push SSE error to channel")
		}
		return
	}

	flushParser()
}

func (s *CursorService) fetchXIsHuman(ctx context.Context) (string, error) {
	// 鉴于 Cursor 的脚本 URL 频繁变动且 404 时随机 Token 依然可用，
	// 直接生成随机 Token 以消除告警并提升响应速度。
	token := utils.GenerateRandomString(64)
	return token, nil
}

func (s *CursorService) prepareJS(cursorJS string) string {
	replacer := strings.NewReplacer(
		"$$currentScriptSrc$$", s.config.ScriptURL,
		"$$UNMASKED_VENDOR_WEBGL$$", s.config.FP.UNMASKED_VENDOR_WEBGL,
		"$$UNMASKED_RENDERER_WEBGL$$", s.config.FP.UNMASKED_RENDERER_WEBGL,
		"$$userAgent$$", s.config.FP.UserAgent,
	)

	mainScript := replacer.Replace(s.mainJS)
	mainScript = strings.Replace(mainScript, "$$env_jscode$$", s.envJS, 1)
	mainScript = strings.Replace(mainScript, "$$cursor_jscode$$", cursorJS, 1)
	return mainScript
}

func (s *CursorService) truncateCursorMessages(messages []models.CursorMessage) []models.CursorMessage {
	if len(messages) == 0 || s.config.MaxInputLength <= 0 {
		return messages
	}

	maxLength := s.config.MaxInputLength
	total := 0
	for _, msg := range messages {
		total += cursorMessageTextLength(msg)
	}

	if total <= maxLength {
		return messages
	}

	var result []models.CursorMessage
	startIdx := 0

	if strings.EqualFold(messages[0].Role, "system") {
		result = append(result, messages[0])
		maxLength -= cursorMessageTextLength(messages[0])
		if maxLength < 0 {
			maxLength = 0
		}
		startIdx = 1
	}

	current := 0
	collected := make([]models.CursorMessage, 0, len(messages)-startIdx)
	for i := len(messages) - 1; i >= startIdx; i-- {
		msg := messages[i]
		msgLen := cursorMessageTextLength(msg)
		if msgLen == 0 {
			continue
		}
		if current+msgLen > maxLength {
			continue
		}
		collected = append(collected, msg)
		current += msgLen
	}

	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}

	return append(result, collected...)
}

func cursorMessageTextLength(msg models.CursorMessage) int {
	total := 0
	for _, part := range msg.Parts {
		total += len(part.Text)
	}
	return total
}

func (s *CursorService) chatHeaders(xIsHuman string) map[string]string {
	return s.headerGenerator.GetChatHeaders(xIsHuman)
}

func (s *CursorService) scriptHeaders() map[string]string {
	return s.headerGenerator.GetScriptHeaders()
}
