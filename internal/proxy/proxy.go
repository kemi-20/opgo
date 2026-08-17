package proxy

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"opgo/internal/balance"
	"opgo/internal/config"
	"opgo/internal/meter"
	"opgo/internal/store"
	"opgo/internal/translate"
)

// BalanceSource 提供余额快照（真实 Syncer 或测试替身）。
type BalanceSource interface {
	Snapshot() (*balance.Snapshot, bool)
}

// Proxy 主 HTTP 处理器。
// cfg 为可热更新的配置管理器：每次请求从 Get() 取当前快照，配置文件变更即时生效。
type Proxy struct {
	cfg       *config.Manager
	db        *store.Store
	indexHTML []byte
	log       *slog.Logger
	balance   BalanceSource
	transport *http.Transport

	mu        sync.Mutex
	userLocks map[string]*sync.Mutex
	rate      *rateLimiter
	boost     *boostTracker
}

func New(cfg *config.Manager, db *store.Store, indexHTML []byte, bal BalanceSource, log *slog.Logger) *Proxy {
	return &Proxy{
		cfg: cfg, db: db, indexHTML: indexHTML, log: log, balance: bal,
		userLocks: map[string]*sync.Mutex{},
		rate:      newRateLimiter(),
		boost:     newBoostTracker(),
		transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/":
		p.serveIndex(w, r)
	case path == "/healthz":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	case path == "/favicon.ico":
		w.WriteHeader(http.StatusNoContent)
	case path == "/api/usage" && r.Method == http.MethodPost:
		p.apiUsage(w, r)
	case path == "/api/admin" && r.Method == http.MethodPost:
		p.apiAdmin(w, r)
	case path == "/v1/usage" && r.Method == http.MethodGet:
		p.serveUsage(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/models"):
		p.serveModels(w, r)
	default:
		p.forward(w, r)
	}
}

// serveUsage 以官方一致格式返回套餐余量（数据来自服务端缓存快照，不携带任何 key）。
func (p *Proxy) serveUsage(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := p.authUser(r); !ok {
		p.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "无效的 key"})
		return
	}
	snap, synced := p.balance.Snapshot()
	if !synced {
		p.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "套餐余量尚未同步，请稍后再试"})
		return
	}
	capJSON := func(c balance.CapInfo) map[string]any {
		return map[string]any{
			"status":   c.Status,
			"percent":  c.Percent,
			"resetsAt": c.ResetsAt.Format(time.RFC3339Nano),
		}
	}
	p.writeJSON(w, http.StatusOK, map[string]any{
		"usage": map[string]any{
			"rolling": capJSON(snap.Rolling),
			"weekly":  capJSON(snap.Weekly),
			"monthly": capJSON(snap.Monthly),
		},
	})
}

func (p *Proxy) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(p.indexHTML)
}

func (p *Proxy) authUser(r *http.Request) (*config.User, string, bool) {
	key := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		key = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if key == "" {
		key = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if key == "" || len(key) > 512 {
		return nil, "", false
	}
	u := p.cfg.Get().UserByKey(key)
	return u, key, u != nil
}

func (p *Proxy) serveModels(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := p.authUser(r); !ok {
		p.writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "无效的 key")
		return
	}
	created := time.Now().Unix()
	if snap, synced := p.balance.Snapshot(); synced {
		created = snap.Monthly.ResetsAt.Unix()
	}
	c := p.cfg.Get()
	data := make([]map[string]any, 0, len(c.ModelNames()))
	for _, name := range c.ModelNames() {
		item := map[string]any{"id": name, "object": "model", "created": created, "owned_by": "config"}
		if pr, ok := c.Price(name); ok {
			if pr.HasContextLength() {
				item["context_length"] = pr.ContextLength
			}
			if pr.HasMaxOutputTokens() {
				item["max_output_tokens"] = pr.MaxOutputTokens
			}
			// 模态：config 为空默认 text->text，拆为 architecture.modality/input_modalities/output_modalities
			m := pr.EffectiveModality()
			item["architecture"] = map[string]any{
				"modality":          m.Raw,
				"input_modalities":  m.Input,
				"output_modalities": m.Output,
			}
		}
		data = append(data, item)
	}
	p.writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request) {
	user, key, ok := p.authUser(r)
	if !ok {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<20))
		p.writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "无效的 key")
		return
	}
	c := p.cfg.Get()
	if !p.rate.allow(user.UUID, c.RateLimitPerMinute, time.Now()) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<20))
		p.writeOpenAIError(w, http.StatusTooManyRequests, "rate_limited", "请求过于频繁，请稍后再试")
		return
	}
	// 图片上传场景 body 可能很大（JSON 内嵌 base64），上限 512MB；
	// 超限返回 413，绝不静默截断后转发损坏的 JSON。
	const maxBodyBytes = 512 << 20
	if r.ContentLength > maxBodyBytes {
		p.writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "请求体过大（超过 512MB）")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		p.writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "读取请求体失败")
		return
	}
	if len(body) > maxBodyBytes {
		p.writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "请求体过大（超过 512MB）")
		return
	}
	// 协议转换：客户端格式 → 模型配置的目标格式（transformation）。
	// 未配置/空/false/0 → 透传只替换认证（与以前一致）。
	srcFormat, srcOK := translate.DetectFormat(r.URL.Path)
	dstFormat := translate.Format("")
	transformEnabled := false
	model := meter.RequestModel(body)
	price, hasPrice := config.ModelPricing{}, false
	if model != "" {
		price, hasPrice = c.Price(model)
		if !hasPrice {
			p.writeOpenAIError(w, http.StatusForbidden, "model_not_allowed", fmt.Sprintf("模型 %s 未在配置中，无法计费", model))
			return
		}
		if dst, ok := translate.ParseFormat(price.Transformation); ok {
			dstFormat = dst
			transformEnabled = srcOK && dst != srcFormat
		}
	}
	snap, synced := p.balance.Snapshot()
	now := time.Now()
	if model != "" {
		if !synced {
			p.writeOpenAIError(w, http.StatusServiceUnavailable, "balance_not_synced", "套餐余量尚未同步，请稍后再试")
			return
		}
		if which, over := poolExceeded(snap); over {
			p.log.Warn("总池拦截", "uuid", user.UUID, "which", which, "model", model)
			p.writeOpenAIError(w, http.StatusTooManyRequests, "insufficient_quota", "已超出"+which+"，本次请求被拒绝")
			return
		}
		if which, over := p.userLimitExceeded(user, snap, now); over {
			p.log.Warn("个人限额拦截", "uuid", user.UUID, "which", which, "model", model)
			p.writeOpenAIError(w, http.StatusTooManyRequests, "insufficient_quota", "已超出"+which+"，本次请求被拒绝")
			return
		}
	}
	// 请求体转换：源格式 → 目标格式
	upstreamPath := stripV1Prefix(r.URL.Path)
	var reqMeta *translate.Request
	if transformEnabled {
		reqMeta, _ = translate.ParseRequest(srcFormat, body)
		nb, err := translate.ConvertRequest(srcFormat, dstFormat, body)
		if err != nil {
			p.log.Warn("请求协议转换失败", "err", err, "uuid", user.UUID, "model", model)
			p.writeOpenAIError(w, http.StatusBadGateway, "translate_error", "请求协议转换失败")
			return
		}
		body = nb
		upstreamPath = translate.FormatPath(dstFormat)
	} else if hasPrice && !strings.HasSuffix(r.URL.Path, "/messages") {
		// 流式 usage 注入（仅 OpenAI 风格端点；Anthropic /messages 不注入）。
		if nb, changed := meter.EnsureStreamUsage(body); changed {
			body = nb
		}
	}
	// 客户端可用标准 OpenAI 路径（/v1/chat/completions 等）：去掉 /v1 前缀后拼接到上游
	upstream := c.UpstreamBase + upstreamPath
	if r.URL.RawQuery != "" {
		upstream += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream, bytes.NewReader(body))
	if err != nil {
		p.writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "构建上游请求失败")
		return
	}
	// 最小干预：除认证外全部原样透传（含 UA、Content-Type、其他头）。
	copyHeaders(req.Header, r.Header)
	req.Header.Del("Content-Length") // 由 Go 按 body 自动重算（值不变）
	req.Header.Set("Authorization", "Bearer "+c.MasterKey)
	req.Header.Set("x-api-key", c.MasterKey)
	// Accept-Encoding 强制 identity：代理必须读取响应 body 解析 usage 计费，
	// 压缩响应无法解析；对客户端透明（Accept-Encoding 仅是偏好）。
	req.Header.Set("Accept-Encoding", "identity")
	removeHopHeaders(req.Header)

	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		p.log.Warn("上游请求失败", "err", err, "uuid", user.UUID, "model", model)
		p.writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "上游请求失败，请稍后再试")
		return
	}
	defer resp.Body.Close()

	streaming := resp.StatusCode == http.StatusOK && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	if streaming {
		copyHeaders(w.Header(), resp.Header)
		removeHopHeaders(w.Header())
		w.Header().Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		var conv *translate.StreamConverter
		if transformEnabled {
			conv = translate.NewStreamConverter(dstFormat, srcFormat, model)
		}
		p.streamCopy(w, r, resp, user, key, model, price, now, conv)
		return
	}
	const maxRespBytes = 512 << 20
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes+1))
	if err != nil {
		p.log.Warn("读取上游响应失败", "err", err)
		return
	}
	if len(respBody) > maxRespBytes {
		p.log.Warn("上游响应过大", "bytes", len(respBody))
		p.writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "上游响应过大")
		return
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && model != "" {
		if u, ok2 := meter.ParseBodyUsage(respBody); ok2 {
			p.recordUsage(user, key, model, r.URL.Path, u, price, now)
		}
	}
	// 仅 2xx 响应做协议转换；错误响应原样透传（保留上游错误语义与状态码）
	if transformEnabled && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		nb, err := translate.ConvertResponseMeta(reqMeta, srcFormat, dstFormat, respBody)
		if err != nil {
			p.log.Warn("响应协议转换失败", "err", err, "uuid", user.UUID, "model", model)
			p.writeOpenAIError(w, http.StatusBadGateway, "translate_error", "响应协议转换失败")
			return
		}
		respBody = nb
	}
	copyHeaders(w.Header(), resp.Header)
	removeHopHeaders(w.Header())
	w.Header().Set("Content-Length", strconv.Itoa(len(respBody)))
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(respBody); err != nil {
		p.log.Debug("写响应失败", "err", err)
	}
}

// streamCopy 透传 SSE 流，同时解析末尾 usage 并记账。
// conv 非 nil 时把上游（目标格式）流逐行转换为客户端格式再写出。
func (p *Proxy) streamCopy(w http.ResponseWriter, r *http.Request, resp *http.Response, user *config.User, key, model string, price config.ModelPricing, now time.Time, conv *translate.StreamConverter) {
	var acc meter.Usage
	got := false
	discard := false // 客户端断开：继续读上游解析 usage 计费，不再写出
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	writeChunks := func(chunks [][]byte) bool {
		for _, c := range chunks {
			if _, err := w.Write(c); err != nil {
				return false
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		return true
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if discard {
			// 客户端断开后仍解析 usage（usage 事件在上游流末尾）
			if u, ok := meter.ParseSSEUsage(line); ok {
				got = true
				mergeUsage(&acc, u)
			}
			continue
		}
		if conv == nil {
			// 透传：同时解析 usage 计费
			if u, ok := meter.ParseSSEUsage(line); ok {
				got = true
				mergeUsage(&acc, u)
			}
			lineCopy := make([]byte, len(line))
			copy(lineCopy, line)
			if !writeChunks([][]byte{append(lineCopy, '\n')}) {
				discard = true
				continue
			}
			continue
		}
		// 转换模式：计费解析用上游原始行；写出用转换后的块
		if u, ok := meter.ParseSSEUsage(line); ok {
			got = true
			mergeUsage(&acc, u)
		}
		chunks, done := conv.Feed(line)
		if !writeChunks(chunks) {
			discard = true
			continue
		}
		if done {
			if !writeChunks(conv.Close()) {
				discard = true
				continue
			}
			conv = nil // 已关闭，不再补
			break
		}
	}
	if got && model != "" {
		p.recordUsage(user, key, model, r.URL.Path, acc, price, now)
	}
}

// mergeUsage 合并一次用量到累计（补全字段）。
func mergeUsage(acc *meter.Usage, u meter.Usage) {
	if u.PromptTokens > 0 || u.CachedTokens > 0 || u.CachedWriteTokens > 0 {
		acc.PromptTokens = u.PromptTokens
		acc.CachedTokens = u.CachedTokens
		acc.CachedWriteTokens = u.CachedWriteTokens
	}
	acc.CompletionTokens += u.CompletionTokens
	if u.TotalTokens > 0 {
		acc.TotalTokens = u.TotalTokens
	}
}

func (p *Proxy) recordUsage(user *config.User, key, model, endpoint string, u meter.Usage, price config.ModelPricing, now time.Time) {
	// 峰谷时：模型级 peak 配置，峰时自动乘倍率（config 中为谷时价格）。
	price = meter.ApplyPeak(price, model, now)
	cost := meter.CostUnits(price, u)
	mu := p.lockFor(user.UUID)
	mu.Lock()
	defer mu.Unlock()
	if err := p.db.RecordUsage(user.UUID, key, model, endpoint, u, cost, now); err != nil {
		p.log.Error("用量记录失败", "err", err)
		return
	}
	p.log.Info("计费", "uuid", user.UUID, "model", model, "tokens", u.TotalTokens, "cost_usd", meter.UnitsToUSD(cost))
}

func (p *Proxy) userLimitExceeded(u *config.User, snap *balance.Snapshot, now time.Time) (string, bool) {
	mu := p.lockFor(u.UUID)
	mu.Lock()
	defer mu.Unlock()
	c := p.cfg.Get()
	limits := c.EffectiveLimits(u)
	// 第一遍：尝试智能提额（在 uuid 锁内串行执行）
	for _, period := range config.Periods {
		lim := limits[period.Name]
		if lim <= 0 {
			continue
		}
		start := snap.Cap(period.Name).WindowStart(period.Duration)
		used, err := p.db.UserWindowSum(u.UUID, start.UnixMilli())
		if err != nil {
			p.log.Error("查询个人用量失败", "err", err)
			continue
		}
		p.maybeBoostLocked(c, u.UUID, period.Name, limits, used, snap)
	}
	// 第二遍：按生效硬卡判定（未提额 = L×105%，提额后 = L×150%，105% 不叠加）
	for _, period := range config.Periods {
		lim := limits[period.Name]
		if lim <= 0 {
			continue
		}
		start := snap.Cap(period.Name).WindowStart(period.Duration)
		used, err := p.db.UserWindowSum(u.UUID, start.UnixMilli())
		if err != nil {
			p.log.Error("查询个人用量失败", "err", err)
			continue
		}
		hard := p.hardLimitFor(c, u.UUID, period.Name, lim, snap.Cap(period.Name).ResetsAt.Unix())
		if used >= meter.USDToUnits(hard) {
			return fmt.Sprintf("个人额度（%s）", period.Name), true
		}
	}
	return "", false
}

func poolExceeded(s *balance.Snapshot) (string, bool) {
	for _, period := range config.Periods {
		if s.Cap(period.Name).Exceeded() {
			return fmt.Sprintf("总池额度（%s）", period.Name), true
		}
	}
	return "", false
}

func (p *Proxy) lockFor(uuid string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.userLocks[uuid]
	if !ok {
		m = &sync.Mutex{}
		p.userLocks[uuid] = m
	}
	return m
}

type windowInfo struct {
	Used       float64  `json:"used"`
	Limit      float64  `json:"limit"` // 始终为 config 原版限额（前端按它显示百分比）
	Percent    *float64 `json:"percent"`
	ResetsAt   string   `json:"resets_at"`
	Boosted    bool     `json:"boosted"`     // 本周期已智能提额
	BoostLimit float64  `json:"boost_limit"` // 提额后的真实限额（未提额时为 0）
}

type capInfo struct {
	Status   string  `json:"status"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resets_at"`
}

func (p *Proxy) apiUsage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.Key == "" {
		p.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体需要 {\"key\": \"...\"}"})
		return
	}
	c := p.cfg.Get()
	u := c.UserByKey(req.Key)
	if u == nil {
		p.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "无效的 key"})
		return
	}
	now := time.Now()
	snap, synced := p.balance.Snapshot()
	p.writeJSON(w, http.StatusOK, map[string]any{
		"uuid":        u.UUID,
		"synced":      synced,
		"snapshot_at": snapshotAt(snap, synced),
		"windows":     p.windowsReport(u.UUID, c.EffectiveLimits(u), snap, synced, now),
		"total":       p.totalReport(snap, synced),
		"pricing":     c.RawPricing(),
	})
}

func (p *Proxy) apiAdmin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		p.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体需要 {\"password\": \"...\"}"})
		return
	}
	c := p.cfg.Get()
	if subtle.ConstantTimeCompare([]byte(req.Password), []byte(c.AdminPassword)) != 1 {
		p.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "密码错误"})
		return
	}
	now := time.Now()
	snap, synced := p.balance.Snapshot()
	users := make([]map[string]any, 0, len(c.Users))
	for i := range c.Users {
		u := &c.Users[i]
		users = append(users, map[string]any{
			"uuid":    u.UUID,
			"remark":  u.Remark,
			"windows": p.windowsReport(u.UUID, c.EffectiveLimits(u), snap, synced, now),
		})
	}
	p.writeJSON(w, http.StatusOK, map[string]any{
		"synced":      synced,
		"snapshot_at": snapshotAt(snap, synced),
		"total":       p.totalReport(snap, synced),
		"users":       users,
		"pricing":     c.RawPricing(),
	})
}

func snapshotAt(snap *balance.Snapshot, synced bool) string {
	if !synced {
		return ""
	}
	return snap.FetchedAt.Format(time.RFC3339)
}

func (p *Proxy) windowsReport(uuid string, limits map[string]float64, snap *balance.Snapshot, synced bool, now time.Time) map[string]windowInfo {
	c := p.cfg.Get()
	out := make(map[string]windowInfo, len(config.Periods))
	for _, period := range config.Periods {
		wi := windowInfo{Limit: limits[period.Name]}
		if synced {
			capInfo := snap.Cap(period.Name)
			start := capInfo.WindowStart(period.Duration)
			used, err := p.db.UserWindowSum(uuid, start.UnixMilli())
			if err != nil {
				p.log.Error("查询个人用量失败", "err", err)
				used = 0
			}
			wi.Used = meter.UnitsToUSD(used)
			wi.ResetsAt = capInfo.ResetsAt.Format(time.RFC3339)
			if wi.Limit > 0 {
				// 百分比永远按 config 原版限额计算
				pct := wi.Used / wi.Limit * 100
				wi.Percent = &pct
				if c.Boost.Enabled {
					if cur, _, ok := p.boost.state(uuid, period.Name, capInfo.ResetsAt.Unix()); ok {
						wi.Boosted = true
						wi.BoostLimit = cur
					}
				}
			}
		}
		out[period.Name] = wi
	}
	return out
}

func (p *Proxy) totalReport(snap *balance.Snapshot, synced bool) map[string]capInfo {
	out := make(map[string]capInfo, len(config.Periods))
	for _, period := range config.Periods {
		ci := capInfo{}
		if synced {
			c := snap.Cap(period.Name)
			ci.Status = c.Status
			ci.Percent = float64(c.Percent)
			ci.ResetsAt = c.ResetsAt.Format(time.RFC3339)
		}
		out[period.Name] = ci
	}
	return out
}

func (p *Proxy) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (p *Proxy) writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	p.writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": message, "type": "opgo_error", "code": code},
	})
}

// stripV1Prefix 去掉路径开头的 /v1 前缀（仅当它是完整路径段）。
func stripV1Prefix(path string) string {
	if path == "/v1" || strings.HasPrefix(path, "/v1/") {
		return strings.TrimPrefix(path, "/v1")
	}
	return path
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func removeHopHeaders(h http.Header) {
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

// rateLimiter 每 uuid 滑动窗口限流。
type rateLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{hits: map[string][]time.Time{}}
}

func (r *rateLimiter) allow(uuid string, limitPerMinute int, now time.Time) bool {
	if limitPerMinute <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := now.Add(-time.Minute)
	kept := r.hits[uuid][:0]
	for _, t := range r.hits[uuid] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= limitPerMinute {
		r.hits[uuid] = kept
		return false
	}
	r.hits[uuid] = append(kept, now)
	return true
}
