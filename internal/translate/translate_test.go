package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------- 请求转换测试 ----------

func TestConvertAnthropicToCompletionsRequest(t *testing.T) {
	raw := []byte(`{
		"model": "mimo-v2.5",
		"max_tokens": 100,
		"system": "You are helpful",
		"messages": [
			{"role": "user", "content": "Hello"},
			{"role": "assistant", "content": [{"type": "text", "text": "Hi"}, {"type": "thinking", "thinking": "think..."}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "tu_1", "content": "42"}]}
		],
		"tools": [{"name": "get_weather", "description": "w", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}]
	}`)
	out, err := ConvertRequest(FormatAnthropic, FormatOpenAICompletions, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["model"] != "mimo-v2.5" {
		t.Errorf("model = %v", obj["model"])
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d", len(msgs))
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "You are helpful" {
		t.Errorf("system = %v", sys)
	}
	asm := msgs[2].(map[string]any)
	if asm["reasoning_content"] != "think..." {
		t.Errorf("reasoning_content = %v", asm["reasoning_content"])
	}
	tools, _ := obj["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %d", len(tools))
	}
	tm := tools[0].(map[string]any)
	if tm["type"] != "function" {
		t.Errorf("tool type = %v", tm["type"])
	}
}

func TestConvertCompletionsToAnthropicRequest(t *testing.T) {
	raw := []byte(`{
		"model": "mimo-v2.5",
		"stream": true,
		"messages": [
			{"role": "system", "content": "S"},
			{"role": "user", "content": "Hi"},
			{"role": "assistant", "content": "", "reasoning_content": "think", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "f", "arguments": "{\"a\":1}"}}]},
			{"role": "tool", "tool_call_id": "call_1", "content": "42"}
		],
		"tools": [{"type": "function", "function": {"name": "f", "parameters": {"type": "object"}}}],
		"stream_options": {"include_usage": true}
	}`)
	out, err := ConvertRequest(FormatOpenAICompletions, FormatAnthropic, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["stream"] != true {
		t.Errorf("stream = %v", obj["stream"])
	}
	if obj["system"] != "S" {
		t.Errorf("system = %v", obj["system"])
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d: %s", len(msgs), out)
	}
	asm := msgs[1].(map[string]any)
	ac, _ := asm["content"].([]any)
	foundThinking := false
	foundToolUse := false
	for _, c := range ac {
		cm := c.(map[string]any)
		if cm["type"] == "thinking" {
			foundThinking = true
		}
		if cm["type"] == "tool_use" && cm["name"] == "f" {
			foundToolUse = true
		}
	}
	if !foundThinking || !foundToolUse {
		t.Errorf("assistant content = %v", ac)
	}
	tr := msgs[2].(map[string]any)
	trc, _ := tr["content"].([]any)
	if len(trc) != 1 || trc[0].(map[string]any)["type"] != "tool_result" {
		t.Errorf("tool result = %v", trc)
	}
}

func TestConvertCompletionsToResponsesRequest(t *testing.T) {
	raw := []byte(`{
		"model": "mimo-v2.5",
		"messages": [{"role": "user", "content": "Hi"}],
		"stream_options": {"include_usage": true}
	}`)
	out, err := ConvertRequest(FormatOpenAICompletions, FormatOpenAIResponses, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if obj["model"] != "mimo-v2.5" {
		t.Errorf("model = %v", obj["model"])
	}
	// 注意：上游（gpt-5.6-luna）拒绝 include:["usage"]（400），且默认就在响应中返回 usage，
	// 因此转换到 Responses 时不再注入 include（参考 CLIProxyAPI 亦不注入 usage include）。
	if _, ok := obj["include"]; ok {
		t.Errorf("include 不应出现（上游拒绝 include:usage）: %v", obj["include"])
	}
	input, _ := obj["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %v", input)
	}
	um := input[0].(map[string]any)
	if um["role"] != "user" {
		t.Errorf("input role = %v", um["role"])
	}
}

func TestConvertResponsesToCompletionsRequest(t *testing.T) {
	raw := []byte(`{
		"model": "mimo-v2.5",
		"instructions": "Be concise",
		"input": "Hello there",
		"reasoning": {"effort": "high"}
	}`)
	out, err := ConvertRequest(FormatOpenAIResponses, FormatOpenAICompletions, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if obj["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v", obj["reasoning_effort"])
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d", len(msgs))
	}
	if msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["role"] != "user" {
		t.Errorf("messages roles wrong: %v", msgs)
	}
}

// ---------- 非流式响应转换测试 ----------

func TestConvertCompletionsToAnthropicResponse(t *testing.T) {
	raw := []byte(`{
		"id": "x", "object": "chat.completion", "model": "mimo-v2.5",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "hi", "reasoning_content": "think"}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15, "prompt_tokens_details": {"cached_tokens": 3}}
	}`)
	out, err := ConvertResponse(FormatAnthropic, FormatOpenAICompletions, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if obj["type"] != "message" || obj["stop_reason"] != "end_turn" {
		t.Errorf("type/stop = %v / %v", obj["type"], obj["stop_reason"])
	}
	content, _ := obj["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %v", content)
	}
	if content[0].(map[string]any)["type"] != "thinking" || content[1].(map[string]any)["type"] != "text" {
		t.Errorf("content blocks = %v", content)
	}
	usage, _ := obj["usage"].(map[string]any)
	if usage["input_tokens"] != float64(10) || usage["cache_read_input_tokens"] != float64(3) {
		t.Errorf("usage = %v", usage)
	}
}

func TestConvertAnthropicToCompletionsResponse(t *testing.T) {
	raw := []byte(`{
		"id": "m", "type": "message", "role": "assistant", "model": "mimo-v2.5",
		"content": [{"type": "thinking", "thinking": "t"}, {"type": "text", "text": "hi"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 10, "output_tokens": 5, "cache_read_input_tokens": 3}
	}`)
	out, err := ConvertResponse(FormatOpenAICompletions, FormatAnthropic, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if obj["object"] != "chat.completion" {
		t.Errorf("object = %v", obj["object"])
	}
	choices, _ := obj["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["reasoning_content"] != "t" || msg["content"] != "hi" {
		t.Errorf("message = %v", msg)
	}
	usage, _ := obj["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(5) {
		t.Errorf("usage = %v", usage)
	}
}

func TestConvertAnthropicToResponsesResponse(t *testing.T) {
	raw := []byte(`{
		"id": "m", "type": "message", "role": "assistant", "model": "mimo-v2.5",
		"content": [{"type": "thinking", "thinking": "t"}, {"type": "text", "text": "hi"}, {"type": "tool_use", "id": "tu", "name": "f", "input": {"a": 1}}],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`)
	out, err := ConvertResponse(FormatOpenAIResponses, FormatAnthropic, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if obj["object"] != "response" {
		t.Errorf("object = %v", obj["object"])
	}
	output, _ := obj["output"].([]any)
	if len(output) < 3 {
		t.Fatalf("output = %v", output)
	}
	if output[0].(map[string]any)["type"] != "reasoning" {
		t.Errorf("output[0] = %v", output[0])
	}
	if output[1].(map[string]any)["type"] != "message" {
		t.Errorf("output[1] = %v", output[1])
	}
	if output[2].(map[string]any)["type"] != "function_call" {
		t.Errorf("output[2] = %v", output[2])
	}
}

// ---------- 流式转换测试 ----------

func TestStreamCompletionsToAnthropic(t *testing.T) {
	conv := NewStreamConverter(FormatOpenAICompletions, FormatAnthropic, "mimo-v2.5")
	lines := [][]byte{
		[]byte(`data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think"},"finish_reason":null}]}`),
		[]byte(`data: {"id":"x","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`),
		[]byte(`data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		[]byte(`data: {"id":"x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`),
		[]byte(`data: [DONE]`),
	}
	var all []string
	for _, l := range lines {
		chunks, _ := conv.Feed(l)
		for _, c := range chunks {
			all = append(all, string(c))
		}
	}
	// 与 proxy 行为一致：循环结束后统一调用 Close() 补齐终止事件
	for _, c := range conv.Close() {
		all = append(all, string(c))
	}
	joined := strings.Join(all, "")
	for _, want := range []string{`"type":"message_start"`, `"type":"thinking"`, `"thinking_delta"`, `"text_delta"`, `"type":"message_delta"`, `"type":"message_stop"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺少 %s in %s", want, joined)
		}
	}
}

func TestStreamAnthropicToCompletions(t *testing.T) {
	conv := NewStreamConverter(FormatAnthropic, FormatOpenAICompletions, "mimo-v2.5")
	lines := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"m","role":"assistant","model":"mimo-v2.5","content":[]}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"think"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hi"}}`),
		[]byte(`data: {"type":"content_block_stop","index":1}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"output_tokens":5}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}
	var all []string
	for _, l := range lines {
		chunks, _ := conv.Feed(l)
		for _, c := range chunks {
			all = append(all, string(c))
		}
	}
	// 与 proxy 行为一致：循环结束后统一调用 Close() 补齐终止事件
	for _, c := range conv.Close() {
		all = append(all, string(c))
	}
	joined := strings.Join(all, "")
	for _, want := range []string{`"reasoning_content":"think"`, `"content":"hi"`, `"finish_reason":"stop"`, `data: [DONE]`} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺少 %s in %s", want, joined)
		}
	}
}

func TestParseFormat(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Format
		ok   bool
	}{
		{"", "", false},
		{"false", "", false},
		{"0", "", false},
		{"openai_completions", FormatOpenAICompletions, true},
		{"openai_responses", FormatOpenAIResponses, true},
		{"anthropic", FormatAnthropic, true},
		{"claude", FormatAnthropic, true},
	} {
		got, ok := ParseFormat(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseFormat(%q) = %q,%v want %q,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestDetectFormat(t *testing.T) {
	for _, tc := range []struct {
		path string
		want Format
		ok   bool
	}{
		{"/v1/chat/completions", FormatOpenAICompletions, true},
		{"/v1/responses", FormatOpenAIResponses, true},
		{"/v1/messages", FormatAnthropic, true},
		{"/v1/whatever", "", false},
	} {
		got, ok := DetectFormat(tc.path)
		if ok != tc.ok || got != tc.want {
			t.Errorf("DetectFormat(%q) = %q,%v want %q,%v", tc.path, got, ok, tc.want, tc.ok)
		}
	}
}

// ---------- 多模态（图片/音频）转换测试 ----------

const testPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="
const testWAV = "data:audio/wav;base64,UklGRiAgAAAB"

// OpenAI completions 图片+音频 → Anthropic：块被转换为 image/audio source
func TestConvertCompletionsToAnthropicMultimodal(t *testing.T) {
	raw := []byte(`{
		"model": "mimo-v2.5",
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "what is this"},
			{"type": "image_url", "image_url": {"url": "` + testPNG + `"}},
			{"type": "input_audio", "input_audio": {"data": "UklGRiAgAAAB", "format": "wav"}}
		]}]
	}`)
	out, err := ConvertRequest(FormatOpenAICompletions, FormatAnthropic, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	msgs, _ := obj["messages"].([]any)
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	var hasImage, hasAudio bool
	for _, c := range content {
		cm := c.(map[string]any)
		switch cm["type"] {
		case "image":
			src, _ := cm["source"].(map[string]any)
			if src["media_type"] == "image/png" && src["data"] == "iVBORw0KGgoAAAANSUhEUg==" {
				hasImage = true
			}
		case "audio":
			src, _ := cm["source"].(map[string]any)
			if src["media_type"] == "audio/wav" && src["data"] == "UklGRiAgAAAB" {
				hasAudio = true
			}
		}
	}
	if !hasImage || !hasAudio {
		t.Errorf("图片/音频未正确转换: content=%v", content)
	}
}

// Anthropic 图片+音频 → OpenAI completions
func TestConvertAnthropicToCompletionsMultimodal(t *testing.T) {
	raw := []byte(`{
		"model": "mimo-v2.5",
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "hi"},
			{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgoAAAANSUhEUg=="}},
			{"type": "audio", "source": {"type": "base64", "media_type": "audio/wav", "data": "UklGRiAgAAAB"}}
		]}]
	}`)
	out, err := ConvertRequest(FormatAnthropic, FormatOpenAICompletions, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	msgs, _ := obj["messages"].([]any)
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	var hasImage, hasAudio bool
	for _, c := range content {
		cm := c.(map[string]any)
		switch cm["type"] {
		case "image_url":
			u, _ := cm["image_url"].(map[string]any)["url"].(string)
			if u == testPNG {
				hasImage = true
			}
		case "input_audio":
			ia, _ := cm["input_audio"].(map[string]any)
			if ia["data"] == "UklGRiAgAAAB" && ia["format"] == "wav" {
				hasAudio = true
			}
		}
	}
	if !hasImage || !hasAudio {
		t.Errorf("图片/音频未正确转换: content=%v", content)
	}
}

// OpenAI completions 图片+音频 → Responses
func TestConvertCompletionsToResponsesMultimodal(t *testing.T) {
	raw := []byte(`{
		"model": "mimo-v2.5",
		"messages": [{"role": "user", "content": [
			{"type": "image_url", "image_url": {"url": "` + testPNG + `"}},
			{"type": "input_audio", "input_audio": {"data": "UklGRiAgAAAB", "format": "wav"}}
		]}]
	}`)
	out, err := ConvertRequest(FormatOpenAICompletions, FormatOpenAIResponses, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	input, _ := obj["input"].([]any)
	content, _ := input[0].(map[string]any)["content"].([]any)
	var hasImage, hasAudio bool
	for _, c := range content {
		cm := c.(map[string]any)
		switch cm["type"] {
		case "input_image":
			if cm["image_url"] == testPNG {
				hasImage = true
			}
		case "input_audio":
			ia, _ := cm["input_audio"].(map[string]any)
			if ia["data"] == "UklGRiAgAAAB" {
				hasAudio = true
			}
		}
	}
	if !hasImage || !hasAudio {
		t.Errorf("图片/音频未正确转换: content=%v", content)
	}
}

// Responses 图片+音频 → Anthropic
func TestConvertResponsesToAnthropicMultimodal(t *testing.T) {
	raw := []byte(`{
		"model": "mimo-v2.5",
		"input": [{"role": "user", "content": [
			{"type": "input_image", "image_url": "` + testPNG + `"},
			{"type": "input_audio", "input_audio": {"data": "UklGRiAgAAAB", "format": "wav"}}
		]}]
	}`)
	out, err := ConvertRequest(FormatOpenAIResponses, FormatAnthropic, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	msgs, _ := obj["messages"].([]any)
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	var hasImage, hasAudio bool
	for _, c := range content {
		cm := c.(map[string]any)
		switch cm["type"] {
		case "image":
			hasImage = true
		case "audio":
			hasAudio = true
		}
	}
	if !hasImage || !hasAudio {
		t.Errorf("图片/音频未正确转换: content=%v", content)
	}
}

// TestConvertRequestForcesIncludeUsage 验证：转换模式强制请求 usage（计费必需）。
func TestConvertRequestForcesIncludeUsage(t *testing.T) {
	// anthropic 流式 → completions：必须带 stream_options.include_usage
	raw := []byte(`{"model":"mimo-v2.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := ConvertRequest(FormatAnthropic, FormatOpenAICompletions, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	so, ok := obj["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Errorf("→completions 流式应注入 stream_options.include_usage: %s", out)
	}
	// anthropic 非流式 → responses：不再注入 include（上游拒绝 include:usage，且默认返回 usage）
	raw2 := []byte(`{"model":"mimo-v2.5","messages":[{"role":"user","content":"hi"}]}`)
	out2, err := ConvertRequest(FormatAnthropic, FormatOpenAIResponses, raw2)
	if err != nil {
		t.Fatal(err)
	}
	var obj2 map[string]any
	_ = json.Unmarshal(out2, &obj2)
	if _, ok := obj2["include"]; ok {
		t.Errorf("→responses 不应注入 include: %s", out2)
	}
}

// TestParseResponsesTopLevelToolItems 验证：Responses 历史中的顶层裸项
// function_call / function_call_output（opencode/Codex 使用 @ai-sdk/openai 发送的格式）
// 必须转换为 assistant(tool_use) / tool(tool_result) 消息，不能丢失。
func TestParseResponsesTopLevelToolItems(t *testing.T) {
	raw := []byte(`{"model":"mimo-v2.5","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"command\":\"echo hello\"}"},{"type":"function_call_output","call_id":"call_1","output":"hello\r\n"},{"type":"function_call","call_id":"call_2","name":"bash","arguments":"{\"command\":\"echo world\"}"},{"type":"function_call_output","call_id":"call_2","output":"world\r\n"}],"stream":true}`)
	req, err := parseOpenAIResponsesRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 5 {
		t.Fatalf("messages = %d, want 5 (user/assistant/tool/assistant/tool)", len(req.Messages))
	}
	m1 := req.Messages[1]
	if m1.Role != "assistant" || len(m1.Content) != 1 || m1.Content[0].Type != "tool_use" {
		t.Fatalf("messages[1] = %+v, want assistant tool_use", m1)
	}
	if m1.Content[0].ToolUseID != "call_1" || m1.Content[0].Name != "bash" {
		t.Fatalf("tool_use = %+v", m1.Content[0])
	}
	if got := string(m1.Content[0].Input); got != `{"command":"echo hello"}` {
		t.Fatalf("tool_use input = %q, want 原始 JSON 字符串", got)
	}
	m2 := req.Messages[2]
	if m2.Role != "tool" || m2.ToolCallID != "call_1" || m2.Text != "hello\r\n" {
		t.Fatalf("messages[2] = %+v, want tool result", m2)
	}
	m3 := req.Messages[3]
	if m3.Role != "assistant" || len(m3.Content) != 1 || m3.Content[0].ToolUseID != "call_2" {
		t.Fatalf("messages[3] = %+v, want assistant call_2", m3)
	}
}

// TestParseResponsesParallelToolCallsMerged 验证：连续的 function_call 顶层裸项合并为一条 assistant 消息。
func TestParseResponsesParallelToolCallsMerged(t *testing.T) {
	raw := []byte(`{"model":"mimo-v2.5","input":[{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{}"},{"type":"function_call","call_id":"call_2","name":"read","arguments":"{\"file_path\":\"a.txt\"}"},{"type":"function_call_output","call_id":"call_1","output":"ok"},{"type":"function_call_output","call_id":"call_2","output":"content"}],"stream":true}`)
	req, err := parseOpenAIResponsesRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (assistant with 2 tool_use + 2 tool)", len(req.Messages))
	}
	m0 := req.Messages[0]
	if m0.Role != "assistant" || len(m0.Content) != 2 {
		t.Fatalf("messages[0] = %+v, want assistant with 2 tool_use", m0)
	}
	if m0.Content[0].ToolUseID != "call_1" || m0.Content[1].ToolUseID != "call_2" {
		t.Fatalf("tool_use ids = %s,%s", m0.Content[0].ToolUseID, m0.Content[1].ToolUseID)
	}
	if req.Messages[1].Role != "tool" || req.Messages[1].ToolCallID != "call_1" {
		t.Fatalf("messages[1] = %+v", req.Messages[1])
	}
	if req.Messages[2].Role != "tool" || req.Messages[2].ToolCallID != "call_2" {
		t.Fatalf("messages[2] = %+v", req.Messages[2])
	}
}

// TestConvertResponsesTopLevelToolItemsToCompletions 验证：转换后的 chat.completions
// 请求包含完整的 tool_calls 与 tool 消息，且 arguments 不带多余引号。
func TestConvertResponsesTopLevelToolItemsToCompletions(t *testing.T) {
	raw := []byte(`{"model":"mimo-v2.5","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"command\":\"echo hello\"}"},{"type":"function_call_output","call_id":"call_1","output":"hello"}],"stream":true}`)
	out, err := ConvertRequest(FormatOpenAIResponses, FormatOpenAICompletions, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			Content    any    `json:"content"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("转换结果不是合法 JSON: %v\n%s", err, out)
	}
	if len(obj.Messages) != 3 {
		t.Fatalf("messages = %d, want 3: %s", len(obj.Messages), out)
	}
	as := obj.Messages[1]
	if as.Role != "assistant" || len(as.ToolCalls) != 1 {
		t.Fatalf("messages[1] = %+v, want assistant tool_calls", as)
	}
	if got := as.ToolCalls[0].Function.Arguments; got != `{"command":"echo hello"}` {
		t.Fatalf("arguments = %q, want 原始 JSON 不带多余引号", got)
	}
	tm := obj.Messages[2]
	if tm.Role != "tool" || tm.ToolCallID != "call_1" {
		t.Fatalf("messages[2] = %+v, want tool result", tm)
	}
	if got, _ := tm.Content.(string); got != "hello" {
		t.Fatalf("tool content = %#v, want hello", tm.Content)
	}
}
