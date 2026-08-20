package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesStreamIDsAreNativeStyleUniqueAndStable(t *testing.T) {
	first := collectResponsesStreamIdentity(t)
	second := collectResponsesStreamIdentity(t)

	for _, got := range []responsesIdentity{first, second} {
		assertResponsesID(t, got.responseID, "resp")
		assertResponsesID(t, got.reasoningID, "rs")
		assertResponsesID(t, got.messageID, "msg")
		assertResponsesID(t, got.functionID, "fc")
		if got.callID != "call_upstream" {
			t.Fatalf("call_id 未保留上游值: %q", got.callID)
		}
	}

	for label, pair := range map[string][2]string{
		"response":  {first.responseID, second.responseID},
		"reasoning": {first.reasoningID, second.reasoningID},
		"message":   {first.messageID, second.messageID},
		"function":  {first.functionID, second.functionID},
	} {
		if pair[0] == pair[1] {
			t.Errorf("两个独立 Responses 请求复用了 %s ID: %q", label, pair[0])
		}
	}
}

func TestResponsesStreamMissingCallIDGetsUniqueNativeFallback(t *testing.T) {
	getCallID := func() string {
		conv := NewStreamConverter(FormatOpenAICompletions, FormatOpenAIResponses, "hy3")
		lines := []string{
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"exec_command","arguments":"{}"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}
		for _, line := range lines {
			chunks, _ := conv.Feed([]byte(line))
			for _, ev := range decodeResponsesEvents(t, chunks) {
				if item, ok := ev["item"].(map[string]any); ok && item["type"] == "function_call" {
					if callID, _ := item["call_id"].(string); callID != "" {
						return callID
					}
				}
			}
		}
		t.Fatal("未生成 function_call")
		return ""
	}

	first, second := getCallID(), getCallID()
	assertResponsesID(t, first, "call")
	assertResponsesID(t, second, "call")
	if first == second {
		t.Fatalf("缺失上游 call_id 时复用了回退值: %q", first)
	}
}

func TestResponsesNonStreamIDsDoNotReuseChatCompletionID(t *testing.T) {
	raw := []byte(`{
		"id":"chatcmpl-reused","object":"chat.completion","model":"hy3","created":123,
		"choices":[{"index":0,"message":{"role":"assistant","content":"checking","reasoning_content":"think","tool_calls":[{"id":"call_upstream","type":"function","function":{"name":"exec_command","arguments":"{}"}}]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
	}`)

	readIDs := func() (string, map[string]string) {
		out, err := ConvertResponse(FormatOpenAIResponses, FormatOpenAICompletions, raw)
		if err != nil {
			t.Fatal(err)
		}
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatal(err)
		}
		ids := map[string]string{}
		for _, rawItem := range obj["output"].([]any) {
			item := rawItem.(map[string]any)
			ids[item["type"].(string)] = item["id"].(string)
		}
		return obj["id"].(string), ids
	}

	resp1, ids1 := readIDs()
	resp2, ids2 := readIDs()
	assertResponsesID(t, resp1, "resp")
	assertResponsesID(t, ids1["reasoning"], "rs")
	assertResponsesID(t, ids1["message"], "msg")
	assertResponsesID(t, ids1["function_call"], "fc")
	if resp1 == resp2 {
		t.Fatalf("非流式转换复用了 response ID: %q", resp1)
	}
	for kind, id1 := range ids1 {
		if id1 == ids2[kind] {
			t.Errorf("非流式转换复用了 %s item ID: %q", kind, id1)
		}
	}
}

type responsesIdentity struct {
	responseID  string
	reasoningID string
	messageID   string
	functionID  string
	callID      string
}

func collectResponsesStreamIdentity(t *testing.T) responsesIdentity {
	t.Helper()
	conv := NewStreamConverter(FormatOpenAICompletions, FormatOpenAIResponses, "hy3")
	lines := []string{
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"think"},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"checking"},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_upstream","type":"function","function":{"name":"exec_command","arguments":"{}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		`data: [DONE]`,
	}
	var events []map[string]any
	for _, line := range lines {
		chunks, _ := conv.Feed([]byte(line))
		events = append(events, decodeResponsesEvents(t, chunks)...)
	}
	events = append(events, decodeResponsesEvents(t, conv.Close())...)

	var got responsesIdentity
	knownItems := map[string]bool{}
	for _, ev := range events {
		if resp, ok := ev["response"].(map[string]any); ok {
			id, _ := resp["id"].(string)
			if got.responseID == "" {
				got.responseID = id
			} else if id != got.responseID {
				t.Errorf("同一事件流 response ID 发生变化: %q -> %q", got.responseID, id)
			}
			if output, ok := resp["output"].([]any); ok {
				for _, rawItem := range output {
					item := rawItem.(map[string]any)
					rememberResponsesItem(t, &got, knownItems, item)
				}
			}
		}
		if item, ok := ev["item"].(map[string]any); ok {
			rememberResponsesItem(t, &got, knownItems, item)
		}
		if itemID, ok := ev["item_id"].(string); ok && itemID != "" {
			if !knownItems[itemID] {
				t.Errorf("事件引用了未通过 output_item.added 声明的 item_id: %q", itemID)
			}
		}
	}
	if got.responseID == "" || got.reasoningID == "" || got.messageID == "" || got.functionID == "" {
		t.Fatalf("未收集到完整 Responses 身份: %+v", got)
	}
	return got
}

func rememberResponsesItem(t *testing.T, got *responsesIdentity, known map[string]bool, item map[string]any) {
	t.Helper()
	id, _ := item["id"].(string)
	kind, _ := item["type"].(string)
	if id == "" || kind == "" {
		return
	}
	known[id] = true
	var slot *string
	switch kind {
	case "reasoning":
		slot = &got.reasoningID
	case "message":
		slot = &got.messageID
	case "function_call":
		slot = &got.functionID
		if callID, _ := item["call_id"].(string); callID != "" {
			got.callID = callID
		}
	default:
		return
	}
	if *slot == "" {
		*slot = id
	} else if *slot != id {
		t.Errorf("同一 %s item 在事件间改变 ID: %q -> %q", kind, *slot, id)
	}
}

func decodeResponsesEvents(t *testing.T, chunks [][]byte) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, chunk := range chunks {
		payload := strings.TrimSpace(strings.TrimPrefix(string(chunk), "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("非法 SSE JSON: %v\n%s", err, payload)
		}
		events = append(events, ev)
	}
	return events
}

func assertResponsesID(t *testing.T, id, prefix string) {
	t.Helper()
	wantPrefix := prefix + "_"
	if !strings.HasPrefix(id, wantPrefix) || len(id) <= len(wantPrefix) {
		t.Errorf("ID %q 不符合原生 %s_* 风格", id, prefix)
	}
	if strings.Contains(strings.TrimPrefix(id, wantPrefix), "-") {
		t.Errorf("ID %q 应使用原生风格的不透明连续标识", id)
	}
}
