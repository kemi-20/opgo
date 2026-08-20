package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnthropicSystemBlockArrayPreserved(t *testing.T) {
	raw := []byte(`{
		"model":"mimo-v2.5","max_tokens":100,
		"system":[
			{"type":"text","text":"first instruction","cache_control":{"type":"ephemeral"}},
			{"type":"text","text":"second instruction"}
		],
		"messages":[{"role":"user","content":"hello"}]
	}`)

	for _, target := range []Format{FormatOpenAICompletions, FormatOpenAIResponses} {
		out, err := ConvertRequest(FormatAnthropic, target, raw)
		if err != nil {
			t.Fatalf("转为 %s: %v", target, err)
		}
		text := string(out)
		if !strings.Contains(text, "first instruction") || !strings.Contains(text, "second instruction") {
			t.Errorf("转为 %s 时丢失 system 块: %s", target, out)
		}
	}
}

func TestAnthropicToolChoicePreserved(t *testing.T) {
	raw := []byte(`{
		"model":"mimo-v2.5","max_tokens":100,
		"messages":[{"role":"user","content":"run it"}],
		"tools":[{"name":"diagnostic_echo","description":"echo","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"diagnostic_echo","disable_parallel_tool_use":true}
	}`)

	for _, target := range []Format{FormatOpenAICompletions, FormatOpenAIResponses} {
		out, err := ConvertRequest(FormatAnthropic, target, raw)
		if err != nil {
			t.Fatalf("转为 %s: %v", target, err)
		}
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatal(err)
		}
		if obj["parallel_tool_calls"] != false {
			t.Errorf("转为 %s 时丢失 disable_parallel_tool_use: %s", target, out)
		}
		choice, _ := obj["tool_choice"].(map[string]any)
		if choice == nil || choice["type"] != "function" {
			t.Errorf("转为 %s 时丢失强制工具选择: %s", target, out)
		}
	}
}

func TestResponsesToolCallMapsToAnthropicToolUseStop(t *testing.T) {
	nonStream := []byte(`{
		"id":"resp_test","object":"response","status":"completed","model":"muse-spark-1.2-contributor",
		"output":[
			{"type":"message","content":[{"type":"output_text","text":"calling"}]},
			{"type":"function_call","call_id":"call_1","name":"probe","arguments":"{}"}
		]
	}`)
	out, err := ConvertResponse(FormatAnthropic, FormatOpenAIResponses, nonStream)
	if err != nil {
		t.Fatal(err)
	}
	var message map[string]any
	if err := json.Unmarshal(out, &message); err != nil {
		t.Fatal(err)
	}
	if message["stop_reason"] != "tool_use" {
		t.Fatalf("非流式 Responses 工具调用 stop_reason=%v, want tool_use: %s", message["stop_reason"], out)
	}

	conv := NewStreamConverter(FormatOpenAIResponses, FormatAnthropic, "muse-spark-1.2-contributor")
	lines := []string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","status":"in_progress","call_id":"call_1","name":"probe","arguments":""}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{}"}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"probe","arguments":"{}"}}`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
	}
	var stopReason string
	for _, line := range lines {
		chunks, _ := conv.Feed([]byte(line))
		for _, chunk := range chunks {
			payload := strings.TrimSpace(strings.TrimPrefix(string(chunk), "data:"))
			var event map[string]any
			if json.Unmarshal([]byte(payload), &event) == nil && event["type"] == "message_delta" {
				delta, _ := event["delta"].(map[string]any)
				stopReason, _ = delta["stop_reason"].(string)
			}
		}
	}
	if stopReason != "tool_use" {
		t.Fatalf("流式 Responses 工具调用 stop_reason=%q, want tool_use", stopReason)
	}
}

// TestResponsesAssistantTurnMergedLikeCLIProxyAPI 验证 Responses 的同轮输出不会被
// 拆成多个连续 assistant 消息。Codex 会把 reasoning、可见进度文本和 function_call
// 分成顶层项；Chat Completions 历史必须还原为一条 assistant 消息。
func TestResponsesAssistantTurnMergedLikeCLIProxyAPI(t *testing.T) {
	raw := []byte(`{
		"model":"mimo-v2.5","stream":true,
		"reasoning":{"effort":"max"},
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"依次调用三个工具"}]},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"先调用第一个。"},{"type":"summary_text","text":"然后继续。"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"现在调用第一个工具。"}]},
			{"type":"function_call","call_id":"call_1","name":"exec","arguments":"{\"input\":\"one\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"one ok"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"第一个成功。"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"现在调用第二个工具。"}]},
			{"type":"function_call","call_id":"call_2","name":"exec","arguments":"{\"input\":\"two\"}"},
			{"type":"function_call_output","call_id":"call_2","output":"two ok"}
		],
		"tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}]
	}`)
	out, err := ConvertRequest(FormatOpenAIResponses, FormatOpenAICompletions, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj struct {
		Messages []struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCallID       string `json:"tool_call_id"`
			ToolCalls        []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj.Messages) != 5 {
		t.Fatalf("messages = %d, want user/assistant/tool/assistant/tool:\n%s", len(obj.Messages), out)
	}
	for _, index := range []int{1, 3} {
		message := obj.Messages[index]
		if message.Role != "assistant" || message.Content == "" || message.ReasoningContent == "" || len(message.ToolCalls) != 1 {
			t.Fatalf("messages[%d] 未合并 reasoning+text+tool_calls: %+v\n%s", index, message, out)
		}
	}
	if obj.Messages[1].ReasoningContent != "先调用第一个。然后继续。" {
		t.Fatalf("多段 reasoning 未完整保留: %q", obj.Messages[1].ReasoningContent)
	}
	if obj.Messages[2].Role != "tool" || obj.Messages[2].ToolCallID != "call_1" || obj.Messages[4].ToolCallID != "call_2" {
		t.Fatalf("工具结果配对错误: %+v", obj.Messages)
	}
}

func TestHy3ReasoningEffortNormalization(t *testing.T) {
	for input, want := range map[string]string{
		"none": "no_think", "minimal": "no_think", "no_think": "no_think",
		"low": "low", "medium": "high", "high": "high", "xhigh": "high", "max": "high",
	} {
		if got := normalizeCompletionsReasoningEffort("hy3", input); got != want {
			t.Errorf("hy3 %q = %q, want %q", input, got, want)
		}
	}
	if got := normalizeCompletionsReasoningEffort("mimo-v2.5", "max"); got != "max" {
		t.Errorf("非 hy3 不应改写: %q", got)
	}
}

// TestToolChoiceParallelForwarding 验证 tool_choice / parallel_tool_calls 在双向转换中不再丢失。
func TestToolChoiceParallelForwarding(t *testing.T) {
	// Responses -> completions
	raw := []byte(`{
		"model":"mimo-v2.5","stream":true,
		"parallel_tool_calls": false,
		"tool_choice": {"type":"function","name":"exec_command"},
		"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]
	}`)
	out, err := ConvertRequest(FormatOpenAIResponses, FormatOpenAICompletions, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if ptc, _ := obj["parallel_tool_calls"].(bool); ptc {
		t.Errorf("parallel_tool_calls 应转发 false: %v", obj["parallel_tool_calls"])
	}
	tc, _ := obj["tool_choice"].(map[string]any)
	if tc == nil || tc["type"] != "function" {
		t.Fatalf("tool_choice 丢失: %v", obj["tool_choice"])
	}
	fn, _ := tc["function"].(map[string]any)
	if fn == nil || fn["name"] != "exec_command" {
		t.Errorf("tool_choice 未转 completions 嵌套形式: %v", obj["tool_choice"])
	}

	// Completions -> Responses
	raw2 := []byte(`{
		"model":"gpt-5.6-luna","stream":true,
		"parallel_tool_calls": true,
		"tool_choice": {"type":"function","function":{"name":"exec_command"}},
		"messages":[{"role":"user","content":"hi"}]
	}`)
	out2, err := ConvertRequest(FormatOpenAICompletions, FormatOpenAIResponses, raw2)
	if err != nil {
		t.Fatal(err)
	}
	var obj2 map[string]any
	_ = json.Unmarshal(out2, &obj2)
	if ptc, _ := obj2["parallel_tool_calls"].(bool); !ptc {
		t.Errorf("parallel_tool_calls 应转发 true: %v", obj2["parallel_tool_calls"])
	}
	tc2, _ := obj2["tool_choice"].(map[string]any)
	if tc2 == nil || tc2["type"] != "function" || tc2["name"] != "exec_command" {
		t.Errorf("tool_choice 未转 Responses 展平形式: %v", obj2["tool_choice"])
	}
	if _, ok := obj2["include"]; ok {
		t.Errorf("include 不应注入（上游拒绝）: %v", obj2["include"])
	}
}

// TestNoTextDuplicationInResponses 验证文本不会因 Text/Content 双字段重复输出。
func TestNoTextDuplicationInResponses(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-luna","messages":[{"role":"user","content":"hi"}]}`)
	out, err := ConvertRequest(FormatOpenAICompletions, FormatOpenAIResponses, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	input, _ := obj["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %v", input)
	}
	um := input[0].(map[string]any)
	content, _ := um["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content 应只有 1 个文本块（防重复）: %v", content)
	}
	part := content[0].(map[string]any)
	if part["type"] != "input_text" || part["text"] != "hi" {
		t.Errorf("content part = %v", part)
	}
}

// TestToolResultTextPreserved 验证 Anthropic tool_result 的文本提取与同消息普通文本保留。
func TestToolResultTextPreserved(t *testing.T) {
	raw := []byte(`{
		"model":"mimo-v2.5","stream":true,"max_tokens":100,
		"messages":[
			{"role":"user","content":"Call tool"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"exec_command","input":{"command":"echo hi"}}]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]},
				{"type":"text","text":"Now write the essay."}
			]}
		],
		"tools":[{"name":"exec_command","description":"run","input_schema":{"type":"object","properties":{}}}]
	}`)
	out, err := ConvertRequest(FormatAnthropic, FormatOpenAICompletions, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	msgs, _ := obj["messages"].([]any)
	var toolContent, userText string
	var sawImage bool
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "tool" {
			toolContent, _ = mm["content"].(string)
		}
		if mm["role"] != "user" {
			continue
		}
		switch content := mm["content"].(type) {
		case string:
			if strings.Contains(content, "essay") {
				userText = content
			}
		case []any:
			for _, item := range content {
				part, _ := item.(map[string]any)
				switch part["type"] {
				case "text":
					if text, _ := part["text"].(string); strings.Contains(text, "essay") {
						userText = text
					}
				case "image_url":
					sawImage = true
				}
			}
		}
	}
	if toolContent != "hi" {
		t.Errorf("tool result 应提取为纯文本 hi, got %q", toolContent)
	}
	if !strings.Contains(userText, "Now write the essay.") {
		t.Errorf("同消息普通文本被丢弃: %q", userText)
	}
	if !sawImage {
		t.Error("工具结果图片应移动到后续 user 多模态消息，而不是丢失或放进 role=tool")
	}
}

// TestSystemBlocksSeparated 验证多 system/instructions 块用换行分隔。
func TestSystemBlocksSeparated(t *testing.T) {
	raw := []byte(`{
		"model":"mimo-v2.5","stream":true,
		"instructions":"You are Codex.",
		"input":[
			{"role":"system","content":[{"type":"input_text","text":"Be careful."}]},
			{"role":"user","content":[{"type":"input_text","text":"hi"}]}
		]
	}`)
	out, err := ConvertRequest(FormatOpenAIResponses, FormatOpenAICompletions, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	msgs, _ := obj["messages"].([]any)
	sys := msgs[0].(map[string]any)
	content, _ := sys["content"].(string)
	if !strings.Contains(content, "You are Codex.\n\nBe careful.") {
		t.Errorf("system 块应换行分隔, got %q", content)
	}
}

// TestNoStreamOptionsOnResponsesPassthrough 验证 /responses 透传不再注入 stream_options（proxy 层）。
// TestToolCallLateIDNoDuplicate 验证 tool id 晚到的上游不会生成重复 function_call item。
func TestToolCallLateIDNoDuplicate(t *testing.T) {
	r := newStreamReader()
	// 第一片：有 id 有 name
	evs := r.readCompletionsLine([]byte(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":""}}]},"finish_reason":null}]}`))
	starts := 0
	for _, e := range evs {
		if e.Kind == "tool_start" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("第一片应产生 1 个 tool_start: %d", starts)
	}
	// 后续片带 id 但同 index（部分上游重复发 id）：不应再产生 tool_start
	evs2 := r.readCompletionsLine([]byte(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"arguments":"{}"}}]},"finish_reason":null}]}`))
	for _, e := range evs2 {
		if e.Kind == "tool_start" {
			t.Fatalf("同 index 重复 id 不应再产生 tool_start")
		}
	}
}

// TestReasoningSummaryEvents 验证 reasoning_summary 事件也能被读取（与 reasoning_text 等价）。
func TestReasoningSummaryEvents(t *testing.T) {
	r := newStreamReader()
	evs := r.readResponsesLine([]byte(`data: {"type":"response.reasoning_summary_text.delta","delta":"think","item_id":"rs_1"}`))
	found := false
	for _, e := range evs {
		if e.Kind == "reasoning" && e.Reasoning == "think" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasoning_summary_text.delta 未解析为 reasoning 事件: %v", evs)
	}
	evs2 := r.readResponsesLine([]byte(`data: {"type":"response.reasoning_summary_part.added","item_id":"rs_1"}`))
	foundStart := false
	for _, e := range evs2 {
		if e.Kind == "start" && e.Text == "reasoning" {
			foundStart = true
		}
	}
	if !foundStart {
		t.Fatalf("reasoning_summary_part.added 未解析为 start: %v", evs2)
	}
}

// TestAnthropicSingleMessageDelta 验证 anthropic 流式输出只发一条 message_delta：
// 上游 finish 与 usage 先后到达时合并为一条（此前会重复发两条，Claude Code 可能困惑）。
func TestAnthropicSingleMessageDelta(t *testing.T) {
	conv := NewStreamConverter(FormatOpenAICompletions, FormatAnthropic, "mimo-v2.5")
	lines := [][]byte{
		[]byte(`data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`),
		[]byte(`data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		[]byte(`data: {"id":"x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`),
		[]byte(`data: [DONE]`),
	}
	var out []byte
	for _, l := range lines {
		chunks, done := conv.Feed(l)
		for _, c := range chunks {
			out = append(out, c...)
		}
		if done {
			break
		}
	}
	for _, c := range conv.Close() {
		out = append(out, c...)
	}
	s := string(out)
	if n := strings.Count(s, `"type":"message_delta"`); n != 1 {
		t.Fatalf("message_delta 应为 1 条, got %d:\n%s", n, s)
	}
	if !strings.Contains(s, `"stop_reason":"end_turn"`) {
		t.Errorf("message_delta 缺少 end_turn stop_reason:\n%s", s)
	}
	if !strings.Contains(s, `"output_tokens":5`) {
		t.Errorf("message_delta 缺少真实 output_tokens=5:\n%s", s)
	}
	if !strings.Contains(s, `"type":"message_stop"`) {
		t.Errorf("缺少 message_stop:\n%s", s)
	}
}

// TestAnthropicSingleMessageDeltaUsageFirst 验证 usage 先于 finish 到达时也只发一条。
func TestAnthropicSingleMessageDeltaUsageFirst(t *testing.T) {
	conv := NewStreamConverter(FormatOpenAIResponses, FormatAnthropic, "gpt-5.6-luna")
	lines := [][]byte{
		[]byte(`data: {"type":"response.output_text.delta","delta":"hi"}`),
		[]byte(`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`),
		[]byte(`data: {"type":"response.output_item.done","item":{"type":"message"}}`),
	}
	var out []byte
	for _, l := range lines {
		chunks, done := conv.Feed(l)
		for _, c := range chunks {
			out = append(out, c...)
		}
		if done {
			break
		}
	}
	for _, c := range conv.Close() {
		out = append(out, c...)
	}
	s := string(out)
	if n := strings.Count(s, `"type":"message_delta"`); n != 1 {
		t.Fatalf("message_delta 应为 1 条, got %d:\n%s", n, s)
	}
}

// TestBuildAnthropicToolMessageNoPanic 验证 completions→anthropic 转换时
// role=tool 空内容不 panic，且文本正确保留（此前 m.Content[0].Content 会 panic/丢文本）。
func TestBuildAnthropicToolMessageNoPanic(t *testing.T) {
	// 空内容 tool 消息（不应 panic，content 为空字符串）
	raw := []byte(`{"model":"gpt-5.6-luna","messages":[
		{"role":"user","content":"call it"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":""}
	]}`)
	out, err := ConvertRequest(FormatOpenAICompletions, FormatAnthropic, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"type":"tool_result"`) {
		t.Errorf("缺少 tool_result:\n%s", out)
	}
	// 文本 tool 消息（应保留文本而不是空）
	raw2 := []byte(`{"model":"gpt-5.6-luna","messages":[
		{"role":"user","content":"call it"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"hello result"}
	]}`)
	out2, err := ConvertRequest(FormatOpenAICompletions, FormatAnthropic, raw2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out2), "hello result") {
		t.Errorf("tool 文本丢失:\n%s", out2)
	}
}
