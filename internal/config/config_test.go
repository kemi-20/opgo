package config

import (
	"io"
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
		"deepseek-v4-flash": {"input_per_million": 0.14, "output_per_million": 0.28, "cached_read_per_million": 0.0028, "cached_write_per_million": 0},
		"mimo-v2.5": {"input_per_million": 0.14, "output_per_million": 0.28, "cached_read_per_million": 0.0028, "cached_write_per_million": 0}
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
	if got := c.ModelNames(); len(got) != 2 || got[0] != "deepseek-v4-flash" || got[1] != "mimo-v2.5" {
		t.Errorf("model order = %v", got)
	}
	if _, ok := c.Price("mimo-v2.5"); !ok {
		t.Error("mimo-v2.5 应有价格")
	}
	if _, ok := c.Price("nope"); ok {
		t.Error("nope 不应有价格")
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
