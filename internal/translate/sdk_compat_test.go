package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStreamCompletionsToResponsesSDKCompatible 模拟 @ai-sdk/openai 的严格解析：
// 1) 任何 delta 事件必须引用已通过 output_item.added 声明的 item_id
// 2) item 的 output_index 必须唯一且递增
// 3) 每个 item 结束要有 output_item.done
// 4) 流结束要有 response.completed（含 usage）
// 上游：completions 流式（mimo 风格：第一块直接 reasoning_content，随后 content）
func TestStreamCompletionsToResponsesSDKCompatible(t *testing.T) {
	conv := NewStreamConverter(FormatOpenAICompletions, FormatOpenAIResponses, "mimo-v2.5")
	lines := [][]byte{
		[]byte(`data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think1"},"finish_reason":null}]}`),
		[]byte(`data: {"id":"x","choices":[{"index":0,"delta":{"reasoning_content":"think2"},"finish_reason":null}]}`),
		[]byte(`data: {"id":"x","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`),
		[]byte(`data: {"id":"x","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`),
		[]byte(`data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		[]byte(`data: {"id":"x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`),
		[]byte(`data: [DONE]`),
	}
	var events []map[string]any
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
				events = append(events, ev)
			}
		}
	}
	if !done {
		for _, c := range conv.Close() {
			payload := strings.TrimSpace(strings.TrimPrefix(string(c), "data:"))
			var ev map[string]any
			if json.Unmarshal([]byte(payload), &ev) == nil {
				events = append(events, ev)
			}
		}
	}

	// 模拟 AI SDK 状态机
	declared := map[string]bool{}
	usedIndex := map[int]bool{}
	closedItems := map[string]bool{}
	var parseErr []string
	for _, ev := range events {
		typ, _ := ev["type"].(string)
		switch typ {
		case "response.output_item.added":
			item, _ := ev["item"].(map[string]any)
			id, _ := item["id"].(string)
			declared[id] = true
			oi, _ := ev["output_index"].(float64)
			idx := int(oi)
			if usedIndex[idx] {
				parseErr = append(parseErr, "重复 output_index="+string(rune('0'+idx)))
			}
			usedIndex[idx] = true
		case "response.reasoning_text.delta", "response.output_text.delta", "response.function_call_arguments.delta":
			itemID, _ := ev["item_id"].(string)
			if !declared[itemID] {
				parseErr = append(parseErr, typ+" 引用未声明 item_id="+itemID)
			}
		case "response.output_item.done":
			item, _ := ev["item"].(map[string]any)
			id, _ := item["id"].(string)
			closedItems[id] = true
		}
	}
	if len(parseErr) > 0 {
		t.Fatalf("SDK 解析失败: %v\n完整事件: %s", parseErr, dumpEvents(events))
	}
	// 至少有一个 message item 被正确闭合
	if len(closedItems) == 0 {
		t.Errorf("没有任何 output_item.done 事件: %s", dumpEvents(events))
	}
	// 必须包含文本增量
	hasText := false
	for _, ev := range events {
		if ev["type"] == "response.output_text.delta" {
			hasText = true
		}
	}
	if !hasText {
		t.Errorf("缺少 output_text.delta: %s", dumpEvents(events))
	}
	// 必须包含 reasoning 增量（mimo 思考）
	hasReason := false
	for _, ev := range events {
		if ev["type"] == "response.reasoning_text.delta" {
			hasReason = true
		}
	}
	if !hasReason {
		t.Errorf("缺少 reasoning_text.delta: %s", dumpEvents(events))
	}
	// 必须有 completed 且 usage 完整
	var lastResp map[string]any
	for _, ev := range events {
		if ev["type"] == "response.completed" {
			r, _ := ev["response"].(map[string]any)
			lastResp = r
		}
	}
	if lastResp == nil {
		t.Fatalf("缺少 response.completed: %s", dumpEvents(events))
	}
	u, _ := lastResp["usage"].(map[string]any)
	if u == nil || u["input_tokens"] == nil || u["output_tokens"] == nil {
		t.Errorf("completed 缺 usage: %v", lastResp)
	}
}

func dumpEvents(events []map[string]any) string {
	var sb strings.Builder
	for _, ev := range events {
		b, _ := json.Marshal(ev)
		sb.Write(b)
		sb.WriteString("\n")
	}
	return sb.String()
}
