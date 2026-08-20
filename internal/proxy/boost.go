package proxy

import (
	"sync"

	"opgo/internal/balance"
	"opgo/internal/config"
	"opgo/internal/meter"
)

// boostState 某 uuid+period 的智能提额状态（仅内存）。
// 同一周期（resetsAt 不变）可多次提额，limit 为当前档生效限额（美元）：
// 第 1 次 = BoostPercent%×L，第 2 次 = BoostPercent%×第 1 次，依此类推。
// level 为已提额次数（0 = 未提额），受 max_boost_times 限制。
// 重置后 resetsAt 变化，或配置热更新/原始限额变化时，状态自动清空并按
// 当前配置重新判断是否提额。
type boostState struct {
	resetsAt  int64          // 所属周期 resetsAt（Unix 秒）
	limit     float64        // 当前档生效限额（美元）；0 = 未提额
	level     int            // 已提额次数
	revision  *config.Config // 创建状态时的配置快照；热更新后指针必然变化
	policy    config.Boost   // 同指针被调用方修改时也能检测策略变化
	baseLimit float64        // 用户/全局原始限额变化同样使旧提额失效
}

// boostTracker 智能提额状态（仅内存）。
type boostTracker struct {
	mu sync.Mutex
	m  map[string]map[string]boostState
}

func newBoostTracker() *boostTracker {
	return &boostTracker{m: map[string]map[string]boostState{}}
}

// state 返回当前周期、当前配置快照下的提额限额与次数；未提额、跨周期、
// 配置热更新或原始限额变化时返回 (0, 0, false) 并清理旧状态。
func (t *boostTracker) state(uuid, period string, resetsAtUnix int64, c *config.Config, baseLimit float64) (float64, int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cur, ok := t.m[uuid][period]
	if !ok || cur.resetsAt != resetsAtUnix || cur.limit <= 0 || cur.revision != c || cur.policy != c.Boost || cur.baseLimit != baseLimit {
		if ok {
			delete(t.m[uuid], period)
			if len(t.m[uuid]) == 0 {
				delete(t.m, uuid)
			}
		}
		return 0, 0, false
	}
	return cur.limit, cur.level, true
}

// mark 记录当前周期提额到 newLimit，已提额次数记为 newLevel。
func (t *boostTracker) mark(uuid, period string, resetsAtUnix int64, newLimit float64, newLevel int, c *config.Config, baseLimit float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m[uuid] == nil {
		t.m[uuid] = map[string]boostState{}
	}
	t.m[uuid][period] = boostState{
		resetsAt: resetsAtUnix, limit: newLimit, level: newLevel,
		revision: c, policy: c.Boost, baseLimit: baseLimit,
	}
}

// maybeBoostLocked 在 uuid 锁内尝试智能提额（由 userLimitExceeded 调用，保证串行）。
// 支持多次提额，每次逻辑完全一样：
//  1. used >= TriggerPercent% × 当前档限额（第 0 档 = 原限额 L）
//  2. 该窗口总池 status == "ok" 且 percent < PoolMaxPercent（每次提额都重新检查）
//  3. 另外两个窗口 used/limit < OtherWindowMaxPercent（防止本窗口提额把其他窗口也冲垮）
//  4. 已提额次数 < MaxBoostTimes（0 = 不限）
//
// 每次提额后生效限额 = BoostPercent% × 当前档限额（第 1 次 = 150%×L，第 2 次 = 150%×150%×L …）。
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
	base := limit
	level := 0
	if cur, lv, ok := p.boost.state(uuid, period, resetsAtUnix, c, limit); ok {
		base = cur // 多级提额：以当前档限额为基数
		level = lv // 已提额次数
	}
	// 最多提额次数限制
	if b.MaxBoostTimes > 0 && level >= b.MaxBoostTimes {
		return false
	}
	// 触发阈值：used >= trigger% × base（base = 原限额 L 或当前档限额）
	if usedUnits < meter.USDToUnits(base*float64(b.TriggerPercent)/100) {
		return false
	}
	// 池子闸门：每次提额都必须余量充足
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
	newLimit := base * float64(b.BoostPercent) / 100
	p.boost.mark(uuid, period, resetsAtUnix, newLimit, level+1, c, limit)
	p.log.Info("智能提额",
		"uuid", uuid,
		"period", period,
		"level", level+1,
		"level_base", base,
		"boost_limit", newLimit,
	)
	return true
}

// hardLimitFor 返回当前生效的硬卡额度（美元）：
//   - boost 未启用：原限额 L
//   - 启用未提额：L × BaseOveragePercent/100（后台缓冲，防对话中途断）
//   - 启用已提额：当前档限额（第 n 次提额 = L × (BoostPercent/100)^n）
func (p *Proxy) hardLimitFor(c *config.Config, uuid, period string, limit float64, resetsAtUnix int64) float64 {
	if !c.Boost.Enabled {
		return limit
	}
	if cur, _, ok := p.boost.state(uuid, period, resetsAtUnix, c, limit); ok {
		return cur
	}
	return limit * float64(c.Boost.BaseOveragePercent) / 100
}
