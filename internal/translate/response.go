package translate

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------- OpenAI chat/completions 非流式响应解析 ----------

func parseOpenAICompletionsResponse(raw []byte) (*Response, error) {
	var obj struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Choices []struct {
			Index   int `json:"index"`
			Message *struct {
				Role             string `json:"role"`
				Content          any    `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			TotalTokens         int64 `json:"total_tokens"`
			PromptTokensDetails *struct {
				CachedTokens     int64 `json:"cached_tokens"`
				CacheWriteTokens int64 `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errf("解析 openai_completions 响应失败: %w", err)
	}
	resp := &Response{ID: obj.ID, Model: obj.Model, Created: obj.Created}
	for _, c := range obj.Choices {
		rc := ResponseChoice{Index: c.Index, FinishReason: c.FinishReason}
		if c.Message != nil {
			rc.Role = c.Message.Role
			rc.Text = textFromOpenAIContent(c.Message.Content)
			if c.Message.ReasoningContent != "" {
				rc.Reasoning = c.Message.ReasoningContent
			} else {
				rc.Reasoning = c.Message.Reasoning
			}
			for _, tc := range c.Message.ToolCalls {
				rc.ToolCalls = append(rc.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
			}
		}
		resp.Choices = append(resp.Choices, rc)
	}
	if obj.Usage != nil {
		resp.Usage.PromptTokens = obj.Usage.PromptTokens
		resp.Usage.CompletionTokens = obj.Usage.CompletionTokens
		resp.Usage.TotalTokens = obj.Usage.TotalTokens
		if obj.Usage.PromptTokensDetails != nil {
			resp.Usage.CachedTokens = obj.Usage.PromptTokensDetails.CachedTokens
			resp.Usage.CachedWriteTokens = obj.Usage.PromptTokensDetails.CacheWriteTokens
		}
		if obj.Usage.CompletionTokensDetails != nil {
			resp.Usage.ReasoningTokens = obj.Usage.CompletionTokensDetails.ReasoningTokens
		}
	}
	return resp, nil
}

func textFromOpenAIContent(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				if typ, _ := m["type"].(string); typ == "text" {
					if s, ok := m["text"].(string); ok {
						sb.WriteString(s)
					}
				}
			}
		}
		return sb.String()
	}
	return ""
}

// ---------- OpenAI Responses 非流式响应解析 ----------

func parseOpenAIResponsesResponse(raw []byte) (*Response, error) {
	var obj struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Created int64  `json:"created_at"`
		Status  string `json:"status"`
		Output  []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Annotations []any  `json:"annotations"`
			} `json:"content"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"output"`
		Usage *struct {
			InputTokens        int64 `json:"input_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
			TotalTokens        int64 `json:"total_tokens"`
			InputTokensDetails *struct {
				CachedTokens     int64 `json:"cached_tokens"`
				CacheWriteTokens int64 `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errf("解析 openai_responses 响应失败: %w", err)
	}
	resp := &Response{ID: obj.ID, Model: obj.Model, Created: obj.Created}
	var rc ResponseChoice
	var reasoning strings.Builder
	var text strings.Builder
	var toolCalls []ToolCall
	for _, o := range obj.Output {
		switch o.Type {
		case "reasoning":
			for _, c := range o.Content {
				// 原生 responses 的 reasoning item content 是 reasoning_text；
				// 部分实现（含上游回显）用 summary_text，两者都读。
				if c.Type == "summary_text" || c.Type == "reasoning_text" {
					reasoning.WriteString(c.Text)
				}
			}
		case "message":
			rc.Role = "assistant"
			for _, c := range o.Content {
				if c.Type == "output_text" {
					text.WriteString(c.Text)
				}
			}
		case "function_call":
			args := ""
			if len(o.Arguments) > 0 {
				if o.Arguments[0] == '"' {
					var s string
					if json.Unmarshal(o.Arguments, &s) == nil {
						args = s
					}
				} else {
					args = string(o.Arguments)
				}
			}
			toolCalls = append(toolCalls, ToolCall{ID: o.CallID, Name: o.Name, Arguments: args})
		}
	}
	rc.Reasoning = reasoning.String()
	rc.Text = text.String()
	rc.ToolCalls = toolCalls
	if len(toolCalls) > 0 && obj.Status == "completed" {
		rc.FinishReason = "tool_calls"
	} else {
		rc.FinishReason = responsesStatusToFinish(obj.Status)
	}
	resp.Choices = append(resp.Choices, rc)
	if obj.Usage != nil {
		resp.Usage.PromptTokens = obj.Usage.InputTokens
		resp.Usage.CompletionTokens = obj.Usage.OutputTokens
		resp.Usage.TotalTokens = obj.Usage.TotalTokens
		if obj.Usage.InputTokensDetails != nil {
			resp.Usage.CachedTokens = obj.Usage.InputTokensDetails.CachedTokens
			resp.Usage.CachedWriteTokens = obj.Usage.InputTokensDetails.CacheWriteTokens
		}
		if obj.Usage.OutputTokensDetails != nil {
			resp.Usage.ReasoningTokens = obj.Usage.OutputTokensDetails.ReasoningTokens
		}
	}
	return resp, nil
}

func responsesStatusToFinish(status string) string {
	switch status {
	case "completed":
		return "stop"
	case "incomplete":
		return "length"
	case "failed":
		return "content_filter"
	}
	return status
}

// ---------- Anthropic 非流式响应解析 ----------

func parseAnthropicResponse(raw []byte) (*Response, error) {
	var obj struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		Usage *struct {
			InputTokens   int64 `json:"input_tokens"`
			OutputTokens  int64 `json:"output_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errf("解析 anthropic 响应失败: %w", err)
	}
	resp := &Response{ID: obj.ID, Model: obj.Model}
	rc := ResponseChoice{Role: "assistant", FinishReason: mapAnthropicStopToOpenAI(obj.StopReason)}
	var reasoning strings.Builder
	var text strings.Builder
	for _, c := range obj.Content {
		switch c.Type {
		case "thinking":
			reasoning.WriteString(c.Thinking)
		case "text":
			text.WriteString(c.Text)
		case "tool_use":
			args := ""
			if len(c.Input) > 0 {
				args = string(c.Input)
			}
			rc.ToolCalls = append(rc.ToolCalls, ToolCall{ID: c.ID, Name: c.Name, Arguments: args})
		}
	}
	rc.Reasoning = reasoning.String()
	rc.Text = text.String()
	resp.Choices = append(resp.Choices, rc)
	if obj.Usage != nil {
		resp.Usage.PromptTokens = obj.Usage.InputTokens
		resp.Usage.CompletionTokens = obj.Usage.OutputTokens
		resp.Usage.TotalTokens = obj.Usage.InputTokens + obj.Usage.OutputTokens
		resp.Usage.CachedTokens = obj.Usage.CacheRead
		resp.Usage.CachedWriteTokens = obj.Usage.CacheCreation
	}
	return resp, nil
}

func mapAnthropicStopToOpenAI(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	}
	return reason
}

// ---------- 响应构建 ----------

func buildOpenAICompletionsResponse(resp *Response) ([]byte, error) {
	choices := make([]map[string]any, 0, len(resp.Choices))
	for _, c := range resp.Choices {
		msg := map[string]any{"role": orDefault(c.Role, "assistant")}
		if c.Reasoning != "" {
			msg["reasoning_content"] = c.Reasoning
		}
		if len(c.ToolCalls) > 0 {
			msg["content"] = ""
			tcs := make([]map[string]any, 0, len(c.ToolCalls))
			for _, tc := range c.ToolCalls {
				tcs = append(tcs, map[string]any{
					"id": tc.ID, "type": "function",
					"function": map[string]any{"name": tc.Name, "arguments": tc.Arguments},
				})
			}
			msg["tool_calls"] = tcs
		} else {
			msg["content"] = c.Text
		}
		choices = append(choices, map[string]any{"index": c.Index, "message": msg, "finish_reason": c.FinishReason})
	}
	out := map[string]any{
		"id": orDefault(resp.ID, "chatcmpl-opgo"), "object": "chat.completion",
		"model": resp.Model, "choices": choices,
	}
	hasUsage := resp.Usage.TotalTokens > 0 || resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0
	if hasUsage {
		u := map[string]any{
			"prompt_tokens":     resp.Usage.PromptTokens,
			"completion_tokens": resp.Usage.CompletionTokens,
			"total_tokens":      resp.Usage.TotalTokens,
		}
		if resp.Usage.CachedTokens > 0 || resp.Usage.CachedWriteTokens > 0 {
			u["prompt_tokens_details"] = map[string]any{
				"cached_tokens":      resp.Usage.CachedTokens,
				"cache_write_tokens": resp.Usage.CachedWriteTokens,
			}
		}
		if resp.Usage.ReasoningTokens > 0 {
			u["completion_tokens_details"] = map[string]any{"reasoning_tokens": resp.Usage.ReasoningTokens}
		}
		out["usage"] = u
	}
	return json.Marshal(out)
}

func buildOpenAIResponsesResponse(resp *Response) ([]byte, error) {
	return buildOpenAIResponsesResponseMeta(resp, nil)
}

// responsesNonStreamResponse 非流式 Responses 响应对象：字段顺序与上游原生一致。
type responsesNonStreamResponse struct {
	ID                   string `json:"id"`
	Object               string `json:"object"`
	CreatedAt            int64  `json:"created_at"`
	CompletedAt          int64  `json:"completed_at"`
	Status               string `json:"status"`
	ParallelToolCalls    bool   `json:"parallel_tool_calls"`
	Temperature          any    `json:"temperature"`
	TopP                 any    `json:"top_p"`
	MaxOutputTokens      any    `json:"max_output_tokens"`
	PreviousResponseID   any    `json:"previous_response_id"`
	Background           bool   `json:"background"`
	Truncation           string `json:"truncation"`
	TopLogprobs          int    `json:"top_logprobs"`
	MaxToolCalls         any    `json:"max_tool_calls"`
	PromptCacheRetention any    `json:"prompt_cache_retention"`
	Model                string `json:"model"`
	Error                any    `json:"error"`
	IncompleteDetails    any    `json:"incomplete_details"`
	Output               []any  `json:"output"`
	Usage                any    `json:"usage"`
	Instructions         any    `json:"instructions"`
	ToolChoice           any    `json:"tool_choice"`
	Tools                []any  `json:"tools"`
	Reasoning            any    `json:"reasoning"`
	Text                 any    `json:"text"`
	Moderation           any    `json:"moderation"`
	Cost                 string `json:"cost"`
}

func buildOpenAIResponsesResponseMeta(resp *Response, meta *Request) ([]byte, error) {
	responseID := newResponsesID("resp")
	output := make([]any, 0, 4)
	// reasoning（对齐原生：content 为 reasoning_text，summary 为空数组）
	if len(resp.Choices) > 0 && resp.Choices[0].Reasoning != "" {
		output = append(output, responsesReasoningItem{
			ID: newResponsesID("rs"), Type: "reasoning", Status: "completed",
			Content: []responsesReasoningContentPart{{Type: "reasoning_text", Text: resp.Choices[0].Reasoning}},
			Summary: []any{},
		})
	}
	for _, c := range resp.Choices {
		if c.Text != "" || len(c.ToolCalls) == 0 {
			output = append(output, responsesMessageItem{
				ID: newResponsesID("msg"), Type: "message", Status: "completed", Role: "assistant", Phase: "final_answer",
				Content: []responsesMessageContentPart{{Type: "output_text", Text: c.Text, Annotations: []any{}, Logprobs: []any{}}},
			})
		}
		for _, tc := range c.ToolCalls {
			argsStr := tc.Arguments
			if argsStr == "" {
				argsStr = "{}"
			}
			// item id 用独立 UUID，call_id 保留上游工具调用 ID（对齐原生 responses）
			output = append(output, responsesFunctionCallItem{
				ID: newResponsesID("fc"), Type: "function_call", Status: "completed",
				Name: tc.Name, CallID: tc.ID, Arguments: argsStr,
			})
		}
	}
	// 与原生一致：usage 的 input/output 详情对象始终存在（无则 0）
	hasUsage := resp.Usage.TotalTokens > 0 || resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0
	var usage any
	if hasUsage {
		usage = responsesUsageObj{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
			TotalTokens:  resp.Usage.TotalTokens,
			InputTokensDetails: responsesInputTokensDetails{
				CachedTokens:     resp.Usage.CachedTokens,
				CacheWriteTokens: resp.Usage.CachedWriteTokens,
			},
			OutputTokensDetails: responsesOutputTokensDetails{ReasoningTokens: resp.Usage.ReasoningTokens},
		}
	}
	temp := any(1)
	topp := any(1)
	var maxOut any
	toolChoice := any("auto")
	parallel := true
	instructions := any(nil)
	reasoning := any(map[string]any{"effort": nil, "summary": nil})
	tools := []any{}
	if meta != nil {
		if meta.Temperature != nil {
			temp = *meta.Temperature
		}
		if meta.TopP != nil {
			topp = *meta.TopP
		}
		if meta.MaxTokens != nil {
			maxOut = *meta.MaxTokens
		}
		if meta.ToolChoice != nil {
			toolChoice = meta.ToolChoice
		}
		if meta.ParallelToolCalls != nil {
			parallel = *meta.ParallelToolCalls
		}
		if meta.Instructions != "" {
			instructions = meta.Instructions
		} else if len(meta.System) > 0 {
			instructions = blocksToText(meta.System)
		}
		if len(meta.Tools) > 0 {
			for _, t := range meta.Tools {
				tools = append(tools, responsesToolEcho{
					Type: "function", Name: t.Name, Description: t.Description, Strict: nil,
					Parameters: json.RawMessage(t.Parameters),
				})
			}
		}
		if meta.ReasoningEffort != "" {
			reasoning = map[string]any{"effort": meta.ReasoningEffort, "summary": nil}
		}
	}
	created := resp.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	out := responsesNonStreamResponse{
		ID: responseID, Object: "response",
		CreatedAt: created, CompletedAt: created, Status: "completed",
		ParallelToolCalls: parallel, Temperature: temp, TopP: topp,
		MaxOutputTokens: maxOut, PreviousResponseID: nil, Background: false,
		Truncation: "disabled", TopLogprobs: 0, MaxToolCalls: nil, PromptCacheRetention: nil,
		Model: resp.Model, Error: nil, IncompleteDetails: nil,
		Output: output, Usage: usage, Instructions: instructions,
		ToolChoice: toolChoice, Tools: tools, Reasoning: reasoning,
		Text:       responsesTextObj{Verbosity: nil, Format: responsesTextFormatObj{Type: "text"}},
		Moderation: nil, Cost: "0",
	}
	return json.Marshal(out)
}

func buildAnthropicResponse(resp *Response) ([]byte, error) {
	content := make([]map[string]any, 0, 4)
	stopReason := "end_turn"
	if len(resp.Choices) > 0 {
		c := resp.Choices[0]
		stopReason = mapOpenAIStopToAnthropic(c.FinishReason)
		if c.Reasoning != "" {
			content = append(content, map[string]any{"type": "thinking", "thinking": c.Reasoning, "signature": ""})
		}
		if c.Text != "" {
			content = append(content, map[string]any{"type": "text", "text": c.Text})
		}
		for _, tc := range c.ToolCalls {
			var inputVal any = map[string]any{}
			if tc.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Arguments), &inputVal)
			}
			content = append(content, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": inputVal})
		}
	}
	out := map[string]any{
		"id": orDefault(resp.ID, "msg_opgo"), "type": "message",
		"role": "assistant", "model": resp.Model,
		"content": content, "stop_reason": stopReason, "stop_sequence": nil,
	}
	hasUsage := resp.Usage.TotalTokens > 0 || resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0
	if hasUsage {
		u := map[string]any{
			"input_tokens":  resp.Usage.PromptTokens,
			"output_tokens": resp.Usage.CompletionTokens,
		}
		if resp.Usage.CachedTokens > 0 {
			u["cache_read_input_tokens"] = resp.Usage.CachedTokens
		}
		if resp.Usage.CachedWriteTokens > 0 {
			u["cache_creation_input_tokens"] = resp.Usage.CachedWriteTokens
		}
		out["usage"] = u
	}
	return json.Marshal(out)
}

func mapOpenAIStopToAnthropic(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	}
	return reason
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// newResponsesID 生成与原生 Responses 相同风格的、不透明且跨请求唯一的 ID。
// 不能只使用 output_index：Codex 会跨多轮工具调用按 item ID 维护界面状态，
// 重复的 msg_0/rs_0 会让旧消息被错误覆盖、重排，甚至显示为仍在执行。
func newResponsesID(prefix string) string {
	return prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

// blocksToText 将 Block 列表拼接为纯文本（用于 instructions 字段回填）。
func blocksToText(blocks []Block) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}
