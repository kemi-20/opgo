package meter

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

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

func TestCostUnitsCachedWrite(t *testing.T) {
	p := config.ModelPricing{
		InputPerMillion:       1.60,
		OutputPerMillion:      7.20,
		CachedReadPerMillion:  0.16,
		CachedWritePerMillion: 2.00,
	}
	u := Usage{
		PromptTokens:      1000,
		CompletionTokens:  100,
		CachedTokens:      200,
		CachedWriteTokens: 300,
	}
	// 普通输入 500*1.60 + 缓存读 200*0.16 + 缓存写 300*2.00 + 输出 100*7.20
	// = $0.002152，按 1e8 微元计为 215200。
	if got := CostUnits(p, u); got != 215200 {
		t.Errorf("cost = %d, want 215200", got)
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
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"prompt_tokens_details":{"cached_tokens":10,"cache_write_tokens":5},"completion_tokens_details":{"reasoning_tokens":20}}}`)
	u, ok := ParseBodyUsage(body)
	if !ok {
		t.Fatal("应解析出 usage")
	}
	if u.PromptTokens != 100 || u.CompletionTokens != 50 || u.TotalTokens != 150 || u.CachedTokens != 10 || u.CachedWriteTokens != 5 || u.ReasoningTokens != 20 {
		t.Errorf("usage = %+v", u)
	}
}

func TestParseBodyUsageResponsesCacheWrite(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,"input_tokens_details":{"cached_tokens":10,"cache_write_tokens":20},"output_tokens_details":{"reasoning_tokens":30}}}`)
	u, ok := ParseBodyUsage(body)
	if !ok {
		t.Fatal("应解析出 Responses usage")
	}
	if u.PromptTokens != 100 || u.CompletionTokens != 50 || u.TotalTokens != 150 || u.CachedTokens != 10 || u.CachedWriteTokens != 20 || u.ReasoningTokens != 30 {
		t.Errorf("usage = %+v", u)
	}
}

func TestReasoningTokensNeverBecomeCachedWriteBilling(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,"input_tokens_details":{"cached_tokens":10},"output_tokens_details":{"reasoning_tokens":30}}}`)
	u, ok := ParseBodyUsage(body)
	if !ok {
		t.Fatal("应解析出 Responses usage")
	}
	if u.CachedWriteTokens != 0 {
		t.Fatalf("reasoning_tokens 不得进入缓存写计费，usage=%+v", u)
	}
	if u.ReasoningTokens != 30 {
		t.Fatalf("reasoning_tokens 应独立保留，usage=%+v", u)
	}
	p := config.ModelPricing{InputPerMillion: 1, OutputPerMillion: 2, CachedWritePerMillion: 99}
	// reasoning 已包含在 output_tokens=50 中，只能按输出价计一次。
	if cost := CostUnits(p, u); cost != 19000 {
		t.Fatalf("reasoning 被重复计费，cost=%d want=19000", cost)
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
	if !ok || u.CompletionTokens != 20 || u.TotalTokens != 0 {
		t.Errorf("usage = %+v, ok=%v", u, ok)
	}
}

func TestParseSSEUsageAnthropicDeltaPreservesAllProvidedFields(t *testing.T) {
	line := []byte(`data: {"type":"message_delta","usage":{"input_tokens":12,"output_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":2,"total_tokens":19}}`)
	u, ok := ParseSSEUsage(line)
	if !ok {
		t.Fatal("message_delta usage should parse")
	}
	if u.PromptTokens != 12 || u.CompletionTokens != 7 || u.CachedTokens != 3 || u.CachedWriteTokens != 2 || u.TotalTokens != 19 {
		t.Fatalf("message_delta usage fields lost: %+v", u)
	}
}

func TestParseSSEUsageResponsesCompleted(t *testing.T) {
	line := []byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":3},"output_tokens_details":{"reasoning_tokens":4}}}}`)
	u, ok := ParseSSEUsage(line)
	if !ok || u.PromptTokens != 10 || u.CachedTokens != 2 || u.CachedWriteTokens != 3 || u.ReasoningTokens != 4 {
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

// TestEnsureStreamUsageMinimalInjection 验证字节级最小注入：插入 stream_options 后，
// 图片 base64、字段顺序、数字精度等其余字节完全不变（只允许在 { 后追加一个键）。
func TestEnsureStreamUsageMinimalInjection(t *testing.T) {
	// 模拟带图片的请求：base64 串特意构造可验证的子串，字段顺序固定
	body := []byte(`{"model":"deepseek-v4-flash","stream":true,"temperature":0.14,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="}}]}]}`)
	out, changed := EnsureStreamUsage(body)
	if !changed {
		t.Fatal("应注入")
	}
	// 注入内容出现在开头
	if !strings.HasPrefix(string(out), `{"stream_options":{"include_usage":true},`) {
		t.Errorf("注入应位于对象开头: %s", out)
	}
	// 原 body 的所有字节（去掉开头 { 后）必须按原顺序完整出现
	rest := string(body)[1:]
	if !strings.Contains(string(out), rest) {
		t.Error("注入后原字节被改动（图片 base64/字段顺序/数字精度不应变）")
	}
	// base64 子串精确保留
	if !strings.Contains(string(out), "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg==") {
		t.Error("图片 base64 应原样保留")
	}
	// 数字精度保留
	if !strings.Contains(string(out), `"temperature":0.14`) {
		t.Error("数字精度应原样保留")
	}
	// 注入后仍是合法 JSON 且可解析出模型与图片
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("注入后不是合法 JSON: %v", err)
	}
	if obj["model"] != "deepseek-v4-flash" {
		t.Errorf("model = %v", obj["model"])
	}
	if obj["stream_options"].(map[string]any)["include_usage"] != true {
		t.Error("include_usage 应为 true")
	}
}

// TestEnsureStreamUsagePreservesWhitespace 前导空白保留：{ 前有空格/换行时注入位置正确。
func TestEnsureStreamUsagePreservesWhitespace(t *testing.T) {
	body := []byte("  {\"model\":\"m\",\"stream\":true}")
	out, changed := EnsureStreamUsage(body)
	if !changed {
		t.Fatal("应注入")
	}
	if !strings.HasPrefix(string(out), "  {\"stream_options\":{\"include_usage\":true},") {
		t.Errorf("前导空白应保留: %s", out)
	}
	if !strings.Contains(string(out), "\"model\":\"m\"") {
		t.Error("原字段应保留")
	}
}

func TestIsPeakTime(t *testing.T) {
	windows := [][]string{{"01:00", "04:00"}, {"06:00", "10:00"}}
	// 边界：00:59 off / 01:00 peak / 03:59 peak / 04:00 off
	cases := []struct {
		time string // "HH:MM" UTC
		peak bool
	}{
		{"00:00", false}, {"00:59", false}, {"01:00", true}, {"01:30", true},
		{"03:59", true}, {"04:00", false}, {"05:59", false}, {"06:00", true},
		{"09:59", true}, {"10:00", false}, {"12:00", false}, {"23:59", false},
	}
	for _, c := range cases {
		tm := parseTestTime(c.time)
		got := IsPeakTime(tm, windows)
		if got != c.peak {
			t.Errorf("IsPeakTime(%s) = %v, want %v", c.time, got, c.peak)
		}
	}
}

func TestIsPeakTimeCrossMidnight(t *testing.T) {
	windows := [][]string{{"22:00", "02:00"}}
	if !IsPeakTime(parseTestTime("23:00"), windows) {
		t.Error("23:00 should be peak (cross-midnight window)")
	}
	if !IsPeakTime(parseTestTime("01:00"), windows) {
		t.Error("01:00 should be peak (cross-midnight window)")
	}
	if IsPeakTime(parseTestTime("21:00"), windows) {
		t.Error("21:00 should be off-peak")
	}
	if IsPeakTime(parseTestTime("03:00"), windows) {
		t.Error("03:00 should be off-peak")
	}
}

func TestApplyPeak(t *testing.T) {
	trueVal := true
	falseVal := false
	windows := [][]string{{"01:00", "04:00"}, {"06:00", "10:00"}}
	peak := &config.ModelPeak{Enabled: &trueVal, Multiplier: 2, Windows: windows}
	price := config.ModelPricing{InputPerMillion: 0.44, OutputPerMillion: 1.32, CachedReadPerMillion: 0.014, Peak: peak}
	// 峰时：翻倍
	p := ApplyPeak(price, "deepseek-v4-flash", parseTestTime("02:00"))
	if p.InputPerMillion != 0.88 || p.OutputPerMillion != 2.64 || p.CachedReadPerMillion != 0.028 {
		t.Errorf("peak price not doubled: %+v", p)
	}
	// 谷时：不变
	p = ApplyPeak(price, "deepseek-v4-flash", parseTestTime("12:00"))
	if p.InputPerMillion != 0.44 || p.OutputPerMillion != 1.32 || p.CachedReadPerMillion != 0.014 {
		t.Errorf("off-peak price changed: %+v", p)
	}
	// 未配置 peak 的模型峰时不变
	noPeak := config.ModelPricing{InputPerMillion: 0.44, OutputPerMillion: 1.32, CachedReadPerMillion: 0.014}
	p = ApplyPeak(noPeak, "mimo-v2.5", parseTestTime("02:00"))
	if p.InputPerMillion != 0.44 {
		t.Errorf("model without peak config changed in peak: %+v", p)
	}
	// enabled=false 时不变
	disabled := &config.ModelPeak{Enabled: &falseVal, Multiplier: 2, Windows: windows}
	dPrice := config.ModelPricing{InputPerMillion: 0.44, OutputPerMillion: 1.32, CachedReadPerMillion: 0.014, Peak: disabled}
	p = ApplyPeak(dPrice, "deepseek-v4-flash", parseTestTime("02:00"))
	if p.InputPerMillion != 0.44 {
		t.Errorf("peak disabled but price changed: %+v", p)
	}
	// 未填 multiplier 默认 2 倍
	noMult := &config.ModelPeak{Enabled: &trueVal, Windows: windows}
	mPrice := config.ModelPricing{InputPerMillion: 0.44, OutputPerMillion: 1.32, CachedReadPerMillion: 0.014, Peak: noMult}
	p = ApplyPeak(mPrice, "deepseek-v4-flash", parseTestTime("02:00"))
	if p.InputPerMillion != 0.88 {
		t.Errorf("default multiplier not applied: %+v", p)
	}
}

func parseTestTime(hhmm string) time.Time {
	h, m := 0, 0
	fmt.Sscanf(hhmm, "%d:%d", &h, &m)
	return time.Date(2026, 1, 1, h, m, 0, 0, time.UTC)
}
