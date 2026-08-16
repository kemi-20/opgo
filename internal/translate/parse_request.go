package translate

import (
	"encoding/json"
)

// ---------- OpenAI chat/completions 请求解析 ----------

func parseOpenAICompletionsRequest(raw []byte) (*Request, error) {
	var obj struct {
		Model         string           `json:"model"`
		Stream        bool             `json:"stream"`
		MaxTokens     *int             `json:"max_tokens"`
		MaxCompletion *int             `json:"max_completion_tokens"`
		Temperature   *float64         `json:"temperature"`
		TopP          *float64         `json:"top_p"`
		Stop          json.RawMessage  `json:"stop"`
		Messages      []map[string]any `json:"messages"`
		Tools         []struct {
			Type     string `json:"type"`
			Function *struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		StreamOptions *struct {
			IncludeUsage *bool `json:"include_usage"`
		} `json:"stream_options"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errf("解析 openai_completions 请求失败: %w", err)
	}
	req := &Request{
		Model:           obj.Model,
		Stream:          obj.Stream,
		MaxTokens:       obj.MaxTokens,
		Temperature:     obj.Temperature,
		TopP:            obj.TopP,
		ReasoningEffort: obj.ReasoningEffort,
	}
	if req.MaxTokens == nil {
		req.MaxTokens = obj.MaxCompletion
	}
	if len(obj.Stop) > 0 {
		var stops []string
		if json.Unmarshal(obj.Stop, &stops) != nil {
			var s string
			if json.Unmarshal(obj.Stop, &s) == nil && s != "" {
				stops = []string{s}
			}
		}
		req.Stop = stops
	}
	if obj.StreamOptions != nil && obj.StreamOptions.IncludeUsage != nil && *obj.StreamOptions.IncludeUsage {
		req.IncludeUsage = true
	}
	for _, m := range obj.Messages {
		role, _ := m["role"].(string)
		req.Messages = append(req.Messages, rebuildOpenAIMessage(role, m))
	}
	req.Messages, req.System = extractSystem(req.Messages)
	for _, t := range obj.Tools {
		if t.Type == "function" && t.Function != nil {
			req.Tools = append(req.Tools, Tool{
				Type:        "function",
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			})
		}
	}
	return req, nil
}

func rebuildOpenAIMessage(role string, m map[string]any) Message {
	msg := Message{Role: role}
	if name, ok := m["name"].(string); ok {
		msg.Name = name
	}
	if role == "tool" {
		if id, ok := m["tool_call_id"].(string); ok {
			msg.ToolCallID = id
		}
	}
	if c, ok := m["content"]; ok {
		msg.Content = parseTextOrBlocks(c)
	}
	if role == "assistant" {
		if rc, ok := m["reasoning_content"].(string); ok && rc != "" {
			msg.Content = append([]Block{{Type: "thinking", Thinking: rc}}, msg.Content...)
		}
		if tcs, ok := m["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				tcm, _ := tc.(map[string]any)
				id, _ := tcm["id"].(string)
				fn, _ := tcm["function"].(map[string]any)
				name, _ := fn["name"].(string)
				args, _ := json.Marshal(fn["arguments"])
				msg.Content = append(msg.Content, Block{Type: "tool_use", ToolUseID: id, Name: name, Input: args})
			}
		}
	}
	if msg.Content == nil {
		if s, ok := m["content"].(string); ok {
			msg.Text = s
		}
	} else {
		msg.Text = blocksText(msg.Content)
	}
	return msg
}

func blocksText(blocks []Block) string {
	var sb []byte
	for _, b := range blocks {
		if b.Type == "text" {
			sb = append(sb, b.Text...)
		}
	}
	return string(sb)
}

func extractSystem(msgs []Message) ([]Message, []Block) {
	var out []Message
	var sys []Block
	for _, m := range msgs {
		if m.Role == "system" {
			if m.Text != "" {
				sys = append(sys, Block{Type: "text", Text: m.Text})
			} else {
				for _, b := range m.Content {
					if b.Type == "text" {
						sys = append(sys, b)
					}
				}
			}
			continue
		}
		out = append(out, m)
	}
	return out, sys
}

// ---------- OpenAI Responses 请求解析 ----------

func parseOpenAIResponsesRequest(raw []byte) (*Request, error) {
	var obj struct {
		Model       string          `json:"model"`
		Stream      bool            `json:"stream"`
		MaxOutput   *int            `json:"max_output_tokens"`
		Temperature *float64        `json:"temperature"`
		TopP        *float64        `json:"top_p"`
		Input       json.RawMessage `json:"input"`
		Tools       []struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"tools"`
		Reasoning *struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Instructions     string          `json:"instructions"`
		ToolChoice       json.RawMessage `json:"tool_choice"`
		ParallelToolCalls *bool          `json:"parallel_tool_calls"`
		Include          []string        `json:"include"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errf("解析 openai_responses 请求失败: %w", err)
	}
	req := &Request{
		Model:       obj.Model,
		Stream:      obj.Stream,
		MaxTokens:   obj.MaxOutput,
		Temperature: obj.Temperature,
		TopP:        obj.TopP,
	}
	if obj.Reasoning != nil {
		req.ReasoningEffort = obj.Reasoning.Effort
	}
	if len(obj.ToolChoice) > 0 && string(obj.ToolChoice) != "null" {
		req.ToolChoice = json.RawMessage(obj.ToolChoice)
	}
	req.ParallelToolCalls = obj.ParallelToolCalls
	for _, i := range obj.Include {
		if i == "usage" {
			req.IncludeUsage = true
		}
	}
	if obj.Instructions != "" {
		req.System = append(req.System, Block{Type: "text", Text: obj.Instructions})
	}
	var input any
	_ = json.Unmarshal(obj.Input, &input)
	switch t := input.(type) {
	case string:
		if t != "" {
			req.Messages = append(req.Messages, Message{Role: "user", Text: t})
		}
	case []any:
		for _, item := range t {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			if role == "system" || role == "developer" {
				if c, ok := m["content"]; ok {
					req.System = append(req.System, parseTextOrBlocks(c)...)
				}
				continue
			}
			// OpenAI Responses 会话历史允许顶层裸项（无 role）：
			// {"type":"function_call","call_id":...,"name":...,"arguments":...} 与
			// {"type":"function_call_output","call_id":...,"output":...}。
			// 必须转成 assistant(tool_use) / tool(tool_result) 消息，否则工具历史丢失，
			// 上游会重复调用同一个工具（表现为"只调工具不输出/死循环"）。
			if role == "" {
				switch m["type"] {
				case "function_call":
					callID, _ := m["call_id"].(string)
					if callID == "" {
						callID, _ = m["id"].(string)
					}
					name, _ := m["name"].(string)
					var argsRaw json.RawMessage
					if s, ok := m["arguments"].(string); ok {
						// arguments 是 JSON 字符串字面量（如 {"command":"echo hello"}），
						// 直接作为原始字节，避免外层再加引号。
						argsRaw = json.RawMessage(s)
					} else {
						argsRaw, _ = json.Marshal(m["arguments"])
					}
					block := Block{Type: "tool_use", ToolUseID: callID, Name: name, Input: argsRaw}
					// 连续的工具调用合并到同一条 assistant 消息（并行调用）。
					if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == "assistant" && allToolUse(req.Messages[n-1].Content) {
						req.Messages[n-1].Content = append(req.Messages[n-1].Content, block)
					} else {
						req.Messages = append(req.Messages, Message{Role: "assistant", Content: []Block{block}})
					}
				case "function_call_output":
					callID, _ := m["call_id"].(string)
					output := ""
					if s, ok := m["output"].(string); ok {
						output = s
					} else {
						b, _ := json.Marshal(m["output"])
						output = string(b)
					}
					req.Messages = append(req.Messages, Message{Role: "tool", ToolCallID: callID, Text: output})
				}
				continue
			}
			msg := Message{Role: role}
			if c, ok := m["content"]; ok {
				msg.Content = parseTextOrBlocks(c)
			}
			msg.Text = blocksText(msg.Content)
			req.Messages = append(req.Messages, msg)
		}
	}
	for _, t := range obj.Tools {
		if t.Type == "function" || t.Type == "web_search" || t.Type == "web_search_preview" {
			req.Tools = append(req.Tools, Tool{Type: t.Type, Name: t.Name, Description: t.Description, Parameters: t.Parameters})
		}
	}
	return req, nil
}

// allToolUse 判断消息内容是否全部为工具调用块（用于合并连续 function_call 裸项）。
func allToolUse(blocks []Block) bool {
	if len(blocks) == 0 {
		return false
	}
	for _, b := range blocks {
		if b.Type != "tool_use" {
			return false
		}
	}
	return true
}

// ---------- Anthropic 请求解析 ----------

func parseAnthropicRequest(raw []byte) (*Request, error) {
	var obj struct {
		Model       string          `json:"model"`
		Stream      bool            `json:"stream"`
		MaxTokens   *int            `json:"max_tokens"`
		Temperature *float64        `json:"temperature"`
		TopP        *float64        `json:"top_p"`
		Stop        []string        `json:"stop_sequences"`
		System      json.RawMessage `json:"system"`
		Messages    []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		Thinking *struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errf("解析 anthropic 请求失败: %w", err)
	}
	req := &Request{
		Model:       obj.Model,
		Stream:      obj.Stream,
		MaxTokens:   obj.MaxTokens,
		Temperature: obj.Temperature,
		TopP:        obj.TopP,
		Stop:        obj.Stop,
	}
	if obj.Thinking != nil {
		switch obj.Thinking.Type {
		case "enabled", "adaptive":
			req.ThinkingEnabled = true
			req.ThinkingBudget = obj.Thinking.BudgetTokens
			if obj.Thinking.BudgetTokens >= 30000 {
				req.ReasoningEffort = "high"
			} else if obj.Thinking.BudgetTokens >= 8000 {
				req.ReasoningEffort = "medium"
			} else {
				req.ReasoningEffort = "low"
			}
		}
	}
	if len(obj.System) > 0 {
		req.System = parseTextOrBlocks(any(obj.System))
		if req.System == nil {
			var s string
			if json.Unmarshal(obj.System, &s) == nil && s != "" {
				req.System = []Block{{Type: "text", Text: s}}
			}
		}
	}
	for _, m := range obj.Messages {
		msg := Message{Role: m.Role}
		if m.Role == "assistant" || m.Role == "user" {
			var blocks []Block
			if len(m.Content) > 0 && m.Content[0] == '"' {
				var s string
				if json.Unmarshal(m.Content, &s) == nil {
					msg.Text = s
				}
			} else {
				var arr []map[string]any
				if err := json.Unmarshal(m.Content, &arr); err == nil {
					for _, b := range arr {
						blocks = append(blocks, parseAnthropicBlock(b))
					}
				}
				msg.Content = blocks
				msg.Text = blocksText(blocks)
			}
		}
		req.Messages = append(req.Messages, msg)
	}
	for _, t := range obj.Tools {
		req.Tools = append(req.Tools, Tool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.InputSchema})
	}
	return req, nil
}

func parseAnthropicBlock(b map[string]any) Block {
	typ, _ := b["type"].(string)
	switch typ {
	case "text":
		text, _ := b["text"].(string)
		return Block{Type: "text", Text: text}
	case "thinking":
		text, _ := b["thinking"].(string)
		return Block{Type: "thinking", Thinking: text}
	case "image":
		src, _ := b["source"].(map[string]any)
		if src != nil {
			if st, _ := src["type"].(string); st == "base64" {
				if md, _ := src["media_type"].(string); md != "" {
					if data, _ := src["data"].(string); data != "" {
						return Block{Type: "image", ImageURL: "data:" + md + ";base64," + data}
					}
				}
			}
		}
	case "audio":
		src, _ := b["source"].(map[string]any)
		if src != nil {
			if st, _ := src["type"].(string); st == "base64" {
				if md, _ := src["media_type"].(string); md != "" {
					if data, _ := src["data"].(string); data != "" {
						return Block{Type: "audio", AudioURL: "data:" + md + ";base64," + data}
					}
				}
			}
		}
	case "tool_use":
		id, _ := b["id"].(string)
		name, _ := b["name"].(string)
		input, _ := json.Marshal(b["input"])
		return Block{Type: "tool_use", ToolUseID: id, Name: name, Input: input}
	case "tool_result":
		id, _ := b["tool_use_id"].(string)
		content, _ := json.Marshal(b["content"])
		return Block{Type: "tool_result", ToolUseID: id, Content: content}
	}
	return Block{Type: "text"}
}
