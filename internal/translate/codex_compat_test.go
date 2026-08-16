package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStreamCompletionsToResponsesCodexCompat 模拟真实上游 completions 流式响应
// （tool_calls + finish_reason + usage + [DONE]），检查 Codex 客户端所需格式：
// 1) response.completed 只能出现 1 次
// 2) output_item.added 的 function_call item 必须带 status: in_progress（与原生一致）
func TestStreamCompletionsToResponsesCodexCompat(t *testing.T) {
	sc := NewStreamConverter(FormatOpenAICompletions, FormatOpenAIResponses, "mimo-v2.5")
	lines := []string{
		`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\": \"Be"}}]},"finish_reason":null}]}`,
		`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ijing\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"id":"x","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		`data: [DONE]`,
	}
	var all [][]byte
	for _, l := range lines {
		out, done := sc.Feed([]byte(l))
		all = append(all, out...)
		if done {
			break
		}
	}
	all = append(all, sc.Close()...)
	// 统计
	completed := 0
	addedStatus := map[string]bool{}
	for _, b := range all {
		line := string(b)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		switch ev["type"] {
		case "response.completed":
			completed++
		case "response.output_item.added":
			if item, ok := ev["item"].(map[string]any); ok {
				addedStatus[item["type"].(string)] = item["status"] != nil
			}
		}
	}
	t.Logf("response.completed count = %d", completed)
	t.Logf("output_item.added status presence: %v", addedStatus)
	if completed != 1 {
		t.Errorf("response.completed emitted %d times, want 1", completed)
	}
	if addedStatus["function_call"] != true {
		t.Errorf("function_call output_item.added missing status:in_progress, got %v", addedStatus)
	}
	// 打印全部事件便于核对
	for _, b := range all {
		t.Logf("EVT: %s", strings.TrimSpace(string(b)))
	}
}
