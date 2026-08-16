package config

import (
	"io"
	"regexp"
	"log/slog"
	"os"
	"path/filepath"
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
		"deepseek-v4-flash": {"input_per_million": 0.14, "output_per_million": 0.28, "cached_read_per_million": 0.0028, "cached_write_per_million": 0, "context_length": 1000000},
		"deepseek-v4-pro": {"input_per_million": 1.74, "output_per_million": 3.48, "cached_read_per_million": 0.0145, "cached_write_per_million": 0, "context_length": 1000000},
		"mimo-v2.5": {"input_per_million": 0.14, "output_per_million": 0.28, "cached_read_per_million": 0.0028, "cached_write_per_million": 0, "context_length": 1000000},
		"gpt-5.6-luna": {"input_per_million": 1.60, "output_per_million": 7.20, "cached_read_per_million": 0.16, "cached_write_per_million": 2.00, "context_length": 1050000}
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
	if got := c.ModelNames(); len(got) != 4 || got[0] != "deepseek-v4-flash" || got[1] != "deepseek-v4-pro" || got[2] != "mimo-v2.5" || got[3] != "gpt-5.6-luna" {
		t.Errorf("model order = %v", got)
	}
	if _, ok := c.Price("mimo-v2.5"); !ok {
		t.Error("mimo-v2.5 应有价格")
	}
	if pp, ok := c.Price("deepseek-v4-pro"); !ok {
		t.Error("deepseek-v4-pro 应有价格")
	} else if pp.InputPerMillion != 1.74 || pp.OutputPerMillion != 3.48 || pp.CachedReadPerMillion != 0.0145 || pp.CachedWritePerMillion != 0 {
		t.Errorf("deepseek-v4-pro 价格 = %+v，应为官网价（0.435/0.87/0.003625）的 4 倍", pp)
	}
	if lp, ok := c.Price("gpt-5.6-luna"); !ok {
		t.Error("gpt-5.6-luna 应有价格")
	} else if lp.InputPerMillion != 1.60 || lp.OutputPerMillion != 7.20 || lp.CachedReadPerMillion != 0.16 || lp.CachedWritePerMillion != 2.00 {
		t.Errorf("gpt-5.6-luna 价格 = %+v，应为官网>272k 档的 4 倍", lp)
	}
	if _, ok := c.Price("nope"); ok {
		t.Error("nope 不应有价格")
	}
	dp, _ := c.Price("deepseek-v4-flash")
	if !dp.HasContextLength() || dp.ContextLength != 1000000 {
		t.Errorf("deepseek-v4-flash context_length = %d，应默认 1000000", dp.ContextLength)
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
	if len(list) != 4 || list[0].Model != "deepseek-v4-flash" || list[1].Model != "deepseek-v4-pro" || list[2].Model != "mimo-v2.5" || list[3].Model != "gpt-5.6-luna" {
		t.Fatalf("RawPricing 顺序 = %+v，应保持 config 书写顺序", list)
	}
	p := list[0].Price
	if p.InputPerMillion != "0.14" {
		t.Errorf("input raw = %q, want 0.14", p.InputPerMillion)
	}
	if p.OutputPerMillion != "0.28" {
		t.Errorf("output raw = %q, want 0.28", p.OutputPerMillion)
	}
	if p.CachedReadPerMillion != "0.0028" {
		t.Errorf("cached read raw = %q, want 0.0028", p.CachedReadPerMillion)
	}
	if p.CachedWritePerMillion != "0" {
		t.Errorf("cached write raw = %q, want 0", p.CachedWritePerMillion)
	}
	// 修改返回的切片不应影响原配置（副本语义）
	list[0].Price.InputPerMillion = "999"
	if got := c.RawPricing()[0].Price.InputPerMillion; got != "0.14" {
		t.Errorf("RawPricing 应返回副本，got %q", got)
	}
}

func TestContextLengthOptional(t *testing.T) {
	// 不写 context_length 时视为未设置，HasContextLength 为 false
	cfg := regexp.MustCompile(`,\s*"context_length":\s*\d+`).ReplaceAllString(exampleJSON, "")
	c, err := Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"deepseek-v4-flash", "deepseek-v4-pro", "mimo-v2.5", "gpt-5.6-luna"} {
		p, ok := c.Price(name)
		if !ok {
			t.Fatalf("%s 应有价格", name)
		}
		if p.HasContextLength() {
			t.Errorf("%s 未配置 context_length 时应视为空", name)
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
		name  string
		cfg   string
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