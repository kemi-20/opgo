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
	"sort"
	"strconv"
	"strings"
	"time"
)

// ModelPricing 每个模型的每百万 token 价格（美元）与上下文长度。
type ModelPricing struct {
	InputPerMillion       float64 `json:"input_per_million"`
	OutputPerMillion      float64 `json:"output_per_million"`
	CachedReadPerMillion  float64 `json:"cached_read_per_million"`
	CachedWritePerMillion float64 `json:"cached_write_per_million"`
	// ContextLength 上下文长度（token）；<=0 视为未设置，/models 不输出该字段。
	ContextLength int64 `json:"context_length"`
	// MaxOutputTokens 单次最大输出 token；<=0 视为未设置，/models 不输出该字段。
	MaxOutputTokens int64 `json:"max_output_tokens"`
	// Transformation 协议转换目标格式：空/false/0 = 透传只替换认证；
	// 支持 openai_completions / openai_responses / anthropic。
	// 客户端以任意格式访问时，自动转换为该格式转发给上游。
	Transformation string `json:"transformation"`
	// Tag 前端定价表中模型名右侧显示的标签（可空）。
	Tag string `json:"tag,omitempty"`
	// Peak 模型级峰谷时配置（可空 = 该模型不启用峰谷）。config 中填谷时价，峰时自动乘 Multiplier。
	Peak *ModelPeak `json:"peak,omitempty"`
	// Modality 模型模态，格式 "输入1+输入2->输出1"；空 = "text->text"。
	// /v1/models 会拆成 architecture.modality / input_modalities / output_modalities。
	Modality string `json:"modality"`
	// Provider 引用顶层 providers 中的名称；为空时使用唯一/第一个 provider。
	Provider string `json:"provider"`
	// ProviderModel 发往上游时替换成的模型名；为空表示继续使用公开模型名。
	// 该别名不出现在 /v1/models，也不能作为客户端请求的模型名。
	ProviderModel string `json:"provider_model,omitempty"`
}

// ModalityInfo 解析后的模态信息。
type ModalityInfo struct {
	Raw    string   `json:"modality"`
	Input  []string `json:"input_modalities"`
	Output []string `json:"output_modalities"`
}

// EffectiveModality 返回模型模态；为空默认 "text->text"。
func (p ModelPricing) EffectiveModality() ModalityInfo {
	raw := strings.TrimSpace(p.Modality)
	if raw == "" {
		raw = "text->text"
	}
	info := ModalityInfo{Raw: raw}
	parts := strings.SplitN(raw, "->", 2)
	if len(parts) == 2 {
		info.Input = splitModalities(parts[0])
		info.Output = splitModalities(parts[1])
	} else {
		info.Input = splitModalities(raw)
		info.Output = []string{"text"}
	}
	return info
}

func splitModalities(s string) []string {
	var out []string
	for _, m := range strings.Split(s, "+") {
		m = strings.TrimSpace(m)
		if m != "" {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		out = []string{"text"}
	}
	return out
}

// HasContextLength 是否配置了上下文长度。
func (p ModelPricing) HasContextLength() bool { return p.ContextLength > 0 }

// HasMaxOutputTokens 是否配置了最大输出 token。
func (p ModelPricing) HasMaxOutputTokens() bool { return p.MaxOutputTokens > 0 }

// ModelPeak 模型级峰谷时配置：Peak 时段自动对该模型价格乘 Multiplier（默认 2 倍）。
// config 中填写的是谷时价格，峰时自动翻倍扣款，无需额外填写峰时价格。
type ModelPeak struct {
	// Enabled 是否启用峰谷（默认 true，配置了 peak 即启用）。
	Enabled *bool `json:"enabled,omitempty"`
	// Multiplier 峰时价格倍率（默认 2）。
	Multiplier float64 `json:"multiplier,omitempty"`
	// Windows 峰时时间段列表，每项 [开始HH:MM, 结束HH:MM)（UTC）。空 = 无峰时。
	Windows [][]string `json:"windows"`
}

// RawPrice 每个模型的每百万 token 价格的原始 JSON 文本（保留 config 原版精度）。
type RawPrice struct {
	InputPerMillion       string `json:"input_per_million"`
	OutputPerMillion      string `json:"output_per_million"`
	CachedReadPerMillion  string `json:"cached_read_per_million"`
	CachedWritePerMillion string `json:"cached_write_per_million"`
	Tag                   string `json:"tag,omitempty"`
}

// User 一个共享用户；同 uuid 多个 key 共享同一额度。
type User struct {
	UUID   string             `json:"uuid"`
	Remark string             `json:"remark"`
	Keys   []string           `json:"keys"`
	Limits map[string]float64 `json:"limits"`
}

// Provider 一个上游服务：地址与其母 key。母 key 只在服务端内存中使用。
type Provider struct {
	URL string `json:"url"`
	Key string `json:"key"`
}

// Boost 临期额度机制（可选）。
// enabled 时：正常硬卡 = 限额×BaseOveragePercent/100（防对话中途断）；
// 某窗口 used ≥ TriggerPercent%×当前档限额 且满足池子/跨窗口健康时，智能提额到
// BoostPercent%×当前档限额（第 1 次 = 150%×L，第 2 次 = 150%×150%×L …，可多次提额），
// MaxBoostTimes 限制每个周期最多提额次数（0 = 不限），
// 提额状态只存在内存，按 resetsAt 周期对齐，重置后从头开始。
type Boost struct {
	Enabled               bool `json:"enabled"`
	BaseOveragePercent    int  `json:"base_overage_percent"`
	TriggerPercent        int  `json:"trigger_percent"`
	BoostPercent          int  `json:"boost_percent"`
	PoolMaxPercent        int  `json:"pool_max_percent"`
	OtherWindowMaxPercent int  `json:"other_window_max_percent"`
	MaxBoostTimes         int  `json:"max_boost_times"` // 每个周期最多提额次数，0 = 不限
}

// BoostDefaults 返回默认值（未配置时合并到 0 值上，enabled 保持 false）。
// MaxBoostTimes 默认 0 = 不限提额次数。
func BoostDefaults() Boost {
	return Boost{
		BaseOveragePercent:    105,
		TriggerPercent:        90,
		BoostPercent:          150,
		PoolMaxPercent:        85,
		OtherWindowMaxPercent: 95,
		MaxBoostTimes:         0,
	}
}

// ApplyDefaults 用默认值补齐 0 字段。
func (b *Boost) ApplyDefaults() {
	d := BoostDefaults()
	if b.BaseOveragePercent <= 0 {
		b.BaseOveragePercent = d.BaseOveragePercent
	}
	if b.TriggerPercent <= 0 {
		b.TriggerPercent = d.TriggerPercent
	}
	if b.BoostPercent <= 0 {
		b.BoostPercent = d.BoostPercent
	}
	if b.PoolMaxPercent <= 0 {
		b.PoolMaxPercent = d.PoolMaxPercent
	}
	if b.OtherWindowMaxPercent <= 0 {
		b.OtherWindowMaxPercent = d.OtherWindowMaxPercent
	}
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
	BalanceProvider        string                  `json:"balance_provider"`
	BalanceIntervalSeconds int                     `json:"balance_interval_seconds"`
	RateLimitPerMinute     int                     `json:"rate_limit_per_minute"`
	LimitsPerUser          map[string]float64      `json:"limits_per_user"`
	Providers              map[string]Provider     `json:"providers"`
	Pricing                map[string]ModelPricing `json:"pricing"`
	Boost                  Boost                   `json:"boost"`
	Users                  []User                  `json:"users"`

	keyIndex      map[string]*User
	userIndex     map[string]*User
	modelOrder    []string
	providerOrder []string
	rawPricing    map[string]RawPrice
}

func DefaultConfigPath() string {
	if runtime.GOOS == "windows" {
		return "config.jsonc"
	}
	return "/opt/opgo/config.jsonc"
}

// ResolveConfigPath 返回实际应读取的配置路径：
// 新版本默认扩展名为 .jsonc（VSCode 按 JSONC 校验、允许注释）；若 .jsonc 不存在
// 但同目录存在旧的 .json 配置，则回退到旧文件（兼容升级，不自动改名、不覆盖用户文件）。
func ResolveConfigPath(path string) string {
	if !strings.HasSuffix(strings.ToLower(path), ".jsonc") {
		return path
	}
	if _, err := os.Stat(path); err == nil {
		return path
	}
	legacy := strings.TrimSuffix(path, ".jsonc") + ".json"
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return path
}

func DefaultDBPath() string {
	if runtime.GOOS == "windows" {
		return "usage.db"
	}
	return "/opt/opgo/usage.db"
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
	log.Warn("请修改 providers / admin_password / users 后重启生效")
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

// stripJSONComments 移除 JSONC 风格注释（// 与 /* */），支持 config.example.jsonc
// 中 transformation 行内注释。逐字符扫描，字符串内（含转义）的 // 不会被误删，
// 因此 https://... 等 URL 安全。
func stripJSONComments(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inStr := false
	esc := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inStr {
			out = append(out, c)
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch {
		case c == '"':
			inStr = true
			out = append(out, c)
		case c == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				i++
			}
			if i < len(data) {
				out = append(out, '\n')
			}
		case c == '/' && i+1 < len(data) && data[i+1] == '*':
			i += 2
			for i+1 < len(data) && !(data[i] == '*' && data[i+1] == '/') {
				i++
			}
			i++ // 跳过 */
		default:
			out = append(out, c)
		}
	}
	return out
}

// Parse 解析并校验配置字节。支持 JSONC 注释（// 与 /* */）。
func Parse(data []byte) (*Config, error) {
	data = stripJSONComments(data)
	var c Config
	if err := decodeFirstWins(data, &c); err != nil {
		return nil, fmt.Errorf("配置不是合法 JSON: %w", err)
	}
	if err := c.ApplyLegacyDefaults(); err != nil {
		return nil, err
	}
	c.modelOrder = dedupeKeepFirst(objectKeyOrder(data, "pricing"))
	c.providerOrder = dedupeKeepFirst(objectKeyOrder(data, "providers"))
	c.rawPricing = objectRawPricing(data)
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// decodeFirstWins 解析配置并保留重复键的第一次出现值。
// encoding/json 默认后写覆盖，不符合“重复配置只取第一个”的规则；这里用
// Decoder 逐 token 重建 map，遇到已存在键时跳过后续值。
func decodeFirstWins(data []byte, out *Config) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValueFirstWins(dec)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func decodeValueFirstWins(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch delim {
	case '{':
		out := map[string]any{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, _ := keyTok.(string)
			val, err := decodeValueFirstWins(dec)
			if err != nil {
				return nil, err
			}
			if _, exists := out[key]; !exists {
				out[key] = val
			}
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return out, nil
	case '[':
		var out []any
		for dec.More() {
			val, err := decodeValueFirstWins(dec)
			if err != nil {
				return nil, err
			}
			out = append(out, val)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		if out == nil {
			out = []any{}
		}
		return out, nil
	}
	return nil, fmt.Errorf("非法 JSON 分隔符: %q", delim)
}

// ApplyLegacyDefaults 兼容旧版单上游配置：providers 为空时，把顶层
// upstream_base / master_key 视为一个名为 default 的 provider。
func (c *Config) ApplyLegacyDefaults() error {
	if len(c.Providers) > 0 || (c.UpstreamBase == "" && c.MasterKey == "") {
		return c.NormalizeProviders()
	}
	c.Providers = map[string]Provider{
		"default": {URL: c.UpstreamBase, Key: c.MasterKey},
	}
	c.providerOrder = []string{"default"}
	return nil
}

// NormalizeProviders 重建 provider 顺序并校验每个 provider；供解析与审计副本重建后调用。
func (c *Config) NormalizeProviders() error {
	if len(c.Providers) == 0 {
		return errors.New("providers 不能为空（或提供旧版 upstream_base/master_key）")
	}
	if len(c.providerOrder) == 0 {
		names := make([]string, 0, len(c.Providers))
		for name := range c.Providers {
			names = append(names, name)
		}
		sort.Strings(names)
		c.providerOrder = names
	}
	ordered := make([]string, 0, len(c.Providers))
	seen := make(map[string]bool, len(c.Providers))
	for _, name := range c.providerOrder {
		if seen[name] {
			continue
		}
		seen[name] = true
		if _, ok := c.Providers[name]; !ok {
			continue
		}
		ordered = append(ordered, name)
	}
	allNames := make([]string, 0, len(c.Providers))
	for name := range c.Providers {
		allNames = append(allNames, name)
	}
	sort.Strings(allNames)
	missing := make([]string, 0, len(allNames))
	for _, name := range allNames {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	c.providerOrder = append(ordered, missing...)
	c.providerOrder = dedupeKeepFirst(c.providerOrder)
	for _, name := range allNames {
		p := c.Providers[name]
		p.URL = strings.TrimRight(p.URL, "/")
		c.Providers[name] = p
		if p.URL == "" {
			return fmt.Errorf("providers[%s].url 不能为空", name)
		}
		if u, err := url.Parse(p.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("providers[%s].url 必须是合法的 http(s) 地址", name)
		}
		if p.Key == "" {
			return fmt.Errorf("providers[%s].key 不能为空", name)
		}
	}
	return nil
}

func (c *Config) applyDefaults() {
	c.Listen = normalizeListen(c.Listen)
	if c.Listen == "" {
		c.Listen = ":3003"
	}
	if c.BalanceIntervalSeconds <= 0 {
		c.BalanceIntervalSeconds = 120
	}
}

// normalizeListen 兼容三种 listen 写法：":3003"、"3003"、"0.0.0.0:3003"。
// 纯数字按本机所有接口端口处理；非法值返回空串，由默认值或校验处理。
func normalizeListen(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, ":") {
		return v
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 65535 {
		return ":" + strconv.Itoa(n)
	}
	return v
}

func (c *Config) validate() error {
	c.applyDefaults()
	c.Boost.ApplyDefaults()
	if c.Boost.Enabled {
		if c.Boost.BaseOveragePercent < 100 {
			return errors.New("boost.base_overage_percent 不能小于 100")
		}
		if c.Boost.BoostPercent <= 100 {
			return errors.New("boost.boost_percent 必须大于 100")
		}
		if c.Boost.TriggerPercent <= 0 || c.Boost.TriggerPercent > 100 {
			return errors.New("boost.trigger_percent 必须在 (0,100] 区间")
		}
		if c.Boost.PoolMaxPercent <= 0 || c.Boost.PoolMaxPercent > 100 {
			return errors.New("boost.pool_max_percent 必须在 (0,100] 区间")
		}
		if c.Boost.OtherWindowMaxPercent <= 0 || c.Boost.OtherWindowMaxPercent > 100 {
			return errors.New("boost.other_window_max_percent 必须在 (0,100] 区间")
		}
		if c.Boost.MaxBoostTimes < 0 {
			return errors.New("boost.max_boost_times 不能为负数（0 = 不限次数）")
		}
	}
	if c.BalanceURL != "" {
		c.BalanceURL = strings.TrimRight(c.BalanceURL, "/")
		if u, err := url.Parse(c.BalanceURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("balance_url 必须是合法的 http(s) 地址")
		}
	}
	if err := c.NormalizeProviders(); err != nil {
		return err
	}
	if c.BalanceProvider != "" {
		if _, ok := c.ProviderByName(c.BalanceProvider); !ok {
			return errors.New("balance_provider 必须引用 providers 中已有的名称")
		}
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
		p.ProviderModel = strings.TrimSpace(p.ProviderModel)
		if p.ProviderModel != "" && p.ProviderModel != name {
			if _, conflict := c.Pricing[p.ProviderModel]; conflict {
				return fmt.Errorf("pricing[%s].provider_model 不能与公开模型同名: %s", name, p.ProviderModel)
			}
			c.Pricing[name] = p
		}
		if p.Provider != "" {
			found := false
			for _, n := range c.ProviderNames() {
				if n == p.Provider {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("pricing[%s].provider 必须引用 providers 中已有的名称: %s", name, p.Provider)
			}
		} else {
			p.Provider = c.ProviderNames()[0]
			c.Pricing[name] = p
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

// dedupeKeepFirst 去重并保留第一次出现的顺序。
func dedupeKeepFirst(values []string) []string {
	seen := map[string]bool{}
	out := values[:0]
	for _, v := range values {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func (c *Config) UserByKey(key string) *User   { return c.keyIndex[key] }
func (c *Config) UserByUUID(uuid string) *User { return c.userIndex[uuid] }

// ProviderByName 按名称取 provider；重复配置只取第一个（书写顺序）。
func (c *Config) ProviderByName(name string) (Provider, bool) {
	names := c.ProviderNames()
	if name == "" {
		if len(names) == 0 {
			return Provider{}, false
		}
		name = names[0]
	}
	for _, n := range names {
		if n == name {
			p, ok := c.Providers[n]
			return p, ok
		}
	}
	return Provider{}, false
}

// ProviderNames 返回 providers 的书写顺序（重复名已去重，只保留第一个）。
func (c *Config) ProviderNames() []string {
	if len(c.providerOrder) == 0 && len(c.Providers) > 0 {
		return dedupeKeepFirst(mapKeys(c.Providers))
	}
	return append([]string(nil), c.providerOrder...)
}

func mapKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Price 查询模型价格。
func (c *Config) Price(model string) (ModelPricing, bool) {
	p, ok := c.Pricing[model]
	return p, ok
}

// RawModelPrice 模型与其原始价格的展示条目（按 config 书写顺序）。
type RawModelPrice struct {
	Model string   `json:"model"`
	Price RawPrice `json:"price"`
}

// RawPricing 返回每个模型价格的原始 JSON 文本（保留 config 书写精度与顺序），供前端展示。
func (c *Config) RawPricing() []RawModelPrice {
	out := make([]RawModelPrice, 0, len(c.modelOrder))
	for _, name := range c.modelOrder {
		if rp, ok := c.rawPricing[name]; ok {
			out = append(out, RawModelPrice{Model: name, Price: rp})
		}
	}
	return out
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

// objectKeyOrder 提取 JSON 对象中键的书写顺序。调用方负责按“只取第一个”
// 规则去重；这里保留原始顺序以便诊断和展示。
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
	return dedupeKeepFirst(out)
}

// objectRawPricing 提取 pricing 对象中每个数值字段的原始 JSON 文本。
// 使用 json.Decoder 保持数字书写精度（如 0.0028 不被 float64 舍入）。
func objectRawPricing(data []byte) map[string]RawPrice {
	out := make(map[string]RawPrice)
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return out
	}
	raw, ok := root["pricing"]
	if !ok {
		return out
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return out
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			break
		}
		ks, _ := k.(string)
		if _, exists := out[ks]; exists {
			var skipped json.RawMessage
			_ = dec.Decode(&skipped)
			continue
		}
		var obj json.RawMessage
		if err := dec.Decode(&obj); err != nil {
			continue
		}
		od := json.NewDecoder(bytes.NewReader(obj))
		if tok, err := od.Token(); err != nil || tok != json.Delim('{') {
			continue
		}
		rp := RawPrice{}
		fieldSeen := map[string]bool{}
		for od.More() {
			field, err := od.Token()
			if err != nil {
				break
			}
			fs, _ := field.(string)
			if fieldSeen[fs] {
				continue
			}
			var val json.RawMessage
			if err := od.Decode(&val); err != nil {
				continue
			}
			fieldSeen[fs] = true
			s := strings.TrimSpace(string(val))
			switch fs {
			case "input_per_million":
				rp.InputPerMillion = s
			case "output_per_million":
				rp.OutputPerMillion = s
			case "cached_read_per_million":
				rp.CachedReadPerMillion = s
			case "cached_write_per_million":
				rp.CachedWritePerMillion = s
			case "tag":
				rp.Tag = strings.Trim(s, "\"")
			}
		}
		out[ks] = rp
	}
	return out
}
