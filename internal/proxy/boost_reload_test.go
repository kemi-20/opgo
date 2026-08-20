package proxy

import (
	"testing"

	"opgo/internal/config"
	"opgo/internal/meter"
)

func TestBoostStateInvalidatesOnConfigHotReload(t *testing.T) {
	tracker := newBoostTracker()
	c1 := &config.Config{Boost: boostCfg()}
	tracker.mark("u", "5h", 123, 3.6, 1, c1, 2.4)
	if limit, level, ok := tracker.state("u", "5h", 123, c1, 2.4); !ok || limit != 3.6 || level != 1 {
		t.Fatalf("初始提额状态 = %v/%v/%v", limit, level, ok)
	}

	// Manager.Reload 每次成功都会生成并发布一个新的 *Config。即使只有
	// boost_percent 之外的配置变化，旧提额也必须按安全策略失效。
	c2 := *c1
	c2.Boost.BoostPercent = 120
	if _, _, ok := tracker.state("u", "5h", 123, &c2, 2.4); ok {
		t.Fatal("热更新后旧提额状态仍然生效")
	}
}

func TestMergeUsageCompletesAnthropicSplitUsageAndReasoning(t *testing.T) {
	var acc meter.Usage
	mergeUsage(&acc, meter.Usage{PromptTokens: 80, CachedTokens: 30, TotalTokens: 80})
	mergeUsage(&acc, meter.Usage{CompletionTokens: 20, ReasoningTokens: 6})
	if acc.PromptTokens != 80 || acc.CompletionTokens != 20 || acc.TotalTokens != 100 || acc.CachedTokens != 30 || acc.ReasoningTokens != 6 {
		t.Fatalf("合并后的 Anthropic usage = %+v", acc)
	}
}

func TestBoostStateInvalidatesOnPolicyOrBaseLimitMutation(t *testing.T) {
	tracker := newBoostTracker()
	c := &config.Config{Boost: boostCfg()}
	tracker.mark("u", "5h", 123, 3.6, 1, c, 2.4)
	c.Boost.MaxBoostTimes = 1
	if _, _, ok := tracker.state("u", "5h", 123, c, 2.4); ok {
		t.Fatal("同配置指针的 boost 策略变化未使旧状态失效")
	}

	tracker.mark("u", "5h", 123, 3.6, 1, c, 2.4)
	if _, _, ok := tracker.state("u", "5h", 123, c, 3.0); ok {
		t.Fatal("原始用户限额变化未使旧状态失效")
	}
}
