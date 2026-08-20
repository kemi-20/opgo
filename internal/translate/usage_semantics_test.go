package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesUsageSeparatesReasoningAndCacheWrite(t *testing.T) {
	raw := []byte(`{
		"id":"resp_x","model":"gpt-5.6-luna","status":"completed",
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],
		"usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,
			"input_tokens_details":{"cached_tokens":10,"cache_write_tokens":20},
			"output_tokens_details":{"reasoning_tokens":30}}
	}`)

	anthropic, err := ConvertResponse(FormatAnthropic, FormatOpenAIResponses, raw)
	if err != nil {
		t.Fatal(err)
	}
	var anth map[string]any
	if err := json.Unmarshal(anthropic, &anth); err != nil {
		t.Fatal(err)
	}
	u := anth["usage"].(map[string]any)
	if u["cache_creation_input_tokens"] != float64(20) {
		t.Fatalf("cache_creation_input_tokens=%v, want 20: %s", u["cache_creation_input_tokens"], anthropic)
	}
	if strings.Contains(string(anthropic), `"cache_creation_input_tokens":30`) {
		t.Fatalf("reasoning_tokens 不得伪装成 Anthropic cache creation: %s", anthropic)
	}

	chat, err := ConvertResponse(FormatOpenAICompletions, FormatOpenAIResponses, raw)
	if err != nil {
		t.Fatal(err)
	}
	var completion map[string]any
	if err := json.Unmarshal(chat, &completion); err != nil {
		t.Fatal(err)
	}
	cu := completion["usage"].(map[string]any)
	promptDetails := cu["prompt_tokens_details"].(map[string]any)
	completionDetails := cu["completion_tokens_details"].(map[string]any)
	if promptDetails["cache_write_tokens"] != float64(20) || completionDetails["reasoning_tokens"] != float64(30) {
		t.Fatalf("Responses→Completions usage 语义混用: %s", chat)
	}
}

func TestAnthropicCacheWriteDoesNotBecomeResponsesReasoning(t *testing.T) {
	raw := []byte(`{
		"id":"msg_x","model":"x","stop_reason":"end_turn",
		"content":[{"type":"text","text":"ok"}],
		"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":10,"cache_creation_input_tokens":20}
	}`)
	out, err := ConvertResponse(FormatOpenAIResponses, FormatAnthropic, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	u := obj["usage"].(map[string]any)
	inDetails := u["input_tokens_details"].(map[string]any)
	outDetails := u["output_tokens_details"].(map[string]any)
	if inDetails["cache_write_tokens"] != float64(20) {
		t.Fatalf("cache_write_tokens=%v, want 20: %s", inDetails["cache_write_tokens"], out)
	}
	if outDetails["reasoning_tokens"] != float64(0) {
		t.Fatalf("Anthropic cache creation 不得变成 reasoning_tokens: %s", out)
	}
}

func TestResponsesStreamUsageSeparatesReasoningAndCacheWrite(t *testing.T) {
	conv := NewStreamConverter(FormatOpenAIResponses, FormatAnthropic, "gpt-5.6-luna")
	lines := [][]byte{
		[]byte(`data: {"type":"response.output_text.delta","delta":"ok"}`),
		[]byte(`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,"input_tokens_details":{"cached_tokens":10,"cache_write_tokens":20},"output_tokens_details":{"reasoning_tokens":30}}}}`),
	}
	var all []byte
	for _, line := range lines {
		chunks, _ := conv.Feed(line)
		for _, chunk := range chunks {
			all = append(all, chunk...)
		}
	}
	all = append(all, joinChunks(conv.Close())...)
	if !strings.Contains(string(all), `"cache_creation_input_tokens":20`) {
		t.Fatalf("流式 cache write 丢失: %s", all)
	}
	if strings.Contains(string(all), `"cache_creation_input_tokens":30`) {
		t.Fatalf("流式 reasoning 被误写成 cache creation: %s", all)
	}
}

func TestAnthropicStreamMergesStartAndDeltaUsage(t *testing.T) {
	conv := NewStreamConverter(FormatAnthropic, FormatOpenAIResponses, "x")
	lines := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":0,"cache_read_input_tokens":10,"cache_creation_input_tokens":20}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":50}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}
	var all []byte
	for _, line := range lines {
		chunks, _ := conv.Feed(line)
		for _, chunk := range chunks {
			all = append(all, chunk...)
		}
	}
	all = append(all, joinChunks(conv.Close())...)

	var completed map[string]any
	for _, line := range strings.Split(string(all), "\n") {
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(payload), &event) == nil && event["type"] == "response.completed" {
			completed = event
		}
	}
	if completed == nil {
		t.Fatalf("缺少 response.completed: %s", all)
	}
	response := completed["response"].(map[string]any)
	u := response["usage"].(map[string]any)
	inDetails := u["input_tokens_details"].(map[string]any)
	outDetails := u["output_tokens_details"].(map[string]any)
	if u["input_tokens"] != float64(100) || u["output_tokens"] != float64(50) || u["total_tokens"] != float64(150) {
		t.Fatalf("Anthropic 分段 usage 未正确合并: %v", u)
	}
	if inDetails["cached_tokens"] != float64(10) || inDetails["cache_write_tokens"] != float64(20) {
		t.Fatalf("Anthropic cache usage 未正确保留: %v", inDetails)
	}
	if outDetails["reasoning_tokens"] != float64(0) {
		t.Fatalf("cache write 不得变成 reasoning: %v", outDetails)
	}
}

func joinChunks(chunks [][]byte) []byte {
	var out []byte
	for _, chunk := range chunks {
		out = append(out, chunk...)
	}
	return out
}
