package store

import (
	"path/filepath"
	"testing"
	"time"

	"opgo/internal/meter"
)

func TestRecordAndSum(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := time.Now().UTC()
	u1 := meter.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}
	u2 := meter.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	if err := s.RecordUsage("uuid-1", "k1", "mimo-v2.5", "/v1/chat/completions", u1, 1000, base); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage("uuid-1", "k1", "mimo-v2.5", "/v1/chat/completions", u2, 200, base.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage("uuid-2", "k2", "mimo-v2.5", "/v1/chat/completions", u2, 500, base); err != nil {
		t.Fatal(err)
	}
	// uuid-1 全部
	if v, _ := s.UserWindowSum("uuid-1", 0); v != 1200 {
		t.Errorf("uuid-1 sum = %d, want 1200", v)
	}
	// uuid-1 从 base+5s 起只含第二笔
	if v, _ := s.UserWindowSum("uuid-1", base.Add(5*time.Second).UnixMilli()); v != 200 {
		t.Errorf("uuid-1 window sum = %d, want 200", v)
	}
	// 总池
	if v, _ := s.TotalWindowSum(0); v != 1700 {
		t.Errorf("total sum = %d, want 1700", v)
	}
	// 未来起点
	if v, _ := s.TotalWindowSum(base.Add(time.Hour).UnixMilli()); v != 0 {
		t.Errorf("future sum = %d, want 0", v)
	}
}
