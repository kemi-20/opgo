package translate

import (
	"encoding/json"
	"strings"
)

// ---------- 序列化到 OpenAI chat/completions ----------

func buildOpenAICompletionsRequest(req *Request) ([]byte, error) {
	out := map[string]any{
		"model": req.Model,
		"stream": req.Stream,
	}
	if req.MaxTokens != nil {
		out["max_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		if len(req.Stop) == 1 {
			out["stop"] = req.Stop[0]
		} else {
			out["stop"] = req.Stop
		}
	}
	if req.ReasoningEffort != "" {
		out["reasoning_effort"] = req.ReasoningEffort
	}
	// messages
	msgs := make([]map[string]any, 0, len(req.Messages)+1)
	if len(req.System) > 0 {
		msgs = append(msgs, map[string]any{"role": "system", "content": blocksToOpenAIContent(req.System)})
	}
	for _, m := range req.Messages {
		mm := map[string]any{"role": m.Role}
		switch m.Role {
		case "tool":
			mm["tool_call_id"] = m.ToolCallID
			if len(m.Content) > 0 {
				mm["content"] = blocksToOpenAIContent(m.Content)
			} else {
				mm["content"] = m.Text
			}
		case "assistant":
			var textParts []Block
			var thinking string
			var toolCalls []map[string]any
			for _, b := range m.Content {
				switch b.Type {
				case "thinking":
					thinking += b.Thinking
				case "tool_use":
					var args string
					if len(b.Input) > 0 {
						args = string(b.Input)
					} else {
						args = "{}"
					}
					toolCalls = append(toolCalls, map[string]any{
						"id":   b.ToolUseID,
						"type": "function",
						"function": map[string]any{"name": b.Name, "arguments": args},
					})
				default:
					textParts = append(textParts, b)
				}
			}
			if len(m.Content) == 0 && m.Text != "" {
				textParts = append([]Block{{Type: "text", Text: m.Text}}, textParts...)
			}
			if len(textParts) > 0 {
				mm["content"] = blocksToOpenAIContent(textParts)
			} else {
				mm["content"] = ""
			}
			if thinking != "" {
				mm["reasoning_content"] = thinking
			}
			if len(toolCalls) > 0 {
				mm["tool_calls"] = toolCalls
			}
		default:
			if len(m.Content) > 0 {
				mm["content"] = blocksToOpenAIContent(m.Content)
			} else {
				mm["content"] = m.Text
			}
		}
		msgs = append(msgs, mm)
	}
	out["messages"] = msgs
	// tools
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type == "function" {
				fn := map[string]any{"name": t.Name}
				if t.Description != "" {
					fn["description"] = t.Description
				}
				if len(t.Parameters) > 0 {
					fn["parameters"] = json.RawMessage(t.Parameters)
				}
				tools = append(tools, map[string]any{"type": "function", "function": fn})
			}
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	if req.Stream && req.IncludeUsage {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(out)
}

func blocksToOpenAIContent(blocks []Block) any {
	if len(blocks) == 0 {
		return ""
	}
	// 全文本 → 直接字符串
	allText := true
	for _, b := range blocks {
		if b.Type != "text" {
			allText = false
			break
		}
	}
	if allText {
		var s string
		for _, b := range blocks {
			s += b.Text
		}
		return s
	}
	arr := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			arr = append(arr, map[string]any{"type": "text", "text": b.Text})
		case "image":
			arr = append(arr, map[string]any{"type": "image_url", "image_url": map[string]any{"url": b.ImageURL}})
		case "audio":
			_, data, format := audioParts(b.AudioURL)
			if data != "" {
				arr = append(arr, map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": data, "format": format}})
			}
		case "tool_result":
			arr = append(arr, map[string]any{"type": "text", "text": rawToText(b.Content)})
		}
	}
	return arr
}

// audioParts 从 data URL 拆出 media_type / data / format（如 wav、mp3）。
func audioParts(url string) (mediaType, data, format string) {
	mediaType, data = splitDataURL(url)
	if data == "" {
		return mediaType, "", ""
	}
	format = strings.TrimPrefix(mediaType, "audio/")
	if format == "" {
		format = "wav"
	}
	return mediaType, data, format
}

func rawToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return string(raw)
}

// ---------- 序列化到 OpenAI Responses ----------

func buildOpenAIResponsesRequest(req *Request) ([]byte, error) {
	out := map[string]any{
		"model":  req.Model,
		"stream": req.Stream,
	}
	if req.MaxTokens != nil {
		out["max_output_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.ReasoningEffort != "" {
		out["reasoning"] = map[string]any{"effort": req.ReasoningEffort}
	}
	if req.Instructions != "" {
		out["instructions"] = req.Instructions
	}
	if req.IncludeUsage {
		out["include"] = []string{"usage"}
	}
	// input
	input := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		mm := map[string]any{"role": m.Role}
		content := make([]map[string]any, 0, len(m.Content)+1)
		if m.Text != "" {
			content = append(content, map[string]any{"type": "input_text", "text": m.Text})
		}
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				content = append(content, map[string]any{"type": "input_text", "text": b.Text})
			case "thinking":
				content = append(content, map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": b.Thinking}}})
			case "image":
				content = append(content, map[string]any{"type": "input_image", "image_url": b.ImageURL})
			case "audio":
				_, data, format := audioParts(b.AudioURL)
				if data != "" {
					content = append(content, map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": data, "format": format}})
				}
			case "tool_use":
				var inputVal any = map[string]any{}
				if len(b.Input) > 0 {
					_ = json.Unmarshal(b.Input, &inputVal)
				}
				content = append(content, map[string]any{"type": "function_call", "call_id": b.ToolUseID, "name": b.Name, "arguments": inputVal})
			case "tool_result":
				content = append(content, map[string]any{"type": "function_call_output", "call_id": b.ToolUseID, "output": rawToText(b.Content)})
			}
		}
		if len(content) > 0 {
			mm["content"] = content
		}
		input = append(input, mm)
	}
	out["input"] = input
	// tools
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			switch t.Type {
			case "function":
				tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": json.RawMessage(t.Parameters)})
			case "web_search", "web_search_preview":
				tools = append(tools, map[string]any{"type": t.Type})
			}
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	return json.Marshal(out)
}

// ---------- 序列化到 Anthropic ----------

func buildAnthropicRequest(req *Request) ([]byte, error) {
	out := map[string]any{
		"model":  req.Model,
		"stream": req.Stream,
	}
	if req.MaxTokens != nil {
		out["max_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		out["stop_sequences"] = req.Stop
	}
	if req.ThinkingEnabled {
		budget := req.ThinkingBudget
		if budget <= 0 {
			budget = 8000
		}
		out["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
	} else if req.ReasoningEffort != "" {
		// openai effort → anthropic thinking（低/中→enabled 小预算，高→大预算）
		budget := 8000
		switch req.ReasoningEffort {
		case "high":
			budget = 32000
		case "medium":
			budget = 8000
		default:
			budget = 2000
		}
		out["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
	}
	// system
	if len(req.System) > 0 {
		if len(req.System) == 1 && req.System[0].Type == "text" {
			out["system"] = req.System[0].Text
		} else {
			blocks := make([]map[string]any, 0, len(req.System))
			for _, b := range req.System {
				if b.Type == "text" {
					blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
				}
			}
			out["system"] = blocks
		}
	}
	// messages
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		mm := map[string]any{"role": m.Role}
		if m.Role == "tool" {
			mm["content"] = []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     rawToText(m.Content[0].Content),
			}}
			msgs = append(msgs, mm)
			continue
		}
		content := make([]map[string]any, 0, len(m.Content)+1)
		hasText := false
		if m.Text != "" {
			content = append(content, map[string]any{"type": "text", "text": m.Text})
			hasText = true
		}
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				if hasText {
					continue // 已通过 m.Text 输出，避免重复
				}
				content = append(content, map[string]any{"type": "text", "text": b.Text})
			case "thinking":
				content = append(content, map[string]any{"type": "thinking", "thinking": b.Thinking})
			case "image":
				mediaType, data := splitDataURL(b.ImageURL)
				if data != "" {
					content = append(content, map[string]any{
						"type": "image",
						"source": map[string]any{"type": "base64", "media_type": mediaType, "data": data},
					})
				}
			case "audio":
				mediaType, data, _ := audioParts(b.AudioURL)
				if data != "" {
					content = append(content, map[string]any{
						"type": "audio",
						"source": map[string]any{"type": "base64", "media_type": mediaType, "data": data},
					})
				}
			case "tool_use":
				var inputVal any = map[string]any{}
				if len(b.Input) > 0 {
					_ = json.Unmarshal(b.Input, &inputVal)
				}
				content = append(content, map[string]any{"type": "tool_use", "id": b.ToolUseID, "name": b.Name, "input": inputVal})
			case "tool_result":
				content = append(content, map[string]any{"type": "tool_result", "tool_use_id": b.ToolUseID, "content": rawToText(b.Content)})
			}
		}
		if len(content) > 0 {
			mm["content"] = content
		} else {
			mm["content"] = m.Text
		}
		msgs = append(msgs, mm)
	}
	out["messages"] = msgs
	// tools
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type == "function" {
				tools = append(tools, map[string]any{
					"name":         t.Name,
					"description":  t.Description,
					"input_schema": json.RawMessage(t.Parameters),
				})
			}
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	return json.Marshal(out)
}

func splitDataURL(url string) (mediaType, data string) {
	const prefix = "data:"
	if len(url) < len(prefix) || url[:len(prefix)] != prefix {
		return "image/png", url
	}
	rest := url[len(prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == ';' {
			mt := rest[:i]
			if i+7 < len(rest) && rest[i:i+8] == ";base64," {
				return mt, rest[i+8:]
			}
			break
		}
	}
	return "image/png", rest
}
