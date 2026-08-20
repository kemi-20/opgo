package store

import (
	"database/sql"
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
	u1 := meter.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, CachedWriteTokens: 7, ReasoningTokens: 11}
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
	var cachedWrite, reasoning int64
	if err := s.db.QueryRow("SELECT cached_write_tokens, reasoning_tokens FROM usage WHERE uuid = ? ORDER BY id LIMIT 1", "uuid-1").Scan(&cachedWrite, &reasoning); err != nil {
		t.Fatal(err)
	}
	if cachedWrite != 7 || reasoning != 11 {
		t.Fatalf("stored details cached_write/reasoning = %d/%d, want 7/11", cachedWrite, reasoning)
	}
}

func TestOpenMigratesLegacyUsageSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE usage (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		uuid TEXT NOT NULL, user_key TEXT NOT NULL, model TEXT NOT NULL, endpoint TEXT NOT NULL,
		prompt_tokens INTEGER NOT NULL, completion_tokens INTEGER NOT NULL,
		cached_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL,
		cost_units INTEGER NOT NULL, created_at_epoch_ms INTEGER NOT NULL
	);
	INSERT INTO usage (uuid,user_key,model,endpoint,prompt_tokens,completion_tokens,cached_tokens,total_tokens,cost_units,created_at_epoch_ms)
	VALUES ('legacy','k','m','/v1/responses',10,5,2,15,100,1);`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("旧数据库自动迁移失败: %v", err)
	}
	defer s.Close()
	var legacyCachedWrite, legacyReasoning int64
	if err := s.db.QueryRow("SELECT cached_write_tokens, reasoning_tokens FROM usage WHERE uuid = ?", "legacy").Scan(&legacyCachedWrite, &legacyReasoning); err != nil {
		t.Fatal(err)
	}
	if legacyCachedWrite != 0 || legacyReasoning != 0 {
		t.Fatalf("旧记录新增列默认值 = %d/%d, want 0/0", legacyCachedWrite, legacyReasoning)
	}
	u := meter.Usage{PromptTokens: 20, CompletionTokens: 8, TotalTokens: 28, CachedWriteTokens: 4, ReasoningTokens: 6}
	if err := s.RecordUsage("new", "k", "m", "/v1/responses", u, 200, time.Now()); err != nil {
		t.Fatal(err)
	}
	var gotCachedWrite, gotReasoning int64
	if err := s.db.QueryRow("SELECT cached_write_tokens, reasoning_tokens FROM usage WHERE uuid = ?", "new").Scan(&gotCachedWrite, &gotReasoning); err != nil {
		t.Fatal(err)
	}
	if gotCachedWrite != 4 || gotReasoning != 6 {
		t.Fatalf("迁移后新记录 = %d/%d, want 4/6", gotCachedWrite, gotReasoning)
	}
}
