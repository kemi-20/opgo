package translate

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
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
	toolIdx int
	// responses 状态
	responseStarted bool
	msgOutputID     string
	// anthropic 状态
	inThinking bool
	inText     bool
	toolName   string
	toolID     string
	curToolIdx int
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
			Index        int `json:"index"`
			Delta        struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    *int `json:"index"`
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
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
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
				events = append(events, StreamEvent{Kind: "tool_start", ToolID: tc.ID, ToolName: tc.Function.Name})
				if tc.Function.Arguments != "" {
					events = append(events, StreamEvent{Kind: "tool_args", ToolID: tc.ID, Args: tc.Function.Arguments})
				}
			} else if r.toolIdx >= 0 {
				if tc.ID != "" {
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
		Type    string `json:"type"`
		Delta   string `json:"delta"`
		Item    *struct {
			Type     string `json:"type"`
			Name     string `json:"name"`
			CallID   string `json:"call_id"`
			ID       string `json:"id"`
			Role     string `json:"role"`
			Content  []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"item"`
		Response *struct {
			Status  string `json:"status"`
			Usage   *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
				TotalTokens  int64 `json:"total_tokens"`
				InputTokensDetails *struct {
					CachedTokens int64 `json:"cached_tokens"`
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
				events = append(events, StreamEvent{Kind: "tool_start", ToolID: obj.Item.CallID, ToolName: obj.Item.Name})
			}
		}
	case "response.reasoning_text.delta":
		events = append(events, StreamEvent{Kind: "reasoning", Reasoning: obj.Delta})
	case "response.output_text.delta":
		events = append(events, StreamEvent{Kind: "text", Text: obj.Delta})
	case "response.function_call_arguments.delta":
		events = append(events, StreamEvent{Kind: "tool_args", Args: obj.Delta})
	case "response.output_item.done":
		// 结束当前块
		events = append(events, StreamEvent{Kind: "end_block"})
	case "response.completed":
		if obj.Response != nil {
			finish := "stop"
			switch obj.Response.Status {
			case "incomplete":
				finish = "length"
			case "failed":
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
				}
				if obj.Response.Usage.OutputTokensDetails != nil {
					u.CachedWriteTokens = obj.Response.Usage.OutputTokensDetails.ReasoningTokens
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
		Type    string `json:"type"`
		Index   *int   `json:"index"`
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
		Usage *struct {
			InputTokens   int64 `json:"input_tokens"`
			OutputTokens  int64 `json:"output_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
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
		// no output
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
			u := &Usage{
				PromptTokens:     obj.Usage.InputTokens,
				CompletionTokens: obj.Usage.OutputTokens,
				TotalTokens:      obj.Usage.InputTokens + obj.Usage.OutputTokens,
				CachedTokens:     obj.Usage.CacheRead,
				CachedWriteTokens: obj.Usage.CacheCreation,
			}
			events = append(events, StreamEvent{Kind: "usage", Usage: u})
		}
	case "message_stop":
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
		chunk := map[string]any{
			"id": "chatcmpl-opgo", "object": "chat.completion.chunk", "model": model,
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens": ev.Usage.PromptTokens,
				"completion_tokens": ev.Usage.CompletionTokens,
				"total_tokens": ev.Usage.TotalTokens,
				"prompt_tokens_details": map[string]any{"cached_tokens": ev.Usage.CachedTokens},
			},
		}
		b, _ := json.Marshal(chunk)
		return [][]byte{sseData(b)}
	case "done":
		return [][]byte{[]byte("data: [DONE]\n\n")}
	case "error":
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": ev.Error, "type": "opgo_translate_error"}})
		return [][]byte{sseData(b)}
	}
	return nil
}

// ---------- OpenAI Responses SSE 写入器 ----------

type responsesStreamWriter struct {
	started      bool
	nextIndex    int
	// 当前活动块
	blockType    string // "" | "reasoning" | "text" | "tool"
	itemID       string
	outputIndex  int
	toolName     string
	textAccum    string
	argsAccum    string
	completed    bool
	pendingUsage *Usage
}

func newResponsesStreamWriter() *responsesStreamWriter {
	return &responsesStreamWriter{}
}

// begin 输出 response.created / response.in_progress（每个流只一次）。
func (w *responsesStreamWriter) ensureStarted(model string, out *[][]byte) {
	if w.started {
		return
	}
	w.started = true
	created, _ := json.Marshal(map[string]any{
		"type": "response.created",
		"response": map[string]any{"id": "resp_1", "object": "response", "status": "in_progress", "model": model},
	})
	*out = append(*out, sseData(created))
	prog, _ := json.Marshal(map[string]any{
		"type": "response.in_progress",
		"response": map[string]any{"id": "resp_1", "object": "response", "status": "in_progress", "model": model},
	})
	*out = append(*out, sseData(prog))
}

// beginReasoning 声明 reasoning item + content part。
func (w *responsesStreamWriter) beginReasoning(out *[][]byte) {
	w.blockType = "reasoning"
	w.itemID = "rs_" + strconv.Itoa(w.nextIndex)
	w.outputIndex = w.nextIndex
	w.nextIndex++
	item, _ := json.Marshal(map[string]any{
		"type": "response.output_item.added", "output_index": w.outputIndex,
		"item": map[string]any{"id": w.itemID, "type": "reasoning", "summary": []any{}},
	})
	*out = append(*out, sseData(item))
	part, _ := json.Marshal(map[string]any{
		"type": "response.content_part.added", "item_id": w.itemID, "output_index": w.outputIndex,
		"part": map[string]any{"type": "summary_text", "text": "", "annotations": []any{}},
	})
	*out = append(*out, sseData(part))
}

func (w *responsesStreamWriter) endReasoning(out *[][]byte) {
	done, _ := json.Marshal(map[string]any{
		"type": "response.reasoning_text.done", "item_id": w.itemID, "output_index": w.outputIndex, "text": "",
	})
	*out = append(*out, sseData(done))
	cd, _ := json.Marshal(map[string]any{
		"type": "response.content_part.done", "item_id": w.itemID, "output_index": w.outputIndex,
		"part": map[string]any{"type": "summary_text", "text": "", "annotations": []any{}},
	})
	*out = append(*out, sseData(cd))
	oid, _ := json.Marshal(map[string]any{
		"type": "response.output_item.done", "output_index": w.outputIndex,
		"item": map[string]any{"id": w.itemID, "type": "reasoning", "summary": []any{}},
	})
	*out = append(*out, sseData(oid))
}

func (w *responsesStreamWriter) beginText(out *[][]byte) {
	w.blockType = "text"
	w.itemID = "msg_" + strconv.Itoa(w.nextIndex)
	w.outputIndex = w.nextIndex
	w.nextIndex++
	w.textAccum = ""
	item, _ := json.Marshal(map[string]any{
		"type": "response.output_item.added", "output_index": w.outputIndex,
		"item": map[string]any{"id": w.itemID, "type": "message", "role": "assistant", "content": []any{}},
	})
	*out = append(*out, sseData(item))
	part, _ := json.Marshal(map[string]any{
		"type": "response.content_part.added", "item_id": w.itemID, "output_index": w.outputIndex,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
	*out = append(*out, sseData(part))
}

func (w *responsesStreamWriter) endText(out *[][]byte) {
	done, _ := json.Marshal(map[string]any{
		"type": "response.output_text.done", "item_id": w.itemID, "output_index": w.outputIndex, "text": w.textAccum,
	})
	*out = append(*out, sseData(done))
	cd, _ := json.Marshal(map[string]any{
		"type": "response.content_part.done", "item_id": w.itemID, "output_index": w.outputIndex,
		"part": map[string]any{"type": "output_text", "text": w.textAccum, "annotations": []any{}},
	})
	*out = append(*out, sseData(cd))
	oid, _ := json.Marshal(map[string]any{
		"type": "response.output_item.done", "output_index": w.outputIndex,
		"item": map[string]any{"id": w.itemID, "type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": w.textAccum, "annotations": []any{}}}},
	})
	*out = append(*out, sseData(oid))
}

func (w *responsesStreamWriter) beginTool(ev StreamEvent, out *[][]byte) {
	w.blockType = "tool"
	id := ev.ToolID
	if id == "" {
		id = "fc_" + ev.ToolName
	}
	w.itemID = id
	w.toolName = ev.ToolName
	w.outputIndex = w.nextIndex
	w.nextIndex++
	w.argsAccum = ""
	item, _ := json.Marshal(map[string]any{
		"type": "response.output_item.added", "output_index": w.outputIndex,
		"item": map[string]any{"id": id, "type": "function_call", "call_id": id, "name": ev.ToolName, "arguments": ""},
	})
	*out = append(*out, sseData(item))
}

func (w *responsesStreamWriter) endTool(out *[][]byte) {
	argsStr := w.argsAccum
	if argsStr == "" {
		argsStr = "{}"
	}
	done, _ := json.Marshal(map[string]any{
		"type": "response.function_call_arguments.done", "item_id": w.itemID, "output_index": w.outputIndex,
		"call_id": w.itemID, "name": w.toolName, "arguments": argsStr,
	})
	*out = append(*out, sseData(done))
	// 注意：AI SDK 对 output_item.done 的 function_call 校验要求 arguments 为字符串
	oid, _ := json.Marshal(map[string]any{
		"type": "response.output_item.done", "output_index": w.outputIndex,
		"item": map[string]any{"id": w.itemID, "type": "function_call", "call_id": w.itemID, "name": w.toolName, "arguments": argsStr, "status": "completed"},
	})
	*out = append(*out, sseData(oid))
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

// emitCompleted 输出 response.completed（带 usage），只一次。
func (w *responsesStreamWriter) emitCompleted(model string, out *[][]byte) {
	if w.completed {
		return
	}
	w.completed = true
	respMap := map[string]any{"id": "resp_1", "object": "response", "status": "completed", "model": model}
	if w.pendingUsage != nil {
		respMap["usage"] = usageMap(w.pendingUsage)
		w.pendingUsage = nil
	}
	evt, _ := json.Marshal(map[string]any{"type": "response.completed", "response": respMap})
	*out = append(*out, sseData(evt))
}

func (w *responsesStreamWriter) Write(ev StreamEvent, model string) [][]byte {
	var out [][]byte
	w.ensureStarted(model, &out)
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
		evt, _ := json.Marshal(map[string]any{
			"type": "response.reasoning_text.delta", "item_id": w.itemID, "output_index": w.outputIndex,
			"delta": ev.Reasoning,
		})
		out = append(out, sseData(evt))
	case "text":
		if w.blockType != "text" {
			if w.blockType != "" {
				w.endBlock(&out)
			}
			w.beginText(&out)
		}
		w.textAccum += ev.Text
		evt, _ := json.Marshal(map[string]any{
			"type": "response.output_text.delta", "item_id": w.itemID, "output_index": w.outputIndex,
			"delta": ev.Text,
		})
		out = append(out, sseData(evt))
	case "tool_start":
		if w.blockType != "" {
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
		evt, _ := json.Marshal(map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": w.itemID, "output_index": w.outputIndex,
			"call_id": w.itemID, "delta": ev.Args,
		})
		out = append(out, sseData(evt))
	case "end_block":
		if w.blockType != "" {
			w.endBlock(&out)
		}
	case "finish":
		if w.blockType != "" {
			w.endBlock(&out)
		}
		w.emitCompleted(model, &out)
	case "usage":
		w.pendingUsage = ev.Usage
		if w.completed {
			// completed 已发，补发带 usage 的 completed（AI SDK 需要 usage）
			u := usageMap(ev.Usage)
			evt, _ := json.Marshal(map[string]any{
				"type": "response.completed",
				"response": map[string]any{"id": "resp_1", "object": "response", "status": "completed", "model": model, "usage": u},
			})
			out = append(out, sseData(evt))
		}
	case "done":
		if w.blockType != "" {
			w.endBlock(&out)
		}
		w.emitCompleted(model, &out)
	case "error":
		evt, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"message": ev.Error}})
		out = append(out, sseData(evt))
	}
	return out
}
func usageMap(u *Usage) map[string]any {
	m := map[string]any{
		"input_tokens":  u.PromptTokens,
		"output_tokens": u.CompletionTokens,
		"total_tokens":  u.TotalTokens,
	}
	if u.CachedTokens > 0 {
		m["input_tokens_details"] = map[string]any{"cached_tokens": u.CachedTokens}
	}
	if u.CachedWriteTokens > 0 {
		m["output_tokens_details"] = map[string]any{"reasoning_tokens": u.CachedWriteTokens}
	}
	return m
}

// ---------- Anthropic SSE 写入器 ----------

type anthropicStreamWriter struct {
	started   bool
	blockIdx  int
	blockType string
	curIdx    int
	finished  bool
	deltaSent bool
}

func newAnthropicStreamWriter() *anthropicStreamWriter {
	return &anthropicStreamWriter{}
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
		w.endBlock(&out)
		if !w.deltaSent {
			delta, _ := json.Marshal(map[string]any{
				"type": "message_delta",
				"delta": map[string]any{"stop_reason": mapOpenAIStopToAnthropic(ev.Finish), "stop_sequence": nil},
				"usage": map[string]any{"output_tokens": 0},
			})
			out = append(out, sseData(delta))
			w.deltaSent = true
		}
	case "usage":
		u := map[string]any{
			"input_tokens":  ev.Usage.PromptTokens,
			"output_tokens": ev.Usage.CompletionTokens,
		}
		if ev.Usage.CachedTokens > 0 {
			u["cache_read_input_tokens"] = ev.Usage.CachedTokens
		}
		if ev.Usage.CachedWriteTokens > 0 {
			u["cache_creation_input_tokens"] = ev.Usage.CachedWriteTokens
		}
		if !w.finished {
			w.finished = true
			w.endBlock(&out)
		}
		delta, _ := json.Marshal(map[string]any{
			"type": "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": u,
		})
		out = append(out, sseData(delta))
		w.deltaSent = true
	case "done":
		if !w.finished {
			w.finished = true
			w.endBlock(&out)
			delta, _ := json.Marshal(map[string]any{
				"type": "message_delta",
				"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
				"usage": map[string]any{"output_tokens": 0},
			})
			out = append(out, sseData(delta))
			w.deltaSent = true
		}
		stop, _ := json.Marshal(map[string]any{"type": "message_stop"})
		out = append(out, sseData(stop))
	case "error":
		evt, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": ev.Error}})
		out = append(out, sseData(evt))
	}
	return out
}
