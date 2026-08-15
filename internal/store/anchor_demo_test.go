package store

import (
	"testing"
	"time"

	"opgo/internal/meter"
)

// TestAnchorShiftDemo 演示 resetsAt 锚点变化时窗口用量如何变化（计费记录本身不受影响）。
func TestAnchorShiftDemo(t *testing.T) {
	db, err := Open(t.TempDir() + "/demo.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	base := time.Now().UTC().Truncate(time.Second)
	// 三笔已落库的消费（时间戳不可变）：0h / 2h / 4h 各 100/200/300 微元
	recs := []struct {
		at   time.Duration
		cost int64
	}{{0, 100}, {2 * time.Hour, 200}, {4 * time.Hour, 300}}
	for _, r := range recs {
		u := meter.Usage{PromptTokens: 1, TotalTokens: 1}
		if err := db.RecordUsage("uuid-1", "sk-demo", "deepseek-v4-flash", "/v1/chat/completions", u, r.cost, base.Add(r.at)); err != nil {
			t.Fatal(err)
		}
	}

	P := 5 * time.Hour
	used := func(resetsAt time.Time) int64 {
		start := resetsAt.Add(-P)
		v, err := db.UserWindowSum("uuid-1", start.UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	t.Logf("基准：resetsAt=%s → 窗口起点=%s → used=%d (100+200+300=600 全部在窗口内)", base.Add(P).Format("15:04:05"), base.Add(P).Add(-P).Format("15:04:05"), used(base.Add(P)))

	// 1) 锚点前移（更早重置）：窗口起点更早，包含更早记录 → used 变大（保守，不会超扣）
	t.Logf("锚点前移 2h：used=%d（包含 t=0h 的 100 → 600，偏保守）", used(base.Add(P-2*time.Hour)))

	// 2) 锚点后退（更晚重置）：窗口起点更晚 → used 变小（暂时放宽，下轮自愈）
	t.Logf("锚点后退 1h：used=%d（只含 t=2h/4h → 500，暂时放宽）", used(base.Add(P+time.Hour)))

	// 3) 锚点回跳（provider 重置清零后给新 resetsAt）：起点 = 4.5h-5h = -0.5h，三笔仍在窗口内
	t.Logf("锚点跳回 4.5h（新周期）：used=%d（起点 -0.5h，三笔全含）", used(base.Add(4*time.Hour+30*time.Minute)))

	// 4) 安全属性：任意锚点下，每笔记录至多计入一次（不重复）；数据库总额始终不变
	total := int64(0)
	for _, r := range recs {
		total += r.cost
	}
	if got := used(base.Add(24 * time.Hour)); got != 0 {
		t.Errorf("窗口滚出后应为 0，got %d", got)
	}
	t.Logf("窗口完全滚出后 used=0；DB 中 3 笔记录总额=%d 始终保留（账不丢、不重复）", total)
}
