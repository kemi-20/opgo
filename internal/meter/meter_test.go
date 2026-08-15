package meter

import (
	"strings"
	"testing"

	"opgo/internal/config"
)

func TestCostUnits(t *testing.T) {
	p := config.ModelPricing{InputPerMillion: 1, OutputPerMillion: 4, CachedReadPerMillion: 0.1}
	u := Usage{PromptTokens: 1000, CompletionTokens: 500}
	// (1000/1e6*1 + 500/1e6*4) * 1e8 = (0.001 + 0.002)*1e8 = 300000
	if got := CostUnits(p, u); got != 300000 {
		t.Errorf("cost = %d, want 300000", got)
	}
}

func TestCostUnitsCached(t *testing.T) {
	p := config.ModelPricing{InputPerMillion: 1, OutputPerMillion: 4, CachedReadPerMillion: 0.1}
	u := Usage{PromptTokens: 1000, CompletionTokens: 500, CachedTokens: 400}
	// (600/1e6*1 + 400/1e6*0.1 + 500/1e6*4)*1e8 = (0.0006+0.00004+0.002)*1e8 = 264000
	if got := CostUnits(p, u); got != 264000 {
		t.Errorf("cost = %d, want 264000", got)
	}
}

func TestPrecisionOneThirdCent(t *testing.T) {
	p := config.ModelPricing{InputPerMillion: 0.33333333}
	u := Usage{PromptTokens: 3000000}
	// 3 * 0.33333333 = 0.99999999 -> 99999999 units
	if got := CostUnits(p, u); got != 99999999 {
		t.Errorf("cost = %d, want 99999999", got)
	}
}

func TestParseBodyUsageOpenAI(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"prompt_tokens_details":{"cached_tokens":10}}}`)
	u, ok := ParseBodyUsage(body)
	if !ok {
		t.Fatal("应解析出 usage")
	}
	if u.PromptTokens != 100 || u.CompletionTokens != 50 || u.TotalTokens != 150 || u.CachedTokens != 10 {
		t.Errorf("usage = %+v", u)
	}
}

func TestParseBodyUsageAnthropic(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":80,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":5}}`)
	u, ok := ParseBodyUsage(body)
	if !ok {
		t.Fatal("应解析出 usage")
	}
	if u.PromptTokens != 80 || u.CompletionTokens != 20 || u.CachedTokens != 30 || u.CachedWriteTokens != 5 {
		t.Errorf("usage = %+v", u)
	}
}

func TestParseBodyUsageNone(t *testing.T) {
	if _, ok := ParseBodyUsage([]byte(`{"choices":[]}`)); ok {
		t.Error("无 usage 应返回 false")
	}
}

func TestParseSSEUsageOpenAIChunk(t *testing.T) {
	line := []byte(`data: {"id":"x","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`)
	u, ok := ParseSSEUsage(line)
	if !ok || u.CompletionTokens != 50 {
		t.Errorf("usage = %+v, ok=%v", u, ok)
	}
}

func TestParseSSEUsageOpenAINull(t *testing.T) {
	line := []byte(`data: {"id":"x","choices":[{"delta":{"content":"hi"}}],"usage":null}`)
	if _, ok := ParseSSEUsage(line); ok {
		t.Error("usage=null 应返回 false")
	}
}

func TestParseSSEUsageAnthropicStart(t *testing.T) {
	line := []byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":80,"output_tokens":0,"cache_read_input_tokens":30}}}`)
	u, ok := ParseSSEUsage(line)
	if !ok || u.PromptTokens != 80 || u.CachedTokens != 30 {
		t.Errorf("usage = %+v, ok=%v", u, ok)
	}
}

func TestParseSSEUsageAnthropicDelta(t *testing.T) {
	line := []byte(`data: {"type":"message_delta","usage":{"output_tokens":20}}`)
	u, ok := ParseSSEUsage(line)
	if !ok || u.CompletionTokens != 20 {
		t.Errorf("usage = %+v, ok=%v", u, ok)
	}
}

func TestParseSSEUsageResponsesCompleted(t *testing.T) {
	line := []byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":2}}}}`)
	u, ok := ParseSSEUsage(line)
	if !ok || u.PromptTokens != 10 || u.CachedTokens != 2 {
		t.Errorf("usage = %+v, ok=%v", u, ok)
	}
}

func TestParseSSEUsageDone(t *testing.T) {
	if _, ok := ParseSSEUsage([]byte("data: [DONE]")); ok {
		t.Error("[DONE] 应返回 false")
	}
}

func TestRequestModel(t *testing.T) {
	if got := RequestModel([]byte(`{"model":"mimo-v2.5","messages":[]}`)); got != "mimo-v2.5" {
		t.Errorf("model = %q", got)
	}
	if got := RequestModel([]byte(`{}`)); got != "" {
		t.Errorf("model = %q", got)
	}
}

func TestEnsureStreamUsage(t *testing.T) {
	body := []byte(`{"model":"m","stream":true,"messages":[]}`)
	out, changed := EnsureStreamUsage(body)
	if !changed {
		t.Fatal("应注入 include_usage")
	}
	if !strings.Contains(string(out), "include_usage") {
		t.Errorf("注入失败: %s", out)
	}
	// 已设置时不重复注入
	if _, changed := EnsureStreamUsage(out); changed {
		t.Error("已设置时不应再注入")
	}
	// 非流式不注入
	if _, changed := EnsureStreamUsage([]byte(`{"model":"m","messages":[]}`)); changed {
		t.Error("非流式不应注入")
	}
}
