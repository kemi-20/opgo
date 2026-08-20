package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestResponsesNativeAlignment 回归测试：mimo 转换到 Responses 的输出必须与
// GPT 风格的 Responses 客户端事件逐字段对齐：
// 1) response.created/in_progress/completed 携带完整 response 对象
// 2) item/part 事件带 status/content_index/logprobs/sequence_number
// 3) usage 始终含 input/output tokens details
// 4) 流末尾有原生 ping 事件
func TestResponsesNativeAlignment(t *testing.T) {
	meta, err := ParseRequest(FormatOpenAIResponses, []byte(`{
		"model":"mimo-v2.5","stream":true,"max_output_tokens":100,"temperature":1,"top_p":1,
		"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[{"type":"function","name":"Bash","description":"run","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	conv := NewStreamConverter(FormatOpenAICompletions, FormatOpenAIResponses, "mimo-v2.5", meta)
	lines := [][]byte{
		[]byte(`data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think"},"finish_reason":null}]}`),
		[]byte(`data: {"id":"x","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`),
		[]byte(`data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		[]byte(`data: {"id":"x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":2}}}`),
		[]byte(`data: [DONE]`),
	}
	var evs []map[string]any
	for _, l := range lines {
		chunks, done := conv.Feed(l)
		for _, c := range chunks {
			payload := strings.TrimSpace(strings.TrimPrefix(string(c), "data:"))
			if payload == "" {
				continue
			}
			var ev map[string]any
			if json.Unmarshal([]byte(payload), &ev) == nil {
				evs = append(evs, ev)
			}
		}
		if done {
			break
		}
	}
	for _, c := range conv.Close() {
		payload := strings.TrimSpace(strings.TrimPrefix(string(c), "data:"))
		if payload == "" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(payload), &ev) == nil {
			evs = append(evs, ev)
		}
	}

	// response.created / in_progress / completed 必须带完整对象
	createdKeys := map[string]bool{}
	completedObj := map[string]any(nil)
	for _, ev := range evs {
		switch ev["type"] {
		case "response.created":
			if r, ok := ev["response"].(map[string]any); ok {
				for k := range r {
					createdKeys[k] = true
				}
			}
		case "response.completed":
			if r, ok := ev["response"].(map[string]any); ok {
				completedObj = r
			}
		}
	}
	for _, k := range []string{"created_at", "completed_at", "parallel_tool_calls", "temperature", "top_p",
		"max_output_tokens", "background", "truncation", "top_logprobs", "max_tool_calls",
		"prompt_cache_retention", "error", "incomplete_details", "instructions", "tool_choice",
		"tools", "reasoning", "text", "moderation"} {
		if !createdKeys[k] {
			t.Errorf("response.created 缺字段 %s", k)
		}
	}
	// tools 回显：echo 请求中的工具
	if completedObj == nil {
		t.Fatal("缺 response.completed")
	}
	tools, _ := completedObj["tools"].([]any)
	if len(tools) != 1 {
		t.Errorf("completed.tools 应回显 1 个工具, got %d", len(tools))
	}
	// 事件字段完整性
	hasContentIndex := false
	hasLogprobs := false
	hasSeq := false
	hasPing := false
	msgStatusOK := false
	msgPhaseOK := false
	hasReasonSummaryPart := false
	hasReasonSummaryDelta := false
	hasReasonSummaryDone := false
	hasReasonSummaryPartDone := false
	hasRawReasoningEvent := false
	for _, ev := range evs {
		if ev["sequence_number"] != nil {
			hasSeq = true
		}
		switch ev["type"] {
		case "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.content_part.done":
			if _, ok := ev["content_index"]; ok {
				hasContentIndex = true
			}
			if ev["type"] == "response.output_text.delta" || ev["type"] == "response.output_text.done" {
				if _, ok := ev["logprobs"]; ok {
					hasLogprobs = true
				}
			}
		case "response.output_item.added":
			if item, ok := ev["item"].(map[string]any); ok && item["type"] == "message" {
				if item["status"] == "in_progress" {
					msgStatusOK = true
				}
				if item["phase"] == "final_answer" {
					msgPhaseOK = true
				}
			}
		case "ping":
			hasPing = true
		case "response.reasoning_summary_part.added":
			hasReasonSummaryPart = ev["summary_index"] == float64(0)
		case "response.reasoning_summary_text.delta":
			hasReasonSummaryDelta = ev["summary_index"] == float64(0)
		case "response.reasoning_summary_text.done":
			hasReasonSummaryDone = ev["summary_index"] == float64(0) && ev["text"] == "think"
		case "response.reasoning_summary_part.done":
			hasReasonSummaryPartDone = ev["summary_index"] == float64(0)
		case "response.reasoning_text.delta", "response.reasoning_text.done":
			hasRawReasoningEvent = true
		}
	}
	if !hasSeq {
		t.Error("缺少 sequence_number")
	}
	if !hasContentIndex {
		t.Error("缺少 content_index")
	}
	if !hasLogprobs {
		t.Error("缺少 logprobs")
	}
	if !msgStatusOK || !msgPhaseOK {
		t.Errorf("message item 缺 status/phase: statusOK=%v phaseOK=%v", msgStatusOK, msgPhaseOK)
	}
	if !hasPing {
		t.Error("缺少原生结尾 ping 事件")
	}
	if !hasReasonSummaryPart || !hasReasonSummaryDelta || !hasReasonSummaryDone || !hasReasonSummaryPartDone {
		t.Errorf("缺少完整 reasoning summary 生命周期: part=%v delta=%v done=%v partDone=%v", hasReasonSummaryPart, hasReasonSummaryDelta, hasReasonSummaryDone, hasReasonSummaryPartDone)
	}
	if hasRawReasoningEvent {
		t.Error("转换后的 Responses 不应混发 reasoning_text 事件")
	}
	// usage 始终带 input/output details
	usage, _ := completedObj["usage"].(map[string]any)
	if usage == nil {
		t.Fatal("completed 缺 usage")
	}
	if _, ok := usage["input_tokens_details"].(map[string]any); !ok {
		t.Errorf("usage 缺 input_tokens_details: %v", usage)
	}
	if _, ok := usage["output_tokens_details"].(map[string]any); !ok {
		t.Errorf("usage 缺 output_tokens_details: %v", usage)
	}
}

// TestResponsesNonStreamNativeShape 非流式 Responses 输出结构与原生一致：
// 顶层字段完整、转换所得思考写入 reasoning summary、usage 带 details。
func TestResponsesNonStreamNativeShape(t *testing.T) {
	raw := `{"id":"x","object":"chat.completion","model":"mimo-v2.5","created":123,"choices":[{"index":0,"message":{"role":"assistant","content":"hi","reasoning_content":"think"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	meta, _ := ParseRequest(FormatOpenAIResponses, []byte(`{"model":"mimo-v2.5","stream":false,"input":[{"role":"user","content":"hi"}]}`))
	out, err := ConvertResponseMeta(meta, FormatOpenAIResponses, FormatOpenAICompletions, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "object", "created_at", "completed_at", "status", "parallel_tool_calls",
		"temperature", "top_p", "max_output_tokens", "previous_response_id", "background", "truncation",
		"top_logprobs", "max_tool_calls", "prompt_cache_retention", "model", "error", "incomplete_details",
		"output", "usage", "instructions", "tool_choice", "tools", "reasoning", "text", "moderation", "cost"} {
		if _, ok := obj[k]; !ok {
			t.Errorf("非流式响应缺顶层字段 %s", k)
		}
	}
	output, _ := obj["output"].([]any)
	if len(output) == 0 {
		t.Fatal("output 为空")
	}
	first, _ := output[0].(map[string]any)
	if first["type"] != "reasoning" {
		t.Fatalf("第一个 output item 应为 reasoning: %v", first)
	}
	s, ok := first["summary"].([]any)
	if !ok || len(s) != 1 {
		t.Errorf("reasoning summary 未保留完整思考: %v", first["summary"])
	} else if part, ok := s[0].(map[string]any); !ok || part["type"] != "summary_text" || part["text"] != "think" {
		t.Errorf("reasoning summary 内容错误: %v", s[0])
	}
	usage, _ := obj["usage"].(map[string]any)
	if _, ok := usage["input_tokens_details"]; !ok {
		t.Errorf("usage 缺 input_tokens_details: %v", usage)
	}
	if _, ok := usage["output_tokens_details"]; !ok {
		t.Errorf("usage 缺 output_tokens_details: %v", usage)
	}
}
