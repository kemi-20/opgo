package proxy

import (
	"sync"

	"opgo/internal/balance"
	"opgo/internal/config"
	"opgo/internal/meter"
)

// boostTracker 智能提额状态（仅内存）。
// 键为 uuid + period，值为提额时对应的 resetsAt Unix 时间戳：
// 同一周期（resetsAt 不变）只提一次；重置后 resetsAt 变化，自动允许再次提额。
type boostTracker struct {
	mu sync.Mutex
	m  map[string]map[string]int64
}

func newBoostTracker() *boostTracker {
	return &boostTracker{m: map[string]map[string]int64{}}
}

// boosted 是否已在本周期提额。
func (t *boostTracker) boosted(uuid, period string, resetsAtUnix int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[uuid][period] == resetsAtUnix
}

// mark 记录某周期已提额。
func (t *boostTracker) mark(uuid, period string, resetsAtUnix int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m[uuid] == nil {
		t.m[uuid] = map[string]int64{}
	}
	t.m[uuid][period] = resetsAtUnix
}

// maybeBoostLocked 在 uuid 锁内尝试智能提额（由 userLimitExceeded 调用，保证串行）。
// 条件（全部满足才提）：
//  1. used >= TriggerPercent% × 原限额
//  2. 该窗口总池 status == "ok" 且 percent < PoolMaxPercent
//  3. 另外两个窗口 used/limit < OtherWindowMaxPercent（防止 5h 提额把 1w/1m 也冲垮）
//  4. 同一 uuid 同一窗口同一 resetsAt 周期只提一次
//
// 提额后生效限额 = BoostPercent% × 原限额（105% 缓冲不叠加）。
func (p *Proxy) maybeBoostLocked(c *config.Config, uuid, period string, limits map[string]float64, usedUnits int64, snap *balance.Snapshot) bool {
	b := c.Boost
	if !b.Enabled {
		return false
	}
	limit := limits[period]
	if limit <= 0 {
		return false // 该窗口不限额度，无需提额
	}
	capInfo := snap.Cap(period)
	resetsAtUnix := capInfo.ResetsAt.Unix()
	if p.boost.boosted(uuid, period, resetsAtUnix) {
		return false
	}
	// 触发阈值：used >= trigger% × L
	if usedUnits < meter.USDToUnits(limit*float64(b.TriggerPercent)/100) {
		return false
	}
	// 池子闸门：余量充足才提
	if capInfo.Status != "ok" || capInfo.Percent >= b.PoolMaxPercent {
		return false
	}
	// 跨窗口健康：其他窗口不能已近限
	for _, other := range config.Periods {
		if other.Name == period {
			continue
		}
		olim := limits[other.Name]
		if olim <= 0 {
			continue
		}
		ostart := snap.Cap(other.Name).WindowStart(other.Duration)
		oused, err := p.db.UserWindowSum(uuid, ostart.UnixMilli())
		if err != nil {
			p.log.Error("查询其他窗口用量失败", "err", err, "uuid", uuid, "period", other.Name)
			continue
		}
		if oused >= meter.USDToUnits(olim*float64(b.OtherWindowMaxPercent)/100) {
			return false
		}
	}
	p.boost.mark(uuid, period, resetsAtUnix)
	p.log.Info("智能提额",
		"uuid", uuid,
		"period", period,
		"limit", limit,
		"boost_limit", limit*float64(b.BoostPercent)/100,
	)
	return true
}

// hardLimit 返回当前生效的硬卡额度（美元）：
//   - boost 未启用：原限额 L
//   - 启用未提额：L × BaseOveragePercent/100（后台缓冲，防对话中途断）
//   - 启用已提额：L × BoostPercent/100（105% 不叠加）
func (p *Proxy) hardLimit(c *config.Config, limit float64, boosted bool) float64 {
	if !c.Boost.Enabled {
		return limit
	}
	if boosted {
		return limit * float64(c.Boost.BoostPercent) / 100
	}
	return limit * float64(c.Boost.BaseOveragePercent) / 100
}
