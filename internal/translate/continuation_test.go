package translate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func continuationMeta(history ...string) *Request {
	meta := &Request{Tools: []Tool{{Type: "function", Name: "probe", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	for _, text := range history {
		meta.Messages = append(meta.Messages, Message{Role: "assistant", Text: text})
	}
	return meta
}

func TestPreludeContinuationGuard(t *testing.T) {
	meta := continuationMeta()
	for _, text := range []string{
		"现在让我读取下一份日志。",
		"接下来我会调用第三个工具。",
		"Now let me inspect the next result.",
	} {
		if !shouldContinueAfterPrelude(text, meta) {
			t.Errorf("应识别未完成过渡句: %q", text)
		}
	}
	for _, text := range []string{
		"检查已经完成，结果正常。",
		"如果还需要帮助，请随时告诉我。",
		"Let me know if you need anything else.",
		strings.Repeat("长", 501),
	} {
		if shouldContinueAfterPrelude(text, meta) {
			t.Errorf("不应续跑正常最终回答: %q", text)
		}
	}

	limited := continuationMeta(
		"现在让我读取第一项。",
		"接下来我会检查第二项。",
		"下一步我来测试第三项。",
	)
	if shouldContinueAfterPrelude("现在让我读取第四项。", limited) {
		t.Fatal("连续过渡句救援超过三次，可能形成无限循环")
	}
	reset := continuationMeta(
		"现在让我读取第一项。",
		"接下来我会检查第二项。",
		"下一步我来测试第三项。",
	)
	reset.Messages = append(reset.Messages, Message{Role: "user", Text: "新问题"})
	if !shouldContinueAfterPrelude("现在让我读取新问题。", reset) {
		t.Fatal("新的用户轮次应重置过渡句救援次数")
	}
}

func TestPreludeContinuationCountsAcrossToolResults(t *testing.T) {
	meta := &Request{Tools: []Tool{{Type: "function", Name: "probe", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	meta.Messages = append(meta.Messages, Message{Role: "user", Text: "Run the batch."})
	for i := 1; i <= 3; i++ {
		meta.Messages = append(meta.Messages,
			Message{Role: "assistant", Text: fmt.Sprintf("现在让我读取第%d项。", i)},
			Message{Role: "tool", ToolCallID: fmt.Sprintf("call_%d", i), Text: "result"},
		)
	}
	if shouldContinueAfterPrelude("现在让我读取第四项。", meta) {
		t.Fatal("同一用户轮次的救援次数超过三次")
	}

	meta.Messages = append(meta.Messages, Message{Role: "user", Text: "新问题"})
	if !shouldContinueAfterPrelude("现在让我读取新问题。", meta) {
		t.Fatal("新的用户消息应重置救援计数")
	}
}

func TestResponsesStreamPreludeRequestsFollowUp(t *testing.T) {
	meta := continuationMeta()
	conv := NewStreamConverter(FormatOpenAICompletions, FormatOpenAIResponses, "mimo-v2.5", meta)
	lines := []string{
		`data: {"choices":[{"delta":{"content":"现在让我读取下一份日志。"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		`data: [DONE]`,
	}
	var events []map[string]any
	for _, line := range lines {
		chunks, _ := conv.Feed([]byte(line))
		for _, chunk := range chunks {
			payload := strings.TrimSpace(strings.TrimPrefix(string(chunk), "data:"))
			var event map[string]any
			if json.Unmarshal([]byte(payload), &event) == nil {
				events = append(events, event)
			}
		}
	}

	var commentary, followUp bool
	for _, event := range events {
		if event["type"] == "response.output_item.done" {
			item, _ := event["item"].(map[string]any)
			commentary = item["type"] == "message" && item["phase"] == "commentary"
		}
		if event["type"] == "response.completed" {
			response, _ := event["response"].(map[string]any)
			value, exists := response["end_turn"]
			followUp = exists && value == false
		}
	}
	if !commentary || !followUp {
		t.Fatalf("过渡句应输出 commentary + end_turn:false: commentary=%v followUp=%v events=%v", commentary, followUp, events)
	}
}

func TestResponsesStreamFinalAnswerDoesNotForceFollowUp(t *testing.T) {
	meta := continuationMeta()
	conv := NewStreamConverter(FormatOpenAICompletions, FormatOpenAIResponses, "mimo-v2.5", meta)
	for _, line := range []string{
		`data: {"choices":[{"delta":{"content":"检查完成，所有结果正常。"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
	} {
		chunks, _ := conv.Feed([]byte(line))
		for _, chunk := range chunks {
			payload := strings.TrimSpace(strings.TrimPrefix(string(chunk), "data:"))
			var event map[string]any
			if json.Unmarshal([]byte(payload), &event) != nil || event["type"] != "response.completed" {
				continue
			}
			response, _ := event["response"].(map[string]any)
			if _, exists := response["end_turn"]; exists {
				t.Fatalf("正常最终回答不应携带 end_turn:false: %v", response)
			}
		}
	}
}
