package balance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"opgo/internal/config"
)

// DefaultURL 余额接口默认地址（代码内置；config 留空即用此值）。
const DefaultURL = "https://opencode.ai/zen/go/v1/usage"

// CapInfo 一个额度的实时状态。
type CapInfo struct {
	Status   string
	Percent  int
	ResetsAt time.Time
}

// Exceeded 该额度是否已用尽（百分比 >= 100 或状态异常）。
func (c CapInfo) Exceeded() bool { return c.Percent >= 100 || c.Status != "ok" }

// WindowStart 当前窗口起点（窗口 = [resetsAt-周期, resetsAt)）。
func (c CapInfo) WindowStart(period time.Duration) time.Time { return c.ResetsAt.Add(-period) }

// Snapshot 一次成功抓取的快照。
type Snapshot struct {
	FetchedAt time.Time
	Rolling   CapInfo // 5h
	Weekly    CapInfo // 1w
	Monthly   CapInfo // 1m
}

// Cap 按周期名取额度信息（5h/1w/1m）。
func (s *Snapshot) Cap(name string) CapInfo {
	switch name {
	case "5h":
		return s.Rolling
	case "1w":
		return s.Weekly
	case "1m":
		return s.Monthly
	}
	return CapInfo{}
}

type capJSON struct {
	Status   string `json:"status"`
	Percent  int    `json:"percent"`
	ResetsAt string `json:"resetsAt"`
}

func (c capJSON) toCap(name string) (CapInfo, error) {
	if c.Status == "" {
		return CapInfo{}, fmt.Errorf("余额响应缺少 %s.status", name)
	}
	if c.Percent < 0 || c.Percent > 100 {
		return CapInfo{}, fmt.Errorf("余额响应 %s.percent 非法: %d", name, c.Percent)
	}
	t, err := time.Parse(time.RFC3339Nano, c.ResetsAt)
	if err != nil {
		return CapInfo{}, fmt.Errorf("余额响应 %s.resetsAt 不是合法时间: %w", name, err)
	}
	return CapInfo{Status: c.Status, Percent: c.Percent, ResetsAt: t}, nil
}

// Parse 解析余额接口响应；任一额度缺失/非法 → 整体失败。
func Parse(body []byte) (Snapshot, error) {
	var obj struct {
		Usage struct {
			Rolling capJSON `json:"rolling"`
			Weekly  capJSON `json:"weekly"`
			Monthly capJSON `json:"monthly"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return Snapshot{}, fmt.Errorf("余额响应不是合法 JSON: %w", err)
	}
	s := Snapshot{FetchedAt: time.Now().UTC()}
	var err error
	if s.Rolling, err = obj.Usage.Rolling.toCap("rolling"); err != nil {
		return Snapshot{}, err
	}
	if s.Weekly, err = obj.Usage.Weekly.toCap("weekly"); err != nil {
		return Snapshot{}, err
	}
	if s.Monthly, err = obj.Usage.Monthly.toCap("monthly"); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

// Syncer 周期性抓取余额并缓存快照（主 key 只在服务端内存与上游请求中使用）。
// 配置通过 cfgSrc 动态读取：热更新 master_key / balance_url / 间隔后无需重启。
type Syncer struct {
	cfgSrc  func() *config.Config
	timeout time.Duration
	client  *http.Client
	log     *slog.Logger

	mu   sync.RWMutex
	snap *Snapshot
}

func New(cfgSrc func() *config.Config, timeout time.Duration, log *slog.Logger) *Syncer {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Syncer{
		cfgSrc: cfgSrc, timeout: timeout, log: log,
		client: &http.Client{Timeout: timeout},
	}
}

// Start 启动后台抓取（立即抓一次，之后按配置间隔，间隔变更即时生效）。
func (s *Syncer) Start(ctx context.Context) {
	go func() {
		s.fetch()
		for {
			t := time.NewTimer(s.interval())
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				s.fetch()
			}
		}
	}()
}

func (s *Syncer) interval() time.Duration {
	sec := 120
	if c := s.cfgSrc(); c != nil && c.BalanceIntervalSeconds > 0 {
		sec = c.BalanceIntervalSeconds
	}
	return time.Duration(sec) * time.Second
}

// current 返回当前生效的余额接口地址与主 key（热更新后即时取用）。
func (s *Syncer) current() (url, token string) {
	if c := s.cfgSrc(); c != nil {
		url = c.BalanceURL
		token = c.MasterKey
	}
	if url == "" {
		url = DefaultURL
	}
	return url, token
}

func (s *Syncer) fetch() {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	url, token := s.current()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		s.log.Error("余额请求构建失败", "err", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36")
	resp, err := s.client.Do(req)
	if err != nil {
		s.log.Warn("余额抓取失败", "err", err)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		s.log.Warn("余额响应读取失败", "err", err)
		return
	}
	if resp.StatusCode != http.StatusOK {
		s.log.Warn("余额接口状态异常", "status", resp.StatusCode)
		return
	}
	snap, err := Parse(body)
	if err != nil {
		s.log.Warn("余额解析失败", "err", err)
		return
	}
	s.mu.Lock()
	s.snap = &snap
	s.mu.Unlock()
	s.log.Info("余额已同步",
		"rolling_percent", snap.Rolling.Percent,
		"weekly_percent", snap.Weekly.Percent,
		"monthly_percent", snap.Monthly.Percent,
		"rolling_resets_at", snap.Rolling.ResetsAt.Format(time.RFC3339),
	)
}

// Snapshot 返回最近快照与是否已成功同步过。
func (s *Syncer) Snapshot() (*Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.snap == nil {
		return nil, false
	}
	cp := *s.snap
	return &cp, true
}
