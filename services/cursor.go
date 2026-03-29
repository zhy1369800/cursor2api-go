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
	client.SetTimeout(time.Duration(cfg.Timeout) * time.Second)
	client.ImpersonateChrome()
	if jar != nil {
		client.SetCookieJar(jar)
	}

	// 显式设置代理，确保在下载 SCRIPT_URL 时能穿墙
	proxyURL := os.Getenv("HTTP_PROXY")
	if proxyURL == "" {
		proxyURL = os.Getenv("http_proxy")
	}
	if proxyURL != "" {
		logrus.Infof("Using proxy for Cursor Service: %s", proxyURL)
		client.SetProxyURL(proxyURL)
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
	truncatedMessages := s.truncateMessages(request.Messages)
	cursorMessages := models.ToCursorMessages(truncatedMessages, s.config.SystemPromptInject)

	// 获取Cursor API使用的实际模型名称
	cursorModel := models.GetCursorModel(request.Model)

	payload := models.CursorRequest{
		Context:  []interface{}{},
		Model:    cursorModel,
		ID:       utils.GenerateRandomString(16),
		Messages: cursorMessages,
		Trigger:  "submit-message",
	}

	jsonPayload, err := json.Marshal(payload)
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
		go s.consumeSSE(ctx, resp.Response, output)
		return output, nil
	}

	return nil, fmt.Errorf("failed after %d attempts", maxRetries)
}

func (s *CursorService) consumeSSE(ctx context.Context, resp *http.Response, output chan interface{}) {
	defer close(output)

	if err := utils.ReadSSEStream(ctx, resp, output); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		errResp := middleware.NewCursorWebError(http.StatusBadGateway, err.Error())
		select {
		case output <- errResp:
		default:
			logrus.WithError(err).Warn("failed to push SSE error to channel")
		}
	}
}

func (s *CursorService) fetchXIsHuman(ctx context.Context) (string, error) {
	// 检查缓存
	s.scriptMutex.RLock()
	cached := s.scriptCache
	lastFetch := s.scriptCacheTime
	s.scriptMutex.RUnlock()

	var scriptBody string
	// 缓存有效期缩短到1分钟,避免 token 过期
	if cached != "" && time.Since(lastFetch) < 1*time.Minute {
		scriptBody = cached
	} else {
		resp, err := s.client.R().
			SetContext(ctx).
			SetHeaders(s.scriptHeaders()).
			Get(s.config.ScriptURL)

		if err != nil {
			// 如果请求失败且有缓存，使用缓存
			if cached != "" {
				logrus.Warnf("Failed to fetch script, using cached version: %v", err)
				scriptBody = cached
			} else {
				// 清除缓存并生成一个简单的token
				s.scriptMutex.Lock()
				s.scriptCache = ""
				s.scriptCacheTime = time.Time{}
				s.scriptMutex.Unlock()
				// 生成一个简单的x-is-human token作为fallback
				token := utils.GenerateRandomString(64)
				logrus.Warnf("Failed to fetch script, generated fallback token")
				return token, nil
			}
		} else if resp.StatusCode != http.StatusOK {
			// 如果状态码异常且有缓存，使用缓存
			if cached != "" {
				logrus.Warnf("Script fetch returned status %d, using cached version", resp.StatusCode)
				scriptBody = cached
			} else {
				// 清除缓存并生成一个简单的token
				s.scriptMutex.Lock()
				s.scriptCache = ""
				s.scriptCacheTime = time.Time{}
				s.scriptMutex.Unlock()
				// 生成一个简单的x-is-human token作为fallback
				token := utils.GenerateRandomString(64)
				logrus.Debugf("Script fetch returned status %d, generated fallback token", resp.StatusCode)
				return token, nil
			}
		} else {
			scriptBody = string(resp.Bytes())
			// 更新缓存
			s.scriptMutex.Lock()
			s.scriptCache = scriptBody
			s.scriptCacheTime = time.Now()
			s.scriptMutex.Unlock()
		}
	}

	compiled := s.prepareJS(scriptBody)
	value, err := utils.RunJS(compiled)
	if err != nil {
		// JS 执行失败时清除缓存并生成fallback token
		s.scriptMutex.Lock()
		s.scriptCache = ""
		s.scriptCacheTime = time.Time{}
		s.scriptMutex.Unlock()
		token := utils.GenerateRandomString(64)
		logrus.Warnf("Failed to execute JS, generated fallback token: %v", err)
		return token, nil
	}

	logrus.WithField("length", len(value)).Debug("Fetched x-is-human token")

func (s *CursorService) fetchXIsHuman(ctx context.Context) (string, error) {
	// 1. 检查内存缓存 (1分钟有效期)
	s.scriptMutex.RLock()
	cached := s.scriptCache
	lastFetch := s.scriptCacheTime
	s.scriptMutex.RUnlock()

	var scriptBody string
	diskCachePath := filepath.Join("jscode", "cursor_latest.js")

	if cached != "" && time.Since(lastFetch) < 1*time.Minute {
		scriptBody = cached
	} else {
		// 2. 检查硬盘缓存 (如果内存没有或者过期，优先看硬盘是否有手动存放的最新包)
		if scriptOnDisk, err := os.ReadFile(diskCachePath); err == nil && len(scriptOnDisk) > 1000 {
			scriptBody = string(scriptOnDisk)
			// 注意：硬盘缓存如果是过期的，我们依然可以用它作为保底，
			// 但我们可以尝试在后台异步更新它(本处简单处理，直接使用)
		}

		// 3. 只有硬盘也没有且内存也没有，或者为了保持最新进行网络请求
		if scriptBody == "" || time.Since(lastFetch) >= 10*time.Minute {
			resp, err := s.client.R().
				SetContext(ctx).
				SetHeaders(s.scriptHeaders()).
				Get(s.config.ScriptURL)

			if err == nil && resp.StatusCode == http.StatusOK {
				scriptBody = string(resp.Bytes())
				// 4. 成功后同步更新内存和硬盘缓存
				s.scriptMutex.Lock()
				s.scriptCache = scriptBody
				s.scriptCacheTime = time.Now()
				s.scriptMutex.Unlock()
				_ = os.WriteFile(diskCachePath, []byte(scriptBody), 0644)
			} else if scriptBody == "" {
				// 网络失败且真的没缓存了，采用最后的随机 Token 模式
				token := utils.GenerateRandomString(64)
				logrus.Warnf("All fetch attempts failed (including 500/timeout), using random fallback token")
				return token, nil
			}
		}
	}

	compiled := s.prepareJS(scriptBody)
	value, err := utils.RunJS(compiled)
	if err != nil {
		// 如果运行失败（可能是本地存的代码太老了），尝试清除硬盘缓存期待下次下载
		if errRemove := os.Remove(diskCachePath); errRemove == nil {
			logrus.Warn("Removed corrupted/outdated local JS cache")
		}
		token := utils.GenerateRandomString(64)
		logrus.Warnf("Failed to execute JS: %v, using random fallback token", err)
		return token, nil
	}

	logrus.WithFields(logrus.Fields{
		"length": len(value),
		"source": "cache/disk",
	}).Debug("Fetched x-is-human token")

	return value, nil
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

func (s *CursorService) truncateMessages(messages []models.Message) []models.Message {
	if len(messages) == 0 || s.config.MaxInputLength <= 0 {
		return messages
	}

	maxLength := s.config.MaxInputLength
	total := 0
	for _, msg := range messages {
		total += len(msg.GetStringContent())
	}

	if total <= maxLength {
		return messages
	}

	var result []models.Message
	startIdx := 0

	if strings.EqualFold(messages[0].Role, "system") {
		result = append(result, messages[0])
		maxLength -= len(messages[0].GetStringContent())
		if maxLength < 0 {
			maxLength = 0
		}
		startIdx = 1
	}

	current := 0
	collected := make([]models.Message, 0, len(messages)-startIdx)
	for i := len(messages) - 1; i >= startIdx; i-- {
		msg := messages[i]
		msgLen := len(msg.GetStringContent())
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
		if part.Type == "text" {
			total += len(part.Text)
		} else if part.Type == "image" && part.Image != nil {
			// 图片 base64 长度也要算进去，否则会因为 payload 太大被 Vercel 拒绝
			total += len(part.Image.Data)
		}
	}
	return total
}

func (s *CursorService) chatHeaders(xIsHuman string) map[string]string {
	return s.headerGenerator.GetChatHeaders(xIsHuman)
}

func (s *CursorService) scriptHeaders() map[string]string {
	return s.headerGenerator.GetScriptHeaders()
}
