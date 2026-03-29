package models

import (
	"encoding/json"
)

// AnthropicMessageRequest 代表来自 Claude Code 或其他 Anthropic 客户端的请求
type AnthropicMessageRequest struct {
	Model       string             `json:"model"`
	Messages    []AnthropicMessage `json:"messages"`
	System      interface{}        `json:"system,omitempty"` // 可以是 string 或 list
	MaxTokens   int                `json:"max_tokens"`
	Stream      bool               `json:"stream,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
	Tools       []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice  interface{}        `json:"tool_choice,omitempty"`
}

type AnthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string 或者是 []AnthropicContentBlock
}

type AnthropicContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string      `json:"tool_use_id,omitempty"`
	Content   interface{} `json:"content,omitempty"`
	// image
	Source *AnthropicImageSource `json:"source,omitempty"`
}

type AnthropicImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/jpeg", "image/png", etc.
	Data      string `json:"data"`       // base64 data
}

type AnthropicTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema interface{} `json:"input_schema"`
}
