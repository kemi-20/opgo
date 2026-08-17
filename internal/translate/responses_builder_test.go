package translate

import (
	"encoding/json"
	"testing"
)

// TestBuildResponsesTopLevelToolItems 验证 Anthropic→Responses 转换时
// function_call / function_call_output / reasoning 均为 input 顶层裸项，
// arguments 是 JSON 字符串，assistant 文本用 output_text，工具结果图片保留。
func TestBuildResponsesTopLevelToolItems(t *testing.T) {
	raw := []byte(`{
		"model":"mimo-v2.5","stream":true,"max_tokens":4000,
		"system":[{"type":"text","text":"You are Claude."}],
		"messages":[
			{"role":"user","content":"Call exec_command with command=\"echo hi\""},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"I should call the tool","signature":"abc"},
				{"type":"text","text":"Let me call the tool."},
				{"type":"tool_use","id":"toolu_1","name":"exec_command","input":{"command":"echo hi"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]},
				{"type":"text","text":"Now write the essay."}
			]}
		],
		"tools":[{"name":"exec_command","description":"Run a shell command.","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}]
	}`)
	out, err := ConvertRequest(FormatAnthropic, FormatOpenAIResponses, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	// 顶层项顺序：message(user) / reasoning / message(assistant) / function_call / function_call_output / message(user)
	var types []string
	for _, it := range obj.Input {
		if typ, _ := it["type"].(string); typ != "" {
			types = append(types, typ)
		} else {
			types = append(types, "message:"+it["role"].(string))
		}
	}
	want := []string{"message:user", "reasoning", "message:assistant", "function_call", "function_call_output", "message:user"}
	if len(types) != len(want) {
		t.Fatalf("input 项 = %v, want %v\n%s", types, want, out)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("input[%d] = %s, want %s\n%s", i, types[i], want[i], out)
		}
	}
	// function_call: arguments 必须是字符串、call_id 保留
	fc := obj.Input[3]
	if fc["call_id"] != "toolu_1" || fc["name"] != "exec_command" {
		t.Errorf("function_call = %v", fc)
	}
	if args, ok := fc["arguments"].(string); !ok || args != `{"command":"echo hi"}` {
		t.Errorf("function_call.arguments 应为 JSON 字符串, got %#v", fc["arguments"])
	}
	// function_call_output: 图片保留为 input_image
	fco := obj.Input[4]
	outArr, _ := fco["output"].([]any)
	if len(outArr) != 2 {
		t.Fatalf("function_call_output.output = %#v, want 2 parts", fco["output"])
	}
	p0 := outArr[0].(map[string]any)
	if p0["type"] != "input_text" || p0["text"] != "hi" {
		t.Errorf("output part0 = %v", p0)
	}
	p1 := outArr[1].(map[string]any)
	if p1["type"] != "input_image" || p1["image_url"] != "data:image/png;base64,AAAA" {
		t.Errorf("output part1 = %v", p1)
	}
	// assistant 消息文本用 output_text
	asm := obj.Input[2]
	content, _ := asm["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "output_text" {
		t.Errorf("assistant content = %v, want output_text", asm["content"])
	}
	// reasoning summary 带原文
	rs := obj.Input[1]
	sum, _ := rs["summary"].([]any)
	if len(sum) == 0 || sum[0].(map[string]any)["text"] != "I should call the tool" {
		t.Errorf("reasoning summary = %v", rs["summary"])
	}
}

// TestBuildResponsesCompletionsToolHistory 验证 completions→Responses 时
// role=tool 转顶层 function_call_output、assistant tool_calls 转顶层 function_call，
// 只有工具调用的 assistant 不产生空 message 项。
func TestBuildResponsesCompletionsToolHistory(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.6-luna","stream":true,
		"messages":[
			{"role":"user","content":"Call the tool"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{\"command\":\"echo hi\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"hi"}
		],
		"tools":[{"type":"function","function":{"name":"exec_command","description":"run","parameters":{"type":"object"}}}]
	}`)
	out, err := ConvertRequest(FormatOpenAICompletions, FormatOpenAIResponses, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if len(obj.Input) != 3 {
		t.Fatalf("input = %d items, want 3 (user / function_call / function_call_output; 空 assistant 不输出):\n%s", len(obj.Input), out)
	}
	if obj.Input[0]["role"] != "user" {
		t.Errorf("input[0] = %v", obj.Input[0])
	}
	if obj.Input[1]["type"] != "function_call" || obj.Input[1]["call_id"] != "call_1" || obj.Input[1]["arguments"] != `{"command":"echo hi"}` {
		t.Errorf("input[1] = %v", obj.Input[1])
	}
	if obj.Input[2]["type"] != "function_call_output" || obj.Input[2]["call_id"] != "call_1" {
		t.Errorf("input[2] = %v", obj.Input[2])
	}
	if s, _ := obj.Input[2]["output"].(string); s != "hi" {
		t.Errorf("input[2] output = %#v", obj.Input[2]["output"])
	}
}

// TestParseResponsesTopLevelReasoning 验证顶层 reasoning 项解析为 assistant 思考块。
func TestParseResponsesTopLevelReasoning(t *testing.T) {
	raw := []byte(`{"model":"mimo-v2.5","input":[
		{"role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think hard"}]},
		{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{}"}
	],"stream":true}`)
	req, err := parseOpenAIResponsesRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	// user / assistant(thinking) / assistant(tool_use)
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3: %+v", len(req.Messages), req.Messages)
	}
	m1 := req.Messages[1]
	if m1.Role != "assistant" || len(m1.Content) != 1 || m1.Content[0].Type != "thinking" || m1.Content[0].Thinking != "think hard" {
		t.Fatalf("messages[1] = %+v, want assistant thinking", m1)
	}
	m2 := req.Messages[2]
	if m2.Role != "assistant" || len(m2.Content) != 1 || m2.Content[0].Type != "tool_use" {
		t.Fatalf("messages[2] = %+v, want assistant tool_use", m2)
	}
}
