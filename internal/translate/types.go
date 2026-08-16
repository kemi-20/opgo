// Package translate 在三种 API 格式之间互转（OpenAI chat/completions、
// OpenAI Responses、Anthropic messages），支持流式/非流式与思考内容。
// 以规范化中间模型为核心：解析源格式 → 规范模型 → 序列化目标格式。
package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Format 一种 API 格式。
type Format string

const (
	FormatOpenAICompletions Format = "openai_completions"
	FormatOpenAIResponses   Format = "openai_responses"
	FormatAnthropic         Format = "anthropic"
)

// ParseFormat 把 config 里的字符串解析为格式；空/假值返回 ok=false（透传）。
func ParseFormat(s string) (Format, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "openai_completions", "openai-completions", "chat_completions", "chat-completions", "completions":
		return FormatOpenAICompletions, true
	case "openai_responses", "openai-responses", "responses":
		return FormatOpenAIResponses, true
	case "anthropic", "claude", "messages":
		return FormatAnthropic, true
	}
	return "", false
}

// DetectFormat 按请求路径检测客户端使用的格式。
func DetectFormat(path string) (Format, bool) {
	p := strings.ToLower(path)
	switch {
	case strings.HasSuffix(p, "/chat/completions"):
		return FormatOpenAICompletions, true
	case strings.HasSuffix(p, "/responses"):
		return FormatOpenAIResponses, true
	case strings.HasSuffix(p, "/messages"):
		return FormatAnthropic, true
	}
	return "", false
}

// ---------- 规范化请求模型 ----------

// Block 消息内容块（文本/图片/思考/工具调用/工具结果）。
type Block struct {
	Type      string          `json:"type"` // text | image | audio | thinking | tool_use | tool_result
	Text      string          `json:"text,omitempty"`
	ImageURL  string          `json:"image_url,omitempty"`
	AudioURL  string          `json:"audio_url,omitempty"` // data:audio/...;base64,...
	Thinking  string          `json:"thinking,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// Message 规范化消息。
type Message struct {
	Role       string  `json:"role"` // system | user | assistant | tool
	Content    []Block `json:"content,omitempty"`
	Text       string  `json:"text,omitempty"`
	ToolCallID string  `json:"tool_call_id,omitempty"`
	Name       string  `json:"name,omitempty"`
}

// Tool 规范化工具声明。
type Tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// Request 规范化请求。
type Request struct {
	Model           string   `json:"model"`
	Stream          bool     `json:"stream"`
	MaxTokens       *int     `json:"max_tokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	Stop            []string `json:"stop,omitempty"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
	ThinkingEnabled bool     `json:"thinking_enabled,omitempty"`
	ThinkingBudget  int      `json:"thinking_budget,omitempty"`
	System          []Block   `json:"system,omitempty"`
	Messages        []Message `json:"messages,omitempty"`
	Tools           []Tool    `json:"tools,omitempty"`
	IncludeUsage    bool      `json:"include_usage,omitempty"`
	Instructions    string    `json:"instructions,omitempty"`
}

// ---------- 规范化响应模型 ----------

// ResponseChoice 一个响应选择/消息。
type ResponseChoice struct {
	Index        int        `json:"index"`
	Role         string     `json:"role"`
	Text         string     `json:"text,omitempty"`
	Reasoning    string     `json:"reasoning,omitempty"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	FinishReason string     `json:"finish_reason,omitempty"`
}

// ToolCall 工具调用（用于响应）。
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Usage 规范化用量。
type Usage struct {
	PromptTokens      int64 `json:"prompt_tokens"`
	CompletionTokens  int64 `json:"completion_tokens"`
	TotalTokens       int64 `json:"total_tokens"`
	CachedTokens      int64 `json:"cached_tokens"`
	CachedWriteTokens int64 `json:"cached_write_tokens"`
}

// Response 规范化响应（非流式）。
type Response struct {
	ID      string           `json:"id"`
	Model   string           `json:"model"`
	Created int64            `json:"created"`
	Choices []ResponseChoice `json:"choices"`
	Usage   Usage            `json:"usage"`
}

// ---------- 工具函数 ----------

// jsonGet 取嵌套字段的原始 JSON（点路径）。
func jsonGet(raw []byte, path string) (json.RawMessage, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	cur := v
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	b, err := json.Marshal(cur)
	if err != nil {
		return nil, false
	}
	return b, true
}

// parseTextOrBlocks 把内容解析为 Block 列表（兼容字符串与块数组）。
func parseTextOrBlocks(v any) []Block {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return []Block{{Type: "text", Text: t}}
	case []any:
		var out []Block
		for _, item := range t {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "text", "input_text", "output_text":
				text, _ := m["text"].(string)
				out = append(out, Block{Type: "text", Text: text})
			case "image_url":
				u := ""
				if iu, ok := m["image_url"].(map[string]any); ok {
					u, _ = iu["url"].(string)
				} else if s, ok := m["image_url"].(string); ok {
					u = s
				}
				out = append(out, Block{Type: "image", ImageURL: u})
			case "input_image":
				// OpenAI Responses: image_url 是字符串；OpenAI chat 是 {url:...}
				u := ""
				if s, ok := m["image_url"].(string); ok {
					u = s
				} else if iu, ok := m["image_url"].(map[string]any); ok {
					u, _ = iu["url"].(string)
				}
				out = append(out, Block{Type: "image", ImageURL: u})
			case "input_audio", "audio":
				// OpenAI chat/responses: {"input_audio":{"data":"...","format":"wav"}}
				// Anthropic: {"source":{"type":"base64","media_type":"audio/wav","data":"..."}}
				u := ""
				if ia, ok := m["input_audio"].(map[string]any); ok {
					if data, ok := ia["data"].(string); ok && data != "" {
						format, _ := ia["format"].(string)
						if format == "" {
							format = "wav"
						}
						u = "data:audio/" + format + ";base64," + data
					}
				}
				if u == "" {
					if src, ok := m["source"].(map[string]any); ok {
						if st, _ := src["type"].(string); st == "base64" {
							md, _ := src["media_type"].(string)
							data, _ := src["data"].(string)
							if md != "" && data != "" {
								u = "data:" + md + ";base64," + data
							}
						}
					}
				}
				if u != "" {
					out = append(out, Block{Type: "audio", AudioURL: u})
				}
			case "thinking", "reasoning":
				text, _ := m["thinking"].(string)
				if text == "" {
					text, _ = m["text"].(string)
				}
				out = append(out, Block{Type: "thinking", Thinking: text})
			case "tool_use":
				id, _ := m["id"].(string)
				name, _ := m["name"].(string)
				input, _ := json.Marshal(m["input"])
				out = append(out, Block{Type: "tool_use", ToolUseID: id, Name: name, Input: input})
			case "tool_result":
				id, _ := m["tool_use_id"].(string)
				content, _ := json.Marshal(m["content"])
				out = append(out, Block{Type: "tool_result", ToolUseID: id, Content: content})
			case "function_call":
				// OpenAI Responses 工具调用
				id, _ := m["call_id"].(string)
				if id == "" {
					id, _ = m["id"].(string)
				}
				name, _ := m["name"].(string)
				args, _ := json.Marshal(m["arguments"])
				out = append(out, Block{Type: "tool_use", ToolUseID: id, Name: name, Input: args})
			case "function_call_output":
				// OpenAI Responses 工具结果
				id, _ := m["call_id"].(string)
				content, _ := json.Marshal(m["output"])
				out = append(out, Block{Type: "tool_result", ToolUseID: id, Content: content})
			case "refusal":
				text, _ := m["refusal"].(string)
				out = append(out, Block{Type: "text", Text: text})
			}
		}
		return out
	}
	return nil
}

// errf 便捷错误。
func errf(format string, args ...any) error { return fmt.Errorf(format, args...) }
