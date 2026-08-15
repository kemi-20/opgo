package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ModelPricing 每个模型的每百万 token 价格（美元）。
type ModelPricing struct {
	InputPerMillion       float64 `json:"input_per_million"`
	OutputPerMillion      float64 `json:"output_per_million"`
	CachedReadPerMillion  float64 `json:"cached_read_per_million"`
	CachedWritePerMillion float64 `json:"cached_write_per_million"`
}

// User 一个共享用户；同 uuid 多个 key 共享同一额度。
type User struct {
	UUID   string             `json:"uuid"`
	Remark string             `json:"remark"`
	Keys   []string           `json:"keys"`
	Limits map[string]float64 `json:"limits"`
}

// Period 计费周期（名称与时长）。
type Period struct {
	Name     string
	Duration time.Duration
}

// Periods 三个计费窗口：5小时 / 7天 / 31天。
var Periods = []Period{
	{Name: "5h", Duration: 5 * time.Hour},
	{Name: "1w", Duration: 7 * 24 * time.Hour},
	{Name: "1m", Duration: 31 * 24 * time.Hour},
}

// Config 主配置。
type Config struct {
	Listen                 string                  `json:"listen"`
	UpstreamBase           string                  `json:"upstream_base"`
	MasterKey              string                  `json:"master_key"`
	AdminPassword          string                  `json:"admin_password"`
	BalanceURL             string                  `json:"balance_url"`
	BalanceIntervalSeconds int                     `json:"balance_interval_seconds"`
	RateLimitPerMinute     int                     `json:"rate_limit_per_minute"`
	LimitsPerUser          map[string]float64      `json:"limits_per_user"`
	Pricing                map[string]ModelPricing `json:"pricing"`
	Users                  []User                  `json:"users"`

	keyIndex   map[string]*User
	userIndex  map[string]*User
	modelOrder []string
}

func DefaultConfigPath() string {
	if runtime.GOOS == "windows" {
		return "config.json"
	}
	return "/etc/opgo/config.json"
}

func DefaultDBPath() string {
	if runtime.GOOS == "windows" {
		return "usage.db"
	}
	return "/var/lib/opgo/usage.db"
}

// Load 读取配置；文件不存在时用 example 自动创建并载入，同时在日志中提示修改。
func Load(path string, example []byte, log *slog.Logger) (*Config, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return Parse(data)
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建配置目录失败: %w", err)
		}
	}
	if err := os.WriteFile(path, example, 0o600); err != nil {
		return nil, fmt.Errorf("自动创建配置文件失败: %w", err)
	}
	log.Warn("未找到配置文件，已自动创建示例配置", "path", path)
	log.Warn("请修改 upstream_base / master_key / admin_password / users 后重启生效")
	return Parse(example)
}

// LoadStrict 只读取已有配置，绝不自动创建或改写（供 -audit 使用）。
func LoadStrict(path string, log *slog.Logger) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败（-audit 模式不会自动创建配置）: %w", err)
	}
	return Parse(data)
}

// Parse 解析并校验配置字节。
func Parse(data []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("配置不是合法 JSON: %w", err)
	}
	c.modelOrder = objectKeyOrder(data, "pricing")
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":3003"
	}
	if c.BalanceIntervalSeconds <= 0 {
		c.BalanceIntervalSeconds = 120
	}
}

func (c *Config) validate() error {
	c.applyDefaults()
	if c.UpstreamBase == "" {
		return errors.New("upstream_base 不能为空")
	}
	c.UpstreamBase = strings.TrimRight(c.UpstreamBase, "/")
	if u, err := url.Parse(c.UpstreamBase); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("upstream_base 必须是合法的 http(s) 地址")
	}
	if c.BalanceURL != "" {
		c.BalanceURL = strings.TrimRight(c.BalanceURL, "/")
		if u, err := url.Parse(c.BalanceURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("balance_url 必须是合法的 http(s) 地址")
		}
	}
	if c.MasterKey == "" {
		return errors.New("master_key 不能为空")
	}
	if c.AdminPassword == "" {
		return errors.New("admin_password 不能为空")
	}
	if c.RateLimitPerMinute < 0 {
		return errors.New("rate_limit_per_minute 不能为负数")
	}
	if c.BalanceIntervalSeconds < 0 {
		return errors.New("balance_interval_seconds 不能为负数")
	}
	if len(c.Pricing) == 0 {
		return errors.New("pricing 不能为空")
	}
	for name, p := range c.Pricing {
		if p.InputPerMillion < 0 || p.OutputPerMillion < 0 || p.CachedReadPerMillion < 0 || p.CachedWritePerMillion < 0 {
			return fmt.Errorf("pricing[%s] 价格不能为负数", name)
		}
	}
	for _, v := range c.LimitsPerUser {
		if v < 0 {
			return errors.New("limits_per_user 不能为负数")
		}
	}
	if len(c.Users) == 0 {
		return errors.New("users 不能为空")
	}
	c.keyIndex = make(map[string]*User, 16)
	c.userIndex = make(map[string]*User, len(c.Users))
	for i := range c.Users {
		u := &c.Users[i]
		if u.UUID == "" {
			return errors.New("存在没有 uuid 的用户")
		}
		if _, dup := c.userIndex[u.UUID]; dup {
			return fmt.Errorf("uuid 重复: %s", u.UUID)
		}
		c.userIndex[u.UUID] = u
		if len(u.Keys) == 0 {
			return fmt.Errorf("用户 %s 没有配置 key", u.UUID)
		}
		for _, k := range u.Keys {
			if k == "" {
				return fmt.Errorf("用户 %s 存在空 key", u.UUID)
			}
			if _, dup := c.keyIndex[k]; dup {
				return fmt.Errorf("key 重复: %s", k)
			}
			c.keyIndex[k] = u
		}
		for _, v := range u.Limits {
			if v < 0 {
				return fmt.Errorf("用户 %s 的 limits 不能为负数", u.UUID)
			}
		}
	}
	return nil
}

func (c *Config) UserByKey(key string) *User   { return c.keyIndex[key] }
func (c *Config) UserByUUID(uuid string) *User { return c.userIndex[uuid] }

// Price 查询模型价格。
func (c *Config) Price(model string) (ModelPricing, bool) {
	p, ok := c.Pricing[model]
	return p, ok
}

// ModelNames 返回 pricing 的书写顺序。
func (c *Config) ModelNames() []string { return c.modelOrder }

// EffectiveLimits 返回用户生效的限额（用户覆盖优先，否则全局限额）。
func (c *Config) EffectiveLimits(u *User) map[string]float64 {
	if len(u.Limits) > 0 {
		return u.Limits
	}
	return c.LimitsPerUser
}

// objectKeyOrder 提取 JSON 对象中键的书写顺序（用于保持 pricing 顺序）。
func objectKeyOrder(data []byte, key string) []string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	raw, ok := m[key]
	if !ok {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil
	}
	var out []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			break
		}
		ks, _ := k.(string)
		out = append(out, ks)
		var v json.RawMessage
		_ = dec.Decode(&v)
	}
	return out
}
