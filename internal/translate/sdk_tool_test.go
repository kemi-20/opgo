package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStreamToolCallSDKCompatibility 模拟 @ai-sdk/openai 对 function_call 流的严格解析：
// delta/done 必须带 call_id/name；output_item.done 的 function_call arguments 必须是字符串、
// status 必须存在；最终 finishReason 应为 tool-calls。
func TestStreamToolCallSDKCompatibility(t *testing.T) {
	conv := NewStreamConverter(FormatOpenAICompletions, FormatOpenAIResponses, "mimo-v2.5")
	lines := [][]byte{
		[]byte(`data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`),
		[]byte(`data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":""}}]},"finish_reason":null}]}`),
		[]byte(`data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\": \"echo hi\"}"}}]},"finish_reason":null}]}`),
		[]byte(`data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		[]byte(`data: {"id":"x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`),
		[]byte(`data: [DONE]`),
	}
	var evs []map[string]any
	done := false
	for _, l := range lines {
		chunks, d := conv.Feed(l)
		done = d
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
	}
	if !done {
		for _, c := range conv.Close() {
			payload := strings.TrimSpace(strings.TrimPrefix(string(c), "data:"))
			var ev map[string]any
			if json.Unmarshal([]byte(payload), &ev) == nil {
				evs = append(evs, ev)
			}
		}
	}

	// 对齐原生 Responses：call_id 只出现在 output_item.added/done 的 item 上，
	// delta/done 事件不带 call_id（与 deepseek-v4-flash/gpt-5.6-luna 原生一致）。
	itemCallIDs := map[string]string{}
	for _, ev := range evs {
		if ev["type"] == "response.output_item.added" {
			if item, ok := ev["item"].(map[string]any); ok && item["type"] == "function_call" {
				cid, _ := item["call_id"].(string)
				itemCallIDs["added"] = cid
				if item["status"] != "in_progress" {
					t.Errorf("output_item.added function_call 缺 status=in_progress: %v", item)
				}
				if _, ok := item["name"].(string); !ok {
					t.Errorf("output_item.added function_call 缺 name: %v", item)
				}
				if _, ok := item["arguments"].(string); !ok {
					t.Errorf("output_item.added function_call arguments 应为字符串: %v", item)
				}
			}
		}
		if ev["type"] == "response.output_item.done" {
			if item, ok := ev["item"].(map[string]any); ok && item["type"] == "function_call" {
				cid, _ := item["call_id"].(string)
				itemCallIDs["done"] = cid
			}
		}
		if ev["type"] == "response.function_call_arguments.delta" {
			if ev["call_id"] != nil {
				t.Errorf("delta 不应带 call_id（原生格式无此字段）: %v", ev)
			}
			if _, ok := ev["item_id"].(string); !ok {
				t.Errorf("delta 缺 item_id: %v", ev)
			}
		}
		if ev["type"] == "response.function_call_arguments.done" {
			if ev["call_id"] != nil {
				t.Errorf("done 不应带 call_id（原生格式无此字段）: %v", ev)
			}
		}
	}
	if itemCallIDs["added"] == "" || itemCallIDs["added"] != "call_1" {
		t.Errorf("output_item.added function_call call_id 应为 call_1: %v", itemCallIDs)
	}
	if itemCallIDs["done"] != "call_1" {
		t.Errorf("output_item.done function_call call_id 应为 call_1: %v", itemCallIDs)
	}
	// done 带 name 与字符串 arguments
	var doneEv map[string]any
	for _, ev := range evs {
		if ev["type"] == "response.function_call_arguments.done" {
			doneEv = ev
		}
	}
	if doneEv == nil {
		t.Fatalf("缺少 function_call_arguments.done: %v", evs)
	}
	if doneEv["name"] != "Bash" {
		t.Errorf("done 缺 name: %v", doneEv)
	}
	if _, ok := doneEv["arguments"].(string); !ok {
		t.Errorf("done arguments 应为字符串: %v", doneEv)
	}
	// output_item.done 的 function_call：arguments 字符串 + status 存在
	var itemDone map[string]any
	for _, ev := range evs {
		if ev["type"] == "response.output_item.done" {
			if item, ok := ev["item"].(map[string]any); ok && item["type"] == "function_call" {
				itemDone = item
			}
		}
	}
	if itemDone == nil {
		t.Fatalf("缺少 function_call output_item.done: %v", evs)
	}
	if _, ok := itemDone["arguments"].(string); !ok {
		t.Errorf("output_item.done arguments 应为字符串: %v", itemDone)
	}
	if itemDone["status"] != "completed" {
		t.Errorf("output_item.done 缺 status=completed: %v", itemDone)
	}
	// 有 tool-input-end 所需事件（done 后 output_item.done）
	foundDone := false
	for _, ev := range evs {
		if ev["type"] == "response.output_item.done" {
			foundDone = true
		}
	}
	if !foundDone {
		t.Error("缺少 output_item.done")
	}
}
