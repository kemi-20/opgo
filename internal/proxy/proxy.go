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
)

// BalanceSource 提供余额快照（真实 Syncer 或测试替身）。
type BalanceSource interface {
	Snapshot() (*balance.Snapshot, bool)
}

// Proxy 主 HTTP 处理器。
type Proxy struct {
	cfg       *config.Config
	db        *store.Store
	indexHTML []byte
	log       *slog.Logger
	balance   BalanceSource
	transport *http.Transport

	mu        sync.Mutex
	userLocks map[string]*sync.Mutex
	rate      *rateLimiter
}

func New(cfg *config.Config, db *store.Store, indexHTML []byte, bal BalanceSource, log *slog.Logger) *Proxy {
	return &Proxy{
		cfg: cfg, db: db, indexHTML: indexHTML, log: log, balance: bal,
		userLocks: map[string]*sync.Mutex{},
		rate:      newRateLimiter(),
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
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/models"):
		p.serveModels(w, r)
	default:
		p.forward(w, r)
	}
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
	u := p.cfg.UserByKey(key)
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
	data := make([]map[string]any, 0, len(p.cfg.ModelNames()))
	for _, name := range p.cfg.ModelNames() {
		data = append(data, map[string]any{"id": name, "object": "model", "created": created, "owned_by": "config"})
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
	if !p.rate.allow(user.UUID, p.cfg.RateLimitPerMinute, time.Now()) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<20))
		p.writeOpenAIError(w, http.StatusTooManyRequests, "rate_limited", "请求过于频繁，请稍后再试")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		p.writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "读取请求体失败")
		return
	}
	model := meter.RequestModel(body)
	price, hasPrice := config.ModelPricing{}, false
	if model != "" {
		price, hasPrice = p.cfg.Price(model)
		if !hasPrice {
			p.writeOpenAIError(w, http.StatusForbidden, "model_not_allowed", fmt.Sprintf("模型 %s 未在配置中，无法计费", model))
			return
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
	// 流式 usage 注入（仅 OpenAI 风格端点；Anthropic /messages 不注入）。
	if hasPrice && !strings.HasSuffix(r.URL.Path, "/messages") {
		if nb, changed := meter.EnsureStreamUsage(body); changed {
			body = nb
		}
	}
	upstream := p.cfg.UpstreamBase + r.URL.Path
	if r.URL.RawQuery != "" {
		upstream += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream, bytes.NewReader(body))
	if err != nil {
		p.writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "构建上游请求失败")
		return
	}
	copyHeaders(req.Header, r.Header)
	req.Header.Del("Content-Length")
	req.Header.Set("Authorization", "Bearer "+p.cfg.MasterKey)
	req.Header.Set("x-api-key", p.cfg.MasterKey)
	req.Header.Set("Accept-Encoding", "identity")
	// 使用浏览器 UA，避免上游 CDN 按指纹拦截非浏览器客户端
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36")
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
		p.streamCopy(w, r, resp, user, key, model, price, now)
		return
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		p.log.Warn("读取上游响应失败", "err", err)
		return
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && model != "" {
		if u, ok2 := meter.ParseBodyUsage(respBody); ok2 {
			p.recordUsage(user, key, model, r.URL.Path, u, price, now)
		}
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
func (p *Proxy) streamCopy(w http.ResponseWriter, r *http.Request, resp *http.Response, user *config.User, key, model string, price config.ModelPricing, now time.Time) {
	var acc meter.Usage
	got := false
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if u, ok := meter.ParseSSEUsage(line); ok {
			got = true
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
		if _, err := w.Write(line); err != nil {
			return
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	if got && model != "" {
		p.recordUsage(user, key, model, r.URL.Path, acc, price, now)
	}
}

func (p *Proxy) recordUsage(user *config.User, key, model, endpoint string, u meter.Usage, price config.ModelPricing, now time.Time) {
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
	limits := p.cfg.EffectiveLimits(u)
	for _, period := range config.Periods {
		start := snap.Cap(period.Name).WindowStart(period.Duration)
		used, err := p.db.UserWindowSum(u.UUID, start.UnixMilli())
		if err != nil {
			p.log.Error("查询个人用量失败", "err", err)
			continue
		}
		lim := limits[period.Name]
		if lim > 0 && used >= meter.USDToUnits(lim) {
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
	Used     float64  `json:"used"`
	Limit    float64  `json:"limit"`
	Percent  *float64 `json:"percent"`
	ResetsAt string   `json:"resets_at"`
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
	u := p.cfg.UserByKey(req.Key)
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
		"windows":     p.windowsReport(u.UUID, p.cfg.EffectiveLimits(u), snap, synced, now),
		"total":       p.totalReport(snap, synced),
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
	if subtle.ConstantTimeCompare([]byte(req.Password), []byte(p.cfg.AdminPassword)) != 1 {
		p.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "密码错误"})
		return
	}
	now := time.Now()
	snap, synced := p.balance.Snapshot()
	users := make([]map[string]any, 0, len(p.cfg.Users))
	for i := range p.cfg.Users {
		u := &p.cfg.Users[i]
		users = append(users, map[string]any{
			"uuid":    u.UUID,
			"remark":  u.Remark,
			"windows": p.windowsReport(u.UUID, p.cfg.EffectiveLimits(u), snap, synced, now),
		})
	}
	p.writeJSON(w, http.StatusOK, map[string]any{
		"synced":      synced,
		"snapshot_at": snapshotAt(snap, synced),
		"total":       p.totalReport(snap, synced),
		"users":       users,
	})
}

func snapshotAt(snap *balance.Snapshot, synced bool) string {
	if !synced {
		return ""
	}
	return snap.FetchedAt.Format(time.RFC3339)
}

func (p *Proxy) windowsReport(uuid string, limits map[string]float64, snap *balance.Snapshot, synced bool, now time.Time) map[string]windowInfo {
	out := make(map[string]windowInfo, len(config.Periods))
	for _, period := range config.Periods {
		wi := windowInfo{Limit: limits[period.Name]}
		if synced {
			start := snap.Cap(period.Name).WindowStart(period.Duration)
			used, err := p.db.UserWindowSum(uuid, start.UnixMilli())
			if err != nil {
				p.log.Error("查询个人用量失败", "err", err)
				used = 0
			}
			wi.Used = meter.UnitsToUSD(used)
			wi.ResetsAt = snap.Cap(period.Name).ResetsAt.Format(time.RFC3339)
			if wi.Limit > 0 {
				pct := wi.Used / wi.Limit * 100
				wi.Percent = &pct
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
