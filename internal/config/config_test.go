package config

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const exampleJSON = `{
	"listen": ":3003",
	"upstream_base": "https://PROVIDER_HOST/v1",
	"master_key": "sk-REPLACE_ME",
	"admin_password": "ADMIN_REPLACE_ME",
	"rate_limit_per_minute": 60,
	"limits_per_user": {"5h": 2.4, "1w": 6.0, "1m": 12.0},
	"pricing": {
		"deepseek-v4-flash": {"input_per_million": 0.44, "output_per_million": 1.32, "cached_read_per_million": 0.014, "cached_write_per_million": 0, "context_length": 1000000, "max_output_tokens": 384000, "tag": "高峰期2x消耗", "peak": {"enabled": true, "multiplier": 2, "windows": [["01:00", "04:00"], ["06:00", "10:00"]]}},
		"deepseek-v4-pro": {"input_per_million": 1.32, "output_per_million": 3.96, "cached_read_per_million": 0.044, "cached_write_per_million": 0, "context_length": 1000000, "max_output_tokens": 384000, "tag": "高峰期2x消耗", "peak": {"enabled": true, "multiplier": 2, "windows": [["01:00", "04:00"], ["06:00", "10:00"]]}},
		"mimo-v2.5": {"input_per_million": 0.14, "output_per_million": 0.28, "cached_read_per_million": 0.0028, "cached_write_per_million": 0, "context_length": 1000000, "max_output_tokens": 128000},
		"gpt-5.6-luna": {"input_per_million": 1.60, "output_per_million": 7.20, "cached_read_per_million": 0.16, "cached_write_per_million": 2.00, "context_length": 1050000, "max_output_tokens": 128000},
		"hy3": {"input_per_million": 0.14, "output_per_million": 0.58, "cached_read_per_million": 0.0028, "cached_write_per_million": 0, "context_length": 256000, "max_output_tokens": 64000, "transformation": "openai_completions"},
		"muse-spark-1.2-contributor": {"input_per_million": 0.10, "output_per_million": 0.20, "cached_read_per_million": 0.002, "cached_write_per_million": 0, "context_length": 1048576, "max_output_tokens": 131072, "modality": "text+image+audio->text"}
	},
	"users": [
		{"uuid": "uuid-1", "remark": "张三", "keys": ["sk-u1", "sk-u1b"]},
		{"uuid": "uuid-2", "keys": ["sk-u2"]}
	]
}`

func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestParseExample(t *testing.T) {
	c, err := Parse([]byte(exampleJSON))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":3003" {
		t.Errorf("listen = %q", c.Listen)
	}
	if c.UpstreamBase != "https://PROVIDER_HOST/v1" {
		t.Errorf("upstream = %q", c.UpstreamBase)
	}
	if got := c.ModelNames(); len(got) != 6 || got[0] != "deepseek-v4-flash" || got[1] != "deepseek-v4-pro" || got[2] != "mimo-v2.5" || got[3] != "gpt-5.6-luna" || got[4] != "hy3" || got[5] != "muse-spark-1.2-contributor" {
		t.Errorf("model order = %v", got)
	}
	if _, ok := c.Price("mimo-v2.5"); !ok {
		t.Error("mimo-v2.5 应有价格")
	}
	if pp, ok := c.Price("deepseek-v4-pro"); !ok {
		t.Error("deepseek-v4-pro 应有价格")
	} else if pp.InputPerMillion != 1.32 || pp.OutputPerMillion != 3.96 || pp.CachedReadPerMillion != 0.044 || pp.CachedWritePerMillion != 0 {
		t.Errorf("deepseek-v4-pro 价格 = %+v，应为官网谷时价（0.66/1.98/0.022）的 2 倍", pp)
	} else if pp.Tag != "高峰期2x消耗" {
		t.Errorf("deepseek-v4-pro tag = %q，应为 高峰期2x消耗", pp.Tag)
	}
	if lp, ok := c.Price("gpt-5.6-luna"); !ok {
		t.Error("gpt-5.6-luna 应有价格")
	} else if lp.InputPerMillion != 1.60 || lp.OutputPerMillion != 7.20 || lp.CachedReadPerMillion != 0.16 || lp.CachedWritePerMillion != 2.00 {
		t.Errorf("gpt-5.6-luna 价格 = %+v，应为官网>272k 档的 4 倍", lp)
	}
	if hp, ok := c.Price("hy3"); !ok {
		t.Error("hy3 应有价格")
	} else if hp.InputPerMillion != 0.14 || hp.OutputPerMillion != 0.58 || hp.CachedReadPerMillion != 0.0028 || hp.CachedWritePerMillion != 0 {
		t.Errorf("hy3 价格 = %+v，应 0.14/0.58/0.0028/0（models.dev）", hp)
	}
	if mp2, ok := c.Price("muse-spark-1.2-contributor"); !ok {
		t.Error("muse-spark-1.2-contributor 应有价格")
	} else if mp2.InputPerMillion != 0.10 || mp2.OutputPerMillion != 0.20 || mp2.CachedReadPerMillion != 0.002 || mp2.CachedWritePerMillion != 0 {
		t.Errorf("muse 价格 = %+v，应 0.10/0.20/0.002/0（models.dev）", mp2)
	} else if mp2.ContextLength != 1048576 || mp2.MaxOutputTokens != 131072 {
		t.Errorf("muse context/max = %d/%d，应 1048576/131072", mp2.ContextLength, mp2.MaxOutputTokens)
	} else if mp2.Modality != "text+image+audio->text" {
		t.Errorf("muse modality = %q，应 text+image+audio->text", mp2.Modality)
	}
	if _, ok := c.Price("nope"); ok {
		t.Error("nope 不应有价格")
	}
	dp, _ := c.Price("deepseek-v4-flash")
	if !dp.HasContextLength() || dp.ContextLength != 1000000 {
		t.Errorf("deepseek-v4-flash context_length = %d，应默认 1000000", dp.ContextLength)
	}
	if dp.InputPerMillion != 0.44 || dp.OutputPerMillion != 1.32 || dp.CachedReadPerMillion != 0.014 {
		t.Errorf("deepseek-v4-flash 价格 = %+v，应为官网谷时价（0.22/0.66/0.007）的 2 倍", dp)
	}
	if dp.Tag != "高峰期2x消耗" {
		t.Errorf("deepseek-v4-flash tag = %q", dp.Tag)
	}
	// 峰谷：模型级 peak 配置
	fp, _ := c.Price("deepseek-v4-flash")
	if fp.Peak == nil || fp.Peak.Enabled == nil || !*fp.Peak.Enabled || fp.Peak.Multiplier != 2 || len(fp.Peak.Windows) != 2 {
		t.Errorf("deepseek-v4-flash peak = %+v，应为模型级 2 窗口 2 倍", fp.Peak)
	}
	mp, _ := c.Price("mimo-v2.5")
	if mp.Peak != nil {
		t.Errorf("mimo-v2.5 不应有 peak 配置，got %+v", mp.Peak)
	}
	if !dp.HasMaxOutputTokens() || dp.MaxOutputTokens != 384000 {
		t.Errorf("deepseek-v4-flash max_output_tokens = %d，应 384000", dp.MaxOutputTokens)
	}
	if mp, _ := c.Price("mimo-v2.5"); !mp.HasMaxOutputTokens() || mp.MaxOutputTokens != 128000 {
		t.Errorf("mimo-v2.5 max_output_tokens = %d，应 128000", mp.MaxOutputTokens)
	}
	if lp, _ := c.Price("gpt-5.6-luna"); !lp.HasMaxOutputTokens() || lp.MaxOutputTokens != 128000 {
		t.Errorf("gpt-5.6-luna max_output_tokens = %d，应 128000", lp.MaxOutputTokens)
	}
	u := c.UserByKey("sk-u1b")
	if u == nil || u.UUID != "uuid-1" {
		t.Error("sk-u1b 应映射到 uuid-1")
	}
	if u := c.UserByKey("sk-wrong"); u != nil {
		t.Error("错误 key 不应有用户")
	}
	lim := c.EffectiveLimits(c.UserByUUID("uuid-1"))
	if lim["5h"] != 2.4 {
		t.Errorf("limits = %v", lim)
	}
}

func TestParseDefaults(t *testing.T) {
	cfg := strings.Replace(exampleJSON, "\":3003\"", "\"\"", 1)
	cfg = strings.Replace(cfg, "\"rate_limit_per_minute\": 60,", "", 1)
	c, err := Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":3003" {
		t.Errorf("listen 默认 = %q", c.Listen)
	}
	if c.BalanceIntervalSeconds != 120 {
		t.Errorf("balance interval 默认 = %d", c.BalanceIntervalSeconds)
	}
	if c.RateLimitPerMinute != 0 {
		t.Errorf("rate limit 应为 0(不限)，got %d", c.RateLimitPerMinute)
	}
}

func TestParseNumericListen(t *testing.T) {
	cfg := strings.Replace(exampleJSON, "\":3003\"", "\"3003\"", 1)
	c, err := Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":3003" {
		t.Errorf("listen = %q, want :3003", c.Listen)
	}
}

func TestProvidersFirstWins(t *testing.T) {
	cfg := strings.Replace(exampleJSON, `"upstream_base": "https://PROVIDER_HOST/v1",`,
		`"providers": {"go": {"url": "https://first/v1", "key": "sk-first"}, "go": {"url": "https://second/v1", "key": "sk-second"}},`, 1)
	c, err := Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := c.ProviderByName("go")
	if !ok || p.URL != "https://first/v1" || p.Key != "sk-first" {
		t.Fatalf("provider = %+v ok=%v, want first wins", p, ok)
	}
}

func TestModelProviderDefaultsToFirstAndValidatesReference(t *testing.T) {
	cfg := strings.Replace(exampleJSON, `"upstream_base": "https://PROVIDER_HOST/v1",`,
		`"providers": {"go": {"url": "https://go/v1", "key": "sk-go"}, "zen": {"url": "https://zen/v1", "key": "sk-zen"}},`, 1)
	c, err := Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := c.Price("deepseek-v4-flash")
	if p.Provider != "go" {
		t.Fatalf("default provider = %q, want go", p.Provider)
	}
	if _, ok := c.ProviderByName("zen"); !ok {
		t.Fatal("zen provider missing")
	}

	bad := strings.Replace(cfg, `"provider": "go"`, `"provider": "missing"`, 1)
	cfgBad, err := Parse([]byte(bad))
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if _, ok := cfgBad.ProviderByName("missing"); ok {
		t.Fatal("未知模型 provider 应校验失败")
	}
}

func TestDuplicatePricingKeepsFirstForBillingAndDisplay(t *testing.T) {
	cfg := strings.Replace(exampleJSON,
		`"pricing": {`,
		`"pricing": {"test-model": {"input_per_million": 9.99, "output_per_million": 19.98, "tag": "second", "input_per_million": 0.12, "output_per_million": 0.24, "tag": "first"},`, 1)
	c, err := Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := c.Price("test-model")
	if !ok || p.InputPerMillion != 9.99 || p.OutputPerMillion != 19.98 || p.Tag != "second" {
		t.Fatalf("price=%+v ok=%v, want first-wins values", p, ok)
	}
	if got := c.ModelNames(); len(got) == 0 || got[0] != "test-model" {
		t.Fatalf("model order=%v, want duplicate name once at front", got)
	}
	for _, name := range c.ModelNames()[1:] {
		if name == "test-model" {
			t.Fatal("RawPricing/model order repeats duplicate model")
		}
	}
	raw := c.RawPricing()
	if len(raw) == 0 || raw[0].Model != "test-model" || raw[0].Price.Tag != "second" || raw[0].Price.InputPerMillion != "9.99" {
		t.Fatalf("raw pricing=%+v, want first occurrence", raw)
	}
}

func TestLoadAutoCreate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c, err := Load(path, []byte(exampleJSON), newDiscardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("应载入示例配置")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != exampleJSON {
		t.Error("自动创建的文件内容应与示例一致")
	}
}

func TestLoadStrictMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")
	if _, err := LoadStrict(path, newDiscardLogger()); err == nil {
		t.Error("LoadStrict 对缺失文件应报错")
	}
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(string) string
	}{
		{"master_key 为空", func(s string) string { return strings.Replace(s, "sk-REPLACE_ME", "", 1) }},
		{"admin_password 为空", func(s string) string { return strings.Replace(s, "ADMIN_REPLACE_ME", "", 1) }},
		{"upstream_base 非法", func(s string) string { return strings.Replace(s, "https://PROVIDER_HOST/v1", "not-a-url", 1) }},
		{"重复 uuid", func(s string) string { return strings.Replace(s, "uuid-2", "uuid-1", 1) }},
		{"重复 key", func(s string) string { return strings.Replace(s, "sk-u2", "sk-u1", 1) }},
		{"负限额", func(s string) string { return strings.Replace(s, "\"5h\": 2.4", "\"5h\": -1", 1) }},
		{"负价格", func(s string) string {
			return strings.Replace(s, "\"input_per_million\": 0.14", "\"input_per_million\": -0.1", 1)
		}},
		{"无用户", func(s string) string {
			i := strings.Index(s, "\"users\":")
			return s[:i] + "\"users\": []}"
		}},
		{"无 pricing", func(s string) string {
			i := strings.Index(s, "\"pricing\":")
			j := strings.Index(s[i:], "},") + i
			return s[:i] + s[j+1:]
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.mut(exampleJSON))); err == nil {
				t.Error("应校验失败")
			}
		})
	}
}

func TestNoLegacyFields(t *testing.T) {
	if strings.Contains(exampleJSON, "plan_start") {
		t.Error("示例配置不应包含 plan_start")
	}
	if strings.Contains(exampleJSON, "total_limits") {
		t.Error("示例配置不应包含 total_limits")
	}
	if strings.Contains(exampleJSON, "plan_caps") {
		t.Error("示例配置不应包含 plan_caps")
	}
}

func TestNoVendorStringInExample(t *testing.T) {
	if strings.Contains(strings.ToLower(exampleJSON), "opencode") {
		t.Error("示例配置不应包含供应商字样")
	}
}

func TestRawPricingPreservesPrecision(t *testing.T) {
	c, err := Parse([]byte(exampleJSON))
	if err != nil {
		t.Fatal(err)
	}
	list := c.RawPricing()
	if list[0].Price.Tag != "高峰期2x消耗" {
		t.Errorf("RawPricing tag = %q，应保留原始 tag", list[0].Price.Tag)
	}
	if len(list) != 6 || list[0].Model != "deepseek-v4-flash" || list[1].Model != "deepseek-v4-pro" || list[2].Model != "mimo-v2.5" || list[3].Model != "gpt-5.6-luna" || list[4].Model != "hy3" || list[5].Model != "muse-spark-1.2-contributor" {
		t.Fatalf("RawPricing 顺序 = %+v，应保持 config 书写顺序", list)
	}
	p := list[0].Price
	if p.InputPerMillion != "0.44" {
		t.Errorf("input raw = %q, want 0.44", p.InputPerMillion)
	}
	if p.OutputPerMillion != "1.32" {
		t.Errorf("output raw = %q, want 1.32", p.OutputPerMillion)
	}
	if p.CachedReadPerMillion != "0.014" {
		t.Errorf("cached read raw = %q, want 0.014", p.CachedReadPerMillion)
	}
	if p.CachedWritePerMillion != "0" {
		t.Errorf("cached write raw = %q, want 0", p.CachedWritePerMillion)
	}
	// 修改返回的切片不应影响原配置（副本语义）
	list[0].Price.InputPerMillion = "999"
	if got := c.RawPricing()[0].Price.InputPerMillion; got != "0.44" {
		t.Errorf("RawPricing 应返回副本，got %q", got)
	}
}

func TestContextLengthOptional(t *testing.T) {
	// 不写 context_length / max_output_tokens 时视为未设置，对应 Has* 为 false
	cfg := regexp.MustCompile(`,\s*"context_length":\s*\d+`).ReplaceAllString(exampleJSON, "")
	cfg = regexp.MustCompile(`,\s*"max_output_tokens":\s*\d+`).ReplaceAllString(cfg, "")
	c, err := Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"deepseek-v4-flash", "deepseek-v4-pro", "mimo-v2.5", "gpt-5.6-luna", "hy3", "muse-spark-1.2-contributor"} {
		p, ok := c.Price(name)
		if !ok {
			t.Fatalf("%s 应有价格", name)
		}
		if p.HasContextLength() {
			t.Errorf("%s 未配置 context_length 时应视为空", name)
		}
		if p.HasMaxOutputTokens() {
			t.Errorf("%s 未配置 max_output_tokens 时应视为空", name)
		}
	}
}

func TestBoostDefaultsWhenAbsent(t *testing.T) {
	c, err := Parse([]byte(exampleJSON)) // exampleJSON 未含 boost
	if err != nil {
		t.Fatal(err)
	}
	if c.Boost.Enabled {
		t.Error("未配置 boost 时 enabled 应为 false")
	}
	if c.Boost.BaseOveragePercent != 105 || c.Boost.TriggerPercent != 90 ||
		c.Boost.BoostPercent != 150 || c.Boost.PoolMaxPercent != 85 || c.Boost.OtherWindowMaxPercent != 95 {
		t.Errorf("默认值 = %+v", c.Boost)
	}
}

func TestBoostParseEnabled(t *testing.T) {
	cfg := strings.Replace(exampleJSON, `"rate_limit_per_minute": 60,`,
		`"rate_limit_per_minute": 60, "boost": {"enabled": true, "base_overage_percent": 110, "trigger_percent": 92, "boost_percent": 160, "pool_max_percent": 88, "other_window_max_percent": 75},`, 1)
	c, err := Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Boost.Enabled || c.Boost.BaseOveragePercent != 110 || c.Boost.TriggerPercent != 92 ||
		c.Boost.BoostPercent != 160 || c.Boost.PoolMaxPercent != 88 || c.Boost.OtherWindowMaxPercent != 75 {
		t.Errorf("boost = %+v", c.Boost)
	}
}

func TestBoostValidation(t *testing.T) {
	with := func(patch string) string {
		return strings.Replace(exampleJSON, `"rate_limit_per_minute": 60,`,
			`"rate_limit_per_minute": 60, "boost": {`+patch+`},`, 1)
	}
	cases := []struct {
		name string
		cfg  string
	}{
		{"base_overage < 100", with(`"enabled": true, "base_overage_percent": 99`)},
		{"boost_percent <= 100", with(`"enabled": true, "boost_percent": 100`)},
		{"trigger > 100", with(`"enabled": true, "trigger_percent": 101`)},
		{"pool_max > 100", with(`"enabled": true, "pool_max_percent": 101`)},
		{"other_window > 100", with(`"enabled": true, "other_window_max_percent": 101`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.cfg)); err == nil {
				t.Error("应校验失败")
			}
		})
	}
	// 未启用时非法值不报错（不校验）
	if _, err := Parse([]byte(with(`"enabled": false, "base_overage_percent": 99`))); err != nil {
		t.Errorf("未启用不应校验失败: %v", err)
	}
}

// TestParseJSONCComments 验证：配置支持 // 与 /* */ 注释（transformation 行内注释），
// 且字符串内（如 https:// URL）的 // 不会被误删。
func TestParseJSONCComments(t *testing.T) {
	cfg := `{
		"listen": ":3003",
		"upstream_base": "https://PROVIDER_HOST/v1", // URL 里的 // 不能被删
		"master_key": "sk-REPLACE_ME",
		"admin_password": "ADMIN_REPLACE_ME",
		"rate_limit_per_minute": 60,
		"pricing": {
			"deepseek-v4-flash": {"input_per_million": 0.14, "output_per_million": 0.28, "cached_read_per_million": 0.0028, "cached_write_per_million": 0, "transformation": "" // 空=透传
			},
			"mimo-v2.5": {"input_per_million": 0.14, "output_per_million": 0.28, "cached_read_per_million": 0.0028, "cached_write_per_million": 0, "transformation": "openai_completions" /* 转 completions */ }
		},
		"users": [{"uuid": "uuid-1", "keys": ["sk-u1"]}]
	}`
	c, err := Parse([]byte(cfg))
	if err != nil {
		t.Fatalf("带注释配置应解析成功: %v", err)
	}
	if c.UpstreamBase != "https://PROVIDER_HOST/v1" {
		t.Errorf("URL 被注释剥离器误伤: %q", c.UpstreamBase)
	}
	if c.Pricing["mimo-v2.5"].Transformation != "openai_completions" {
		t.Errorf("mimo transformation = %q", c.Pricing["mimo-v2.5"].Transformation)
	}
	if c.Pricing["deepseek-v4-flash"].Transformation != "" {
		t.Errorf("flash transformation = %q", c.Pricing["deepseek-v4-flash"].Transformation)
	}
}

// TestStripJSONCommentsStringSafety 验证字符串内的 // 与转义引号安全。
func TestStripJSONCommentsStringSafety(t *testing.T) {
	in := []byte(`{"a":"http://x/y","b":"line // not comment","c":"esc \" // still str"}`)
	out := string(stripJSONComments(in))
	if out != `{"a":"http://x/y","b":"line // not comment","c":"esc \" // still str"}` {
		t.Errorf("字符串内注释被误删: %s", out)
	}
}

// TestResolveConfigPath 验证 .jsonc/.json 兼容选择：
// 只有 jsonc 读 jsonc；只有 json 读 json；两者都在优先 jsonc。
func TestResolveConfigPath(t *testing.T) {
	dir := t.TempDir()
	jc := filepath.Join(dir, "config.jsonc")
	j := filepath.Join(dir, "config.json")

	// 只有 jsonc
	if err := os.WriteFile(jc, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveConfigPath(jc); got != jc {
		t.Errorf("仅 jsonc 时应返回 jsonc, got %s", got)
	}

	// 只有 json
	if err := os.Remove(jc); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(j, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveConfigPath(jc); got != j {
		t.Errorf("仅 json 时应回退到 json, got %s", got)
	}

	// 两者都在 → 优先 jsonc
	if err := os.WriteFile(jc, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveConfigPath(jc); got != jc {
		t.Errorf("同时存在时应优先 jsonc, got %s", got)
	}

	// 显式 -config config.json 不绕
	if got := ResolveConfigPath(j); got != j {
		t.Errorf("显式 json 路径不应改写, got %s", got)
	}
}
