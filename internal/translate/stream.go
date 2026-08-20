package translate

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// StreamEvent 流式转换的中间增量事件。
type StreamEvent struct {
	Kind      string // start | reasoning | text | tool_start | tool_args | tool_end | finish | usage | done | error
	Text      string
	Reasoning string
	ToolID    string
	ToolName  string
	Args      string
	Finish    string
	Usage     *Usage
	Error     string
}

// SSE 行处理：读取器把一行 `data: {...}` 转为一个或多个增量事件。

// ---------- OpenAI chat/completions SSE 读取器 ----------

// streamReader 持有跨行状态。
type streamReader struct {
	// completions 状态
	toolIdx   int
	toolHasID bool // 当前 tool index 是否已发出过带 id 的 tool_start（防重复 item）
	// responses 状态
	responseStarted bool
	msgOutputID     string
	responseHasTool bool
	// anthropic 状态
	inThinking bool
	inText     bool
	toolName   string
	toolID     string
	curToolIdx int
	// Anthropic 把输入/cache usage 放在 message_start，把输出 usage 放在
	// message_delta；这里合并后只向目标 writer 发一次完整 usage。
	anthropicUsage        Usage
	anthropicUsageSeen    bool
	anthropicUsageEmitted bool
}

func newStreamReader() *streamReader { return &streamReader{toolIdx: -1, curToolIdx: -1} }

// readCompletionsLine 解析 openai chat/completions SSE 行。
func (r *streamReader) readCompletionsLine(line []byte) []StreamEvent {
	s := strings.TrimSpace(string(line))
	if !strings.HasPrefix(s, "data:") {
		return nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if payload == "" {
		return nil
	}
	if payload == "[DONE]" {
		return []StreamEvent{{Kind: "done"}}
	}
	var obj struct {
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				ToolCalls        []struct {
					Index    *int   `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
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
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		return nil
	}
	var events []StreamEvent
	if len(obj.Choices) > 0 {
		d := obj.Choices[0].Delta
		if d.ReasoningContent != "" {
			events = append(events, StreamEvent{Kind: "reasoning", Reasoning: d.ReasoningContent})
		} else if d.Reasoning != "" {
			// 部分上游（如 mimo）思考字段为 delta.reasoning
			events = append(events, StreamEvent{Kind: "reasoning", Reasoning: d.Reasoning})
		}
		if d.Content != "" {
			events = append(events, StreamEvent{Kind: "text", Text: d.Content})
		}
		for _, tc := range d.ToolCalls {
			idx := r.toolIdx + 1
			if tc.Index != nil {
				idx = *tc.Index
			}
			if idx > r.toolIdx {
				r.toolIdx = idx
				r.toolHasID = tc.ID != ""
				events = append(events, StreamEvent{Kind: "tool_start", ToolID: tc.ID, ToolName: tc.Function.Name})
				if tc.Function.Arguments != "" {
					events = append(events, StreamEvent{Kind: "tool_args", ToolID: tc.ID, Args: tc.Function.Arguments})
				}
			} else if r.toolIdx >= 0 {
				// 同一 index 的后续分片：只补发一次带 id 的 tool_start（部分上游 id 晚到），
				// 避免同一工具调用生成两个 function_call item。
				if tc.ID != "" && !r.toolHasID {
					r.toolHasID = true
					events = append(events, StreamEvent{Kind: "tool_start", ToolID: tc.ID, ToolName: tc.Function.Name})
				}
				if tc.Function.Arguments != "" {
					events = append(events, StreamEvent{Kind: "tool_args", Args: tc.Function.Arguments})
				}
			}
		}
		if obj.Choices[0].FinishReason != nil && *obj.Choices[0].FinishReason != "" {
			events = append(events, StreamEvent{Kind: "finish", Finish: *obj.Choices[0].FinishReason})
		}
	}
	if obj.Usage != nil {
		u := &Usage{
			PromptTokens:     obj.Usage.PromptTokens,
			CompletionTokens: obj.Usage.CompletionTokens,
			TotalTokens:      obj.Usage.TotalTokens,
		}
		if obj.Usage.PromptTokensDetails != nil {
			u.CachedTokens = obj.Usage.PromptTokensDetails.CachedTokens
			u.CachedWriteTokens = obj.Usage.PromptTokensDetails.CacheWriteTokens
		}
		if obj.Usage.CompletionTokensDetails != nil {
			u.ReasoningTokens = obj.Usage.CompletionTokensDetails.ReasoningTokens
		}
		events = append(events, StreamEvent{Kind: "usage", Usage: u})
	}
	return events
}

// ---------- OpenAI Responses SSE 读取器 ----------

func (r *streamReader) readResponsesLine(line []byte) []StreamEvent {
	s := strings.TrimSpace(string(line))
	if !strings.HasPrefix(s, "data:") {
		return nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if payload == "" {
		return nil
	}
	var obj struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
		Item  *struct {
			Type    string `json:"type"`
			Name    string `json:"name"`
			CallID  string `json:"call_id"`
			ID      string `json:"id"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"item"`
		Response *struct {
			Status string `json:"status"`
			Usage  *struct {
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
		} `json:"response"`
		OutputIndex int `json:"output_index"`
	}
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		return nil
	}
	var events []StreamEvent
	switch obj.Type {
	case "response.created", "response.in_progress":
		// no output
	case "response.output_item.added":
		if obj.Item != nil {
			switch obj.Item.Type {
			case "reasoning":
				events = append(events, StreamEvent{Kind: "start", Text: "reasoning"})
			case "message":
				events = append(events, StreamEvent{Kind: "start", Text: "text"})
			case "function_call":
				r.responseHasTool = true
				events = append(events, StreamEvent{Kind: "tool_start", ToolID: obj.Item.CallID, ToolName: obj.Item.Name})
			}
		}
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		events = append(events, StreamEvent{Kind: "reasoning", Reasoning: obj.Delta})
	case "response.reasoning_summary_part.added":
		events = append(events, StreamEvent{Kind: "start", Text: "reasoning"})
	case "response.reasoning_summary_part.done":
		// 保持块打开，直到 output_item.done 关闭（对齐原生语义）
	case "response.output_text.delta":
		events = append(events, StreamEvent{Kind: "text", Text: obj.Delta})
	case "response.function_call_arguments.delta":
		r.responseHasTool = true
		events = append(events, StreamEvent{Kind: "tool_args", Args: obj.Delta})
	case "response.output_item.done":
		if obj.Item != nil && obj.Item.Type == "function_call" {
			r.responseHasTool = true
		}
		// 结束当前块
		events = append(events, StreamEvent{Kind: "end_block"})
	case "response.completed":
		if obj.Response != nil {
			finish := "stop"
			switch {
			case obj.Response.Status == "completed" && r.responseHasTool:
				finish = "tool_calls"
			case obj.Response.Status == "incomplete":
				finish = "length"
			case obj.Response.Status == "failed":
				finish = "content_filter"
			}
			events = append(events, StreamEvent{Kind: "finish", Finish: finish})
			if obj.Response.Usage != nil {
				u := &Usage{
					PromptTokens:     obj.Response.Usage.InputTokens,
					CompletionTokens: obj.Response.Usage.OutputTokens,
					TotalTokens:      obj.Response.Usage.TotalTokens,
				}
				if obj.Response.Usage.InputTokensDetails != nil {
					u.CachedTokens = obj.Response.Usage.InputTokensDetails.CachedTokens
					u.CachedWriteTokens = obj.Response.Usage.InputTokensDetails.CacheWriteTokens
				}
				if obj.Response.Usage.OutputTokensDetails != nil {
					u.ReasoningTokens = obj.Response.Usage.OutputTokensDetails.ReasoningTokens
				}
				events = append(events, StreamEvent{Kind: "usage", Usage: u})
			}
		}
	case "response.failed":
		events = append(events, StreamEvent{Kind: "finish", Finish: "content_filter"})
	case "error":
		events = append(events, StreamEvent{Kind: "error", Error: payload})
	}
	return events
}

// ---------- Anthropic SSE 读取器 ----------

type anthropicStreamUsage struct {
	InputTokens   int64 `json:"input_tokens"`
	OutputTokens  int64 `json:"output_tokens"`
	CacheRead     int64 `json:"cache_read_input_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
}

func (r *streamReader) mergeAnthropicUsage(u *anthropicStreamUsage) {
	if u == nil {
		return
	}
	r.anthropicUsageSeen = true
	if u.InputTokens > 0 {
		r.anthropicUsage.PromptTokens = u.InputTokens
	}
	if u.OutputTokens > 0 {
		r.anthropicUsage.CompletionTokens = u.OutputTokens
	}
	if u.CacheRead > 0 {
		r.anthropicUsage.CachedTokens = u.CacheRead
	}
	if u.CacheCreation > 0 {
		r.anthropicUsage.CachedWriteTokens = u.CacheCreation
	}
	r.anthropicUsage.TotalTokens = r.anthropicUsage.PromptTokens + r.anthropicUsage.CompletionTokens
}

func (r *streamReader) readAnthropicLine(line []byte) []StreamEvent {
	s := strings.TrimSpace(string(line))
	if !strings.HasPrefix(s, "data:") {
		return nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if payload == "" {
		return nil
	}
	var obj struct {
		Type         string `json:"type"`
		Index        *int   `json:"index"`
		ContentBlock *struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content_block"`
		Delta *struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage   *anthropicStreamUsage `json:"usage"`
		Message *struct {
			Usage *anthropicStreamUsage `json:"usage"`
		} `json:"message"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		return nil
	}
	var events []StreamEvent
	switch obj.Type {
	case "message_start":
		if obj.Message != nil {
			r.mergeAnthropicUsage(obj.Message.Usage)
		}
	case "content_block_start":
		if obj.ContentBlock != nil {
			switch obj.ContentBlock.Type {
			case "text":
				events = append(events, StreamEvent{Kind: "start", Text: "text"})
				if obj.ContentBlock.Text != "" {
					events = append(events, StreamEvent{Kind: "text", Text: obj.ContentBlock.Text})
				}
			case "thinking":
				events = append(events, StreamEvent{Kind: "start", Text: "reasoning"})
				if obj.ContentBlock.Thinking != "" {
					events = append(events, StreamEvent{Kind: "reasoning", Reasoning: obj.ContentBlock.Thinking})
				}
			case "tool_use":
				events = append(events, StreamEvent{Kind: "tool_start", ToolID: obj.ContentBlock.ID, ToolName: obj.ContentBlock.Name})
				if len(obj.ContentBlock.Input) > 0 {
					events = append(events, StreamEvent{Kind: "tool_args", Args: string(obj.ContentBlock.Input)})
				}
			}
		}
	case "content_block_delta":
		if obj.Delta != nil {
			switch obj.Delta.Type {
			case "text_delta":
				events = append(events, StreamEvent{Kind: "text", Text: obj.Delta.Text})
			case "thinking_delta":
				events = append(events, StreamEvent{Kind: "reasoning", Reasoning: obj.Delta.Thinking})
			case "input_json_delta":
				events = append(events, StreamEvent{Kind: "tool_args", Args: obj.Delta.PartialJSON})
			}
		}
	case "content_block_stop":
		events = append(events, StreamEvent{Kind: "end_block"})
	case "message_delta":
		if obj.Delta != nil && obj.Delta.StopReason != "" {
			events = append(events, StreamEvent{Kind: "finish", Finish: mapAnthropicStopToOpenAI(obj.Delta.StopReason)})
		}
		if obj.Usage != nil {
			r.mergeAnthropicUsage(obj.Usage)
			u := r.anthropicUsage
			r.anthropicUsageEmitted = true
			events = append(events, StreamEvent{Kind: "usage", Usage: &u})
		}
	case "message_stop":
		if r.anthropicUsageSeen && !r.anthropicUsageEmitted {
			// 非标准/截断流没有 message_delta 时，上游没有提供精确
			// output_tokens，不能根据文本字节数伪造 token。这里只向客户端
			// 转发已知的 input/cache 明细；代理计费独立解析上游原始 SSE，
			// 不会读取这条转换后的 usage。
			u := r.anthropicUsage
			r.anthropicUsageEmitted = true
			events = append(events, StreamEvent{Kind: "usage", Usage: &u})
		}
		events = append(events, StreamEvent{Kind: "done"})
	case "error":
		events = append(events, StreamEvent{Kind: "error", Error: payload})
	}
	return events
}

// ---------- SSE 工具 ----------

func sseData(payload []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("data: ")
	buf.Write(payload)
	buf.WriteString("\n\n")
	return buf.Bytes()
}

// ---------- OpenAI chat/completions SSE 写入器 ----------

type completionsStreamWriter struct {
	started     bool
	toolStarted map[int]bool
	nextToolIdx int
	finished    bool
}

func newCompletionsStreamWriter() *completionsStreamWriter {
	return &completionsStreamWriter{toolStarted: map[int]bool{}}
}

func (w *completionsStreamWriter) Write(ev StreamEvent, model string) [][]byte {
	// 首个 delta 事件带 role=assistant（OpenAI SDK 兼容）
	if !w.started {
		switch ev.Kind {
		case "reasoning", "text", "tool_start":
			w.started = true
		}
	}
	switch ev.Kind {
	case "start", "end_block":
		return nil
	case "reasoning":
		chunk := map[string]any{
			"id": "chatcmpl-opgo", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "reasoning_content": ev.Reasoning}, "finish_reason": nil}},
		}
		b, _ := json.Marshal(chunk)
		return [][]byte{sseData(b)}
	case "text":
		chunk := map[string]any{
			"id": "chatcmpl-opgo", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ev.Text}, "finish_reason": nil}},
		}
		b, _ := json.Marshal(chunk)
		return [][]byte{sseData(b)}
	case "tool_start":
		idx := w.nextToolIdx
		w.nextToolIdx++
		w.toolStarted[idx] = true
		chunk := map[string]any{
			"id": "chatcmpl-opgo", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": idx, "id": ev.ToolID, "type": "function",
				"function": map[string]any{"name": ev.ToolName, "arguments": ""},
			}}}, "finish_reason": nil}},
		}
		b, _ := json.Marshal(chunk)
		return [][]byte{sseData(b)}
	case "tool_args":
		idx := w.nextToolIdx - 1
		if idx < 0 {
			idx = 0
		}
		chunk := map[string]any{
			"id": "chatcmpl-opgo", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": idx, "function": map[string]any{"arguments": ev.Args},
			}}}, "finish_reason": nil}},
		}
		b, _ := json.Marshal(chunk)
		return [][]byte{sseData(b)}
	case "finish":
		if w.finished {
			return nil
		}
		w.finished = true
		chunk := map[string]any{
			"id": "chatcmpl-opgo", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": ev.Finish}},
		}
		b, _ := json.Marshal(chunk)
		return [][]byte{sseData(b)}
	case "usage":
		usage := map[string]any{
			"prompt_tokens":     ev.Usage.PromptTokens,
			"completion_tokens": ev.Usage.CompletionTokens,
			"total_tokens":      ev.Usage.TotalTokens,
			"prompt_tokens_details": map[string]any{
				"cached_tokens":      ev.Usage.CachedTokens,
				"cache_write_tokens": ev.Usage.CachedWriteTokens,
			},
		}
		if ev.Usage.ReasoningTokens > 0 {
			usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": ev.Usage.ReasoningTokens}
		}
		chunk := map[string]any{
			"id": "chatcmpl-opgo", "object": "chat.completion.chunk", "model": model,
			"choices": []any{},
			"usage":   usage,
		}
		b, _ := json.Marshal(chunk)
		return [][]byte{sseData(b)}
	case "done":
		// 终止事件由 Close() 统一补齐，避免重复 [DONE]
		return nil
	case "error":
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": ev.Error, "type": "opgo_translate_error"}})
		return [][]byte{sseData(b)}
	}
	return nil
}

// ---------- OpenAI Responses SSE 写入器 ----------
// 严格对齐上游原生 Responses 流式事件结构（以 deepseek-v4-flash 原生为基准）：
// 每个事件带递增 sequence_number；response.created/in_progress/completed 携带完整
// response 对象（含 tools/temperature 等回显）；item/part 字段与原生逐一对齐。

type responsesResponseObj struct {
	ID                   string `json:"id"`
	Object               string `json:"object"`
	CreatedAt            int64  `json:"created_at"`
	CompletedAt          any    `json:"completed_at"`
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
	EndTurn              *bool  `json:"end_turn,omitempty"`
}

type responsesInputTokensDetails struct {
	CachedTokens     int64 `json:"cached_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

type responsesOutputTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

type responsesUsageObj struct {
	InputTokens         int64                        `json:"input_tokens"`
	OutputTokens        int64                        `json:"output_tokens"`
	TotalTokens         int64                        `json:"total_tokens"`
	InputTokensDetails  responsesInputTokensDetails  `json:"input_tokens_details"`
	OutputTokensDetails responsesOutputTokensDetails `json:"output_tokens_details"`
}

type responsesReasoningContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesReasoningItem struct {
	ID      string                          `json:"id"`
	Type    string                          `json:"type"`
	Status  string                          `json:"status"`
	Content []responsesReasoningContentPart `json:"content"`
	Summary []any                           `json:"summary"`
}

type responsesMessageContentPart struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
	Logprobs    []any  `json:"logprobs"`
}

type responsesMessageItem struct {
	ID      string                        `json:"id"`
	Type    string                        `json:"type"`
	Status  string                        `json:"status"`
	Role    string                        `json:"role"`
	Phase   string                        `json:"phase,omitempty"`
	Content []responsesMessageContentPart `json:"content"`
}

type responsesFunctionCallItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Name      string `json:"name"`
	CallID    string `json:"call_id"`
	Arguments string `json:"arguments"`
}

type responsesToolEcho struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Strict      any             `json:"strict"`
	Parameters  json.RawMessage `json:"parameters"`
}

type responsesTextFormatObj struct {
	Type string `json:"type"`
}

type responsesTextObj struct {
	Verbosity any                    `json:"verbosity"`
	Format    responsesTextFormatObj `json:"format"`
}

type responsesCreatedEvent struct {
	Type           string               `json:"type"`
	SequenceNumber int                  `json:"sequence_number"`
	Response       responsesResponseObj `json:"response"`
}

type responsesCompletedEvent struct {
	Type           string               `json:"type"`
	SequenceNumber int                  `json:"sequence_number"`
	Response       responsesResponseObj `json:"response"`
}

type responsesOutputItemAddedEvent struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	OutputIndex    int    `json:"output_index"`
	Item           any    `json:"item"`
}

type responsesOutputItemDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	OutputIndex    int    `json:"output_index"`
	Item           any    `json:"item"`
}

type responsesContentPartAddedEvent struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index"`
	ItemID         string `json:"item_id"`
	Part           any    `json:"part"`
}

type responsesContentPartDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index"`
	ItemID         string `json:"item_id"`
	Part           any    `json:"part"`
}

type responsesReasoningSummaryPartEvent struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	OutputIndex    int    `json:"output_index"`
	SummaryIndex   int    `json:"summary_index"`
	ItemID         string `json:"item_id"`
	Part           any    `json:"part"`
}

type responsesReasoningSummaryTextDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	OutputIndex    int    `json:"output_index"`
	SummaryIndex   int    `json:"summary_index"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
}

type responsesReasoningSummaryTextDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	OutputIndex    int    `json:"output_index"`
	SummaryIndex   int    `json:"summary_index"`
	ItemID         string `json:"item_id"`
	Text           string `json:"text"`
}

type responsesOutputTextDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
	Logprobs       []any  `json:"logprobs"`
}

type responsesOutputTextDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index"`
	ItemID         string `json:"item_id"`
	Text           string `json:"text"`
	Logprobs       []any  `json:"logprobs"`
}

type responsesFunctionCallArgsDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
}

type responsesFunctionCallArgsDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
	Arguments      string `json:"arguments"`
	Name           string `json:"name"`
}

type responsesPingEvent struct {
	Type string `json:"type"`
	Cost string `json:"cost"`
}

type responsesStreamWriter struct {
	meta               *Request
	model              string
	respID             string
	createdAt          int64
	started            bool
	nextIndex          int
	blockType          string // "" | "reasoning" | "text" | "tool"
	itemID             string
	toolCallID         string
	outputIndex        int
	toolName           string
	textAccum          string
	reasonAccum        string
	argsAccum          string
	completed          bool
	finishSeen         bool
	pendingUsage       *Usage
	outputItems        []any
	seq                int
	pingSent           bool
	forceFollowUp      bool
	textFollowedByTool bool
}

func newResponsesStreamWriter(meta *Request) *responsesStreamWriter {
	return &responsesStreamWriter{meta: meta, respID: newResponsesID("resp"), createdAt: time.Now().Unix(), outputItems: []any{}}
}

// nextSeq 返回当前 sequence_number 并递增（对齐原生流式协议）。
func (w *responsesStreamWriter) nextSeq() int {
	s := w.seq
	w.seq++
	return s
}

// responseObj 构建与原生逐字段对齐的 response 对象。
func (w *responsesStreamWriter) responseObj(status string, completedAt any, usage any) responsesResponseObj {
	temp := any(1)
	topp := any(1)
	var maxOut any
	toolChoice := any("auto")
	parallel := true
	instructions := any(nil)
	reasoning := any(map[string]any{"effort": nil, "summary": nil})
	tools := []any{}
	if w.meta != nil {
		if w.meta.Temperature != nil {
			temp = *w.meta.Temperature
		}
		if w.meta.TopP != nil {
			topp = *w.meta.TopP
		}
		if w.meta.MaxTokens != nil {
			maxOut = *w.meta.MaxTokens
		}
		if w.meta.ToolChoice != nil {
			toolChoice = w.meta.ToolChoice
		}
		if w.meta.ParallelToolCalls != nil {
			parallel = *w.meta.ParallelToolCalls
		}
		if w.meta.Instructions != "" {
			instructions = w.meta.Instructions
		}
		if len(w.meta.Tools) > 0 {
			for _, t := range w.meta.Tools {
				tools = append(tools, responsesToolEcho{
					Type: "function", Name: t.Name, Description: t.Description, Strict: nil,
					Parameters: json.RawMessage(t.Parameters),
				})
			}
		}
		if w.meta.ReasoningEffort != "" {
			reasoning = map[string]any{"effort": w.meta.ReasoningEffort, "summary": nil}
		}
	}
	return responsesResponseObj{
		ID: w.respID, Object: "response", CreatedAt: w.createdAt, CompletedAt: completedAt,
		Status: status, ParallelToolCalls: parallel, Temperature: temp, TopP: topp,
		MaxOutputTokens: maxOut, PreviousResponseID: nil, Background: false,
		Truncation: "disabled", TopLogprobs: 0, MaxToolCalls: nil, PromptCacheRetention: nil,
		Model: w.model, Error: nil, IncompleteDetails: nil, Output: w.outputItems,
		Usage: usage, Instructions: instructions, ToolChoice: toolChoice, Tools: tools,
		Reasoning:  reasoning,
		Text:       responsesTextObj{Verbosity: nil, Format: responsesTextFormatObj{Type: "text"}},
		Moderation: nil, EndTurn: func() *bool {
			if !w.forceFollowUp {
				return nil
			}
			v := false
			return &v
		}(),
	}
}

// ensureStarted 输出 response.created / response.in_progress（每个流只一次，携带完整对象）。
func (w *responsesStreamWriter) ensureStarted(out *[][]byte) {
	if w.started {
		return
	}
	w.started = true
	obj := w.responseObj("in_progress", nil, nil)
	created, _ := json.Marshal(responsesCreatedEvent{Type: "response.created", SequenceNumber: w.nextSeq(), Response: obj})
	*out = append(*out, sseData(created))
	prog, _ := json.Marshal(responsesCreatedEvent{Type: "response.in_progress", SequenceNumber: w.nextSeq(), Response: obj})
	*out = append(*out, sseData(prog))
}

func (w *responsesStreamWriter) beginReasoning(out *[][]byte) {
	w.blockType = "reasoning"
	w.reasonAccum = ""
	w.itemID = newResponsesID("rs")
	w.outputIndex = w.nextIndex
	w.nextIndex++
	item := responsesReasoningItem{ID: w.itemID, Type: "reasoning", Status: "in_progress", Content: []responsesReasoningContentPart{}, Summary: []any{}}
	ev, _ := json.Marshal(responsesOutputItemAddedEvent{Type: "response.output_item.added", SequenceNumber: w.nextSeq(), OutputIndex: w.outputIndex, Item: item})
	*out = append(*out, sseData(ev))
	part, _ := json.Marshal(responsesReasoningSummaryPartEvent{Type: "response.reasoning_summary_part.added", SequenceNumber: w.nextSeq(), OutputIndex: w.outputIndex, SummaryIndex: 0, ItemID: w.itemID, Part: responsesReasoningContentPart{Type: "summary_text", Text: ""}})
	*out = append(*out, sseData(part))
}

func (w *responsesStreamWriter) endReasoning(out *[][]byte) {
	done, _ := json.Marshal(responsesReasoningSummaryTextDoneEvent{Type: "response.reasoning_summary_text.done", SequenceNumber: w.nextSeq(), OutputIndex: w.outputIndex, SummaryIndex: 0, ItemID: w.itemID, Text: w.reasonAccum})
	*out = append(*out, sseData(done))
	cd, _ := json.Marshal(responsesReasoningSummaryPartEvent{Type: "response.reasoning_summary_part.done", SequenceNumber: w.nextSeq(), OutputIndex: w.outputIndex, SummaryIndex: 0, ItemID: w.itemID, Part: responsesReasoningContentPart{Type: "summary_text", Text: w.reasonAccum}})
	*out = append(*out, sseData(cd))
	item := responsesReasoningItem{ID: w.itemID, Type: "reasoning", Status: "completed", Content: []responsesReasoningContentPart{}, Summary: []any{responsesReasoningContentPart{Type: "summary_text", Text: w.reasonAccum}}}
	oid, _ := json.Marshal(responsesOutputItemDoneEvent{Type: "response.output_item.done", SequenceNumber: w.nextSeq(), OutputIndex: w.outputIndex, Item: item})
	*out = append(*out, sseData(oid))
	w.outputItems = append(w.outputItems, item)
}

func (w *responsesStreamWriter) beginText(out *[][]byte) {
	w.blockType = "text"
	w.itemID = newResponsesID("msg")
	w.outputIndex = w.nextIndex
	w.nextIndex++
	w.textAccum = ""
	w.textFollowedByTool = false
	// added 阶段尚不知道这是工具前说明还是最终回答；phase 缺省是 Responses
	// 明确支持的兼容形态，结束 item 时再给出准确 phase。
	item := responsesMessageItem{ID: w.itemID, Type: "message", Status: "in_progress", Role: "assistant", Content: []responsesMessageContentPart{}}
	ev, _ := json.Marshal(responsesOutputItemAddedEvent{Type: "response.output_item.added", SequenceNumber: w.nextSeq(), OutputIndex: w.outputIndex, Item: item})
	*out = append(*out, sseData(ev))
	part, _ := json.Marshal(responsesContentPartAddedEvent{Type: "response.content_part.added", SequenceNumber: w.nextSeq(), OutputIndex: w.outputIndex, ContentIndex: 0, ItemID: w.itemID, Part: responsesMessageContentPart{Type: "output_text", Text: "", Annotations: []any{}, Logprobs: []any{}}})
	*out = append(*out, sseData(part))
}

func (w *responsesStreamWriter) endText(out *[][]byte) {
	done, _ := json.Marshal(responsesOutputTextDoneEvent{Type: "response.output_text.done", SequenceNumber: w.nextSeq(), OutputIndex: w.outputIndex, ContentIndex: 0, ItemID: w.itemID, Text: w.textAccum, Logprobs: []any{}})
	*out = append(*out, sseData(done))
	cd, _ := json.Marshal(responsesContentPartDoneEvent{Type: "response.content_part.done", SequenceNumber: w.nextSeq(), OutputIndex: w.outputIndex, ContentIndex: 0, ItemID: w.itemID, Part: responsesMessageContentPart{Type: "output_text", Text: w.textAccum, Annotations: []any{}, Logprobs: []any{}}})
	*out = append(*out, sseData(cd))
	phase := "final_answer"
	if w.forceFollowUp || w.textFollowedByTool {
		phase = "commentary"
	}
	item := responsesMessageItem{ID: w.itemID, Type: "message", Status: "completed", Role: "assistant", Phase: phase, Content: []responsesMessageContentPart{{Type: "output_text", Text: w.textAccum, Annotations: []any{}, Logprobs: []any{}}}}
	oid, _ := json.Marshal(responsesOutputItemDoneEvent{Type: "response.output_item.done", SequenceNumber: w.nextSeq(), OutputIndex: w.outputIndex, Item: item})
	*out = append(*out, sseData(oid))
	w.outputItems = append(w.outputItems, item)
}

func (w *responsesStreamWriter) beginTool(ev StreamEvent, out *[][]byte) {
	w.blockType = "tool"
	// item id 独立且跨请求唯一，call_id 优先保留上游工具调用 ID。
	callID := ev.ToolID
	if callID == "" {
		callID = newResponsesID("call")
	}
	w.itemID = newResponsesID("fc")
	w.toolCallID = callID
	w.toolName = ev.ToolName
	w.outputIndex = w.nextIndex
	w.nextIndex++
	w.argsAccum = ""
	item := responsesFunctionCallItem{ID: w.itemID, Type: "function_call", Status: "in_progress", Name: ev.ToolName, CallID: callID, Arguments: ""}
	evt, _ := json.Marshal(responsesOutputItemAddedEvent{Type: "response.output_item.added", SequenceNumber: w.nextSeq(), OutputIndex: w.outputIndex, Item: item})
	*out = append(*out, sseData(evt))
}

func (w *responsesStreamWriter) endTool(out *[][]byte) {
	argsStr := w.argsAccum
	if argsStr == "" {
		argsStr = "{}"
	}
	done, _ := json.Marshal(responsesFunctionCallArgsDoneEvent{Type: "response.function_call_arguments.done", SequenceNumber: w.nextSeq(), OutputIndex: w.outputIndex, ItemID: w.itemID, Arguments: argsStr, Name: w.toolName})
	*out = append(*out, sseData(done))
	// 注意：AI SDK 对 output_item.done 的 function_call 校验要求 arguments 为字符串
	item := responsesFunctionCallItem{ID: w.itemID, Type: "function_call", Status: "completed", Name: w.toolName, CallID: w.toolCallID, Arguments: argsStr}
	oid, _ := json.Marshal(responsesOutputItemDoneEvent{Type: "response.output_item.done", SequenceNumber: w.nextSeq(), OutputIndex: w.outputIndex, Item: item})
	*out = append(*out, sseData(oid))
	w.outputItems = append(w.outputItems, item)
}

// endBlock 闭合当前活动块（按类型输出 done 序列）。
func (w *responsesStreamWriter) endBlock(out *[][]byte) {
	switch w.blockType {
	case "reasoning":
		w.endReasoning(out)
	case "text":
		w.endText(out)
	case "tool":
		w.endTool(out)
	}
	w.blockType = ""
	w.itemID = ""
	w.outputIndex = 0
}

// emitCompleted 输出 response.completed（带完整对象 + usage），只一次；随后输出原生结尾 ping。
func (w *responsesStreamWriter) emitCompleted(model string, out *[][]byte) {
	if w.completed {
		return
	}
	w.completed = true
	var usage any
	if w.pendingUsage != nil {
		usage = responsesUsageObj{
			InputTokens:  w.pendingUsage.PromptTokens,
			OutputTokens: w.pendingUsage.CompletionTokens,
			TotalTokens:  w.pendingUsage.TotalTokens,
			InputTokensDetails: responsesInputTokensDetails{
				CachedTokens:     w.pendingUsage.CachedTokens,
				CacheWriteTokens: w.pendingUsage.CachedWriteTokens,
			},
			OutputTokensDetails: responsesOutputTokensDetails{ReasoningTokens: w.pendingUsage.ReasoningTokens},
		}
		w.pendingUsage = nil
	}
	obj := w.responseObj("completed", time.Now().Unix(), usage)
	evt, _ := json.Marshal(responsesCompletedEvent{Type: "response.completed", SequenceNumber: w.nextSeq(), Response: obj})
	*out = append(*out, sseData(evt))
	w.emitPing(out)
}

func (w *responsesStreamWriter) emitPing(out *[][]byte) {
	if w.pingSent {
		return
	}
	w.pingSent = true
	evt, _ := json.Marshal(responsesPingEvent{Type: "ping", Cost: "0"})
	*out = append(*out, sseData(evt))
}

func (w *responsesStreamWriter) Write(ev StreamEvent, model string) [][]byte {
	var out [][]byte
	if w.model == "" {
		w.model = model
	}
	w.ensureStarted(&out)
	switch ev.Kind {
	case "start":
		// 若已有活动块，先闭合再开新块
		if w.blockType != "" && ev.Text != w.blockType {
			w.endBlock(&out)
		}
		if ev.Text == "reasoning" {
			w.beginReasoning(&out)
		} else if ev.Text == "text" {
			w.beginText(&out)
		}
	case "reasoning":
		if w.blockType != "reasoning" {
			if w.blockType != "" {
				w.endBlock(&out)
			}
			w.beginReasoning(&out)
		}
		w.reasonAccum += ev.Reasoning
		evt, _ := json.Marshal(responsesReasoningSummaryTextDeltaEvent{Type: "response.reasoning_summary_text.delta", SequenceNumber: w.nextSeq(), OutputIndex: w.outputIndex, SummaryIndex: 0, ItemID: w.itemID, Delta: ev.Reasoning})
		out = append(out, sseData(evt))
	case "text":
		if w.blockType != "text" {
			if w.blockType != "" {
				w.endBlock(&out)
			}
			w.beginText(&out)
		}
		w.textAccum += ev.Text
		evt, _ := json.Marshal(responsesOutputTextDeltaEvent{Type: "response.output_text.delta", SequenceNumber: w.nextSeq(), OutputIndex: w.outputIndex, ContentIndex: 0, ItemID: w.itemID, Delta: ev.Text, Logprobs: []any{}})
		out = append(out, sseData(evt))
	case "tool_start":
		if w.blockType != "" {
			if w.blockType == "text" {
				w.textFollowedByTool = true
			}
			w.endBlock(&out)
		}
		w.beginTool(ev, &out)
	case "tool_args":
		if w.blockType != "tool" {
			if w.blockType != "" {
				w.endBlock(&out)
			}
			w.beginTool(ev, &out)
		}
		w.argsAccum += ev.Args
		evt, _ := json.Marshal(responsesFunctionCallArgsDeltaEvent{Type: "response.function_call_arguments.delta", SequenceNumber: w.nextSeq(), OutputIndex: w.outputIndex, ItemID: w.itemID, Delta: ev.Args})
		out = append(out, sseData(evt))
	case "end_block":
		if w.blockType != "" {
			w.endBlock(&out)
		}
	case "finish":
		if w.blockType == "text" && ev.Finish == "stop" && shouldContinueAfterPrelude(w.textAccum, w.meta) {
			w.forceFollowUp = true
		}
		if w.blockType != "" {
			w.endBlock(&out)
		}
		// 延迟发送 response.completed：等 usage 到达后合并，只发一次（对齐上游原生格式）。
		w.finishSeen = true
		if w.pendingUsage != nil {
			w.emitCompleted(model, &out)
		}
	case "usage":
		w.pendingUsage = ev.Usage
		if w.finishSeen {
			w.emitCompleted(model, &out)
		}
	case "done":
		if w.blockType == "text" && shouldContinueAfterPrelude(w.textAccum, w.meta) {
			w.forceFollowUp = true
		}
		if w.blockType != "" {
			w.endBlock(&out)
		}
		w.emitCompleted(model, &out)
	case "error":
		evt, _ := json.Marshal(map[string]any{"type": "error", "sequence_number": w.nextSeq(), "error": map[string]any{"message": ev.Error}})
		out = append(out, sseData(evt))
	}
	return out
}

// ---------- Anthropic SSE 写入器 ----------

type anthropicStreamWriter struct {
	started      bool
	blockIdx     int
	blockType    string
	curIdx       int
	finished     bool
	deltaSent    bool
	finishReason string // 上游 finish 事件的 stop_reason，等 usage 合并后一次性发出
	pendingUsage *Usage
}

func newAnthropicStreamWriter() *anthropicStreamWriter {
	return &anthropicStreamWriter{}
}

// emitDelta 发出唯一一条 message_delta（含 stop_reason 与用量）。
func (w *anthropicStreamWriter) emitDelta(out *[][]byte, stopReason string, u *Usage) {
	if w.deltaSent {
		return
	}
	w.deltaSent = true
	if stopReason == "" {
		stopReason = "end_turn"
	}
	usage := map[string]any{"input_tokens": int64(0), "output_tokens": int64(0)}
	if u != nil {
		usage = map[string]any{"input_tokens": u.PromptTokens, "output_tokens": u.CompletionTokens}
		if u.CachedTokens > 0 {
			usage["cache_read_input_tokens"] = u.CachedTokens
		}
		if u.CachedWriteTokens > 0 {
			usage["cache_creation_input_tokens"] = u.CachedWriteTokens
		}
	}
	delta, _ := json.Marshal(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": usage,
	})
	*out = append(*out, sseData(delta))
}

func (w *anthropicStreamWriter) ensureStarted(model string, out *[][]byte) {
	if w.started {
		return
	}
	w.started = true
	start, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_opgo", "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	*out = append(*out, sseData(start))
}

func (w *anthropicStreamWriter) beginBlock(typ string, out *[][]byte, ev StreamEvent) {
	if w.blockType != "" {
		w.endBlock(out)
	}
	w.blockType = typ
	w.curIdx = w.blockIdx
	w.blockIdx++
	switch typ {
	case "thinking":
		cb, _ := json.Marshal(map[string]any{
			"type": "content_block_start", "index": w.curIdx,
			"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
		})
		*out = append(*out, sseData(cb))
	case "text":
		cb, _ := json.Marshal(map[string]any{
			"type": "content_block_start", "index": w.curIdx,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		*out = append(*out, sseData(cb))
	case "tool":
		var inputVal any = map[string]any{}
		cb, _ := json.Marshal(map[string]any{
			"type": "content_block_start", "index": w.curIdx,
			"content_block": map[string]any{"type": "tool_use", "id": ev.ToolID, "name": ev.ToolName, "input": inputVal},
		})
		*out = append(*out, sseData(cb))
	}
}

func (w *anthropicStreamWriter) endBlock(out *[][]byte) {
	if w.blockType == "" {
		return
	}
	stop, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": w.curIdx})
	*out = append(*out, sseData(stop))
	w.blockType = ""
	w.curIdx = 0
}

func (w *anthropicStreamWriter) Write(ev StreamEvent, model string) [][]byte {
	var out [][]byte
	w.ensureStarted(model, &out)
	switch ev.Kind {
	case "start":
		if ev.Text == "reasoning" {
			w.beginBlock("thinking", &out, ev)
		} else if ev.Text == "text" {
			w.beginBlock("text", &out, ev)
		}
	case "reasoning":
		if w.blockType != "thinking" {
			w.beginBlock("thinking", &out, ev)
		}
		delta, _ := json.Marshal(map[string]any{
			"type": "content_block_delta", "index": w.curIdx,
			"delta": map[string]any{"type": "thinking_delta", "thinking": ev.Reasoning},
		})
		out = append(out, sseData(delta))
	case "text":
		if w.blockType != "text" {
			w.beginBlock("text", &out, ev)
		}
		delta, _ := json.Marshal(map[string]any{
			"type": "content_block_delta", "index": w.curIdx,
			"delta": map[string]any{"type": "text_delta", "text": ev.Text},
		})
		out = append(out, sseData(delta))
	case "tool_start":
		w.beginBlock("tool", &out, ev)
	case "tool_args":
		if w.blockType != "tool" {
			w.beginBlock("tool", &out, ev)
		}
		delta, _ := json.Marshal(map[string]any{
			"type": "content_block_delta", "index": w.curIdx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.Args},
		})
		out = append(out, sseData(delta))
	case "end_block":
		w.endBlock(&out)
	case "finish":
		if w.finished {
			return out
		}
		w.finished = true
		w.finishReason = mapOpenAIStopToAnthropic(ev.Finish)
		w.endBlock(&out)
		// 延迟 message_delta：等 usage 到达后合并为一条（避免重复 message_delta）
		if w.pendingUsage != nil {
			w.emitDelta(&out, w.finishReason, w.pendingUsage)
		}
	case "usage":
		w.pendingUsage = &Usage{
			PromptTokens:      ev.Usage.PromptTokens,
			CompletionTokens:  ev.Usage.CompletionTokens,
			TotalTokens:       ev.Usage.TotalTokens,
			CachedTokens:      ev.Usage.CachedTokens,
			CachedWriteTokens: ev.Usage.CachedWriteTokens,
			ReasoningTokens:   ev.Usage.ReasoningTokens,
		}
		if w.finished && !w.deltaSent {
			w.endBlock(&out)
			w.emitDelta(&out, w.finishReason, w.pendingUsage)
		}
	case "done":
		if !w.finished {
			w.finished = true
			w.endBlock(&out)
		}
		if !w.deltaSent {
			w.emitDelta(&out, w.finishReason, w.pendingUsage)
		}
		// message_stop 由 Close() 统一补齐，避免重复
	case "error":
		evt, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": ev.Error}})
		out = append(out, sseData(evt))
	}
	return out
}
