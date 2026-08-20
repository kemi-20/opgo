package translate

import (
	"encoding/json"
	"testing"
)

func TestResponsesToolMediaBecomesTextToolAndUserAttachments(t *testing.T) {
	raw := []byte(`{
		"model":"mimo-v2.5","stream":true,
		"input":[
			{"type":"function_call","call_id":"call_1","name":"inspect_media","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":[
				{"type":"input_text","text":"screen captured"},
				{"type":"input_image","image_url":"data:image/png;base64,AAAA"},
				{"type":"input_audio","input_audio":{"data":"BBBB","format":"wav"}}
			]}
		]
	}`)
	out, err := ConvertRequest(FormatOpenAIResponses, FormatOpenAICompletions, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj.Messages) != 3 {
		t.Fatalf("messages=%d, want assistant/tool/user: %s", len(obj.Messages), out)
	}
	tool := obj.Messages[1]
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Fatalf("tool message=%v", tool)
	}
	if text, ok := tool["content"].(string); !ok || text != "screen captured" {
		t.Fatalf("role=tool content 必须是纯文本，got %#v", tool["content"])
	}

	user := obj.Messages[2]
	if user["role"] != "user" {
		t.Fatalf("附件消息 role=%v, want user", user["role"])
	}
	parts, ok := user["content"].([]any)
	if !ok {
		t.Fatalf("附件 user content=%#v, want parts array", user["content"])
	}
	var sawLabel, sawImage, sawAudio bool
	for _, item := range parts {
		part, _ := item.(map[string]any)
		switch part["type"] {
		case "text":
			if part["text"] == "Tool output attachments for call call_1:" {
				sawLabel = true
			}
		case "image_url":
			sawImage = true
		case "input_audio":
			sawAudio = true
		}
	}
	if !sawLabel || !sawImage || !sawAudio {
		t.Fatalf("多模态工具结果未完整保留 label=%v image=%v audio=%v: %s", sawLabel, sawImage, sawAudio, out)
	}
}

func TestToolMediaOnlyStillHasValidTextToolResult(t *testing.T) {
	raw := []byte(`{
		"model":"hy3","input":[
			{"type":"function_call","call_id":"call_img","name":"screenshot","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_img","output":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}
		]
	}`)
	out, err := ConvertRequest(FormatOpenAIResponses, FormatOpenAICompletions, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj.Messages) != 3 {
		t.Fatalf("messages=%d: %s", len(obj.Messages), out)
	}
	if content, ok := obj.Messages[1]["content"].(string); !ok || content == "" {
		t.Fatalf("纯媒体工具结果也必须给 role=tool 合法文本占位，got %#v", obj.Messages[1]["content"])
	}
}

func TestToolMediaMergesWithFollowingUserMessage(t *testing.T) {
	req := &Request{
		Model: "mimo-v2.5",
		Messages: []Message{
			{Role: "assistant", Content: []Block{{Type: "tool_use", ToolUseID: "call_1", Name: "screen", Input: json.RawMessage(`{}`)}}},
			{Role: "tool", ToolCallID: "call_1", Content: []Block{{Type: "image", ImageURL: "data:image/png;base64,AAAA"}}},
			{Role: "user", Text: "continue"},
		},
	}
	out, err := buildOpenAICompletionsRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var obj struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj.Messages) != 3 {
		t.Fatalf("附件应并入后续 user，messages=%d: %s", len(obj.Messages), out)
	}
	parts, ok := obj.Messages[2]["content"].([]any)
	if !ok {
		t.Fatalf("user content=%#v", obj.Messages[2]["content"])
	}
	var sawImage, sawContinue bool
	for _, item := range parts {
		part, _ := item.(map[string]any)
		if part["type"] == "image_url" {
			sawImage = true
		}
		if part["type"] == "text" && part["text"] == "continue" {
			sawContinue = true
		}
	}
	if !sawImage || !sawContinue {
		t.Fatalf("附件或后续 user 文本丢失: %s", out)
	}
}
