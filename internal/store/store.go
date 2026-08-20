package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"opgo/internal/meter"
)

// Store sqlite 用量存储。
type Store struct{ db *sql.DB }

// Open 打开（必要时创建）数据库并建表。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建数据目录失败: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA synchronous=NORMAL;",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("设置 sqlite 参数失败: %w", err)
		}
	}
	const schema = "CREATE TABLE IF NOT EXISTS usage (" +
		"id INTEGER PRIMARY KEY AUTOINCREMENT," +
		"uuid TEXT NOT NULL," +
		"user_key TEXT NOT NULL," +
		"model TEXT NOT NULL," +
		"endpoint TEXT NOT NULL," +
		"prompt_tokens INTEGER NOT NULL," +
		"completion_tokens INTEGER NOT NULL," +
		"cached_tokens INTEGER NOT NULL DEFAULT 0," +
		"cached_write_tokens INTEGER NOT NULL DEFAULT 0," +
		"reasoning_tokens INTEGER NOT NULL DEFAULT 0," +
		"total_tokens INTEGER NOT NULL," +
		"cost_units INTEGER NOT NULL," +
		"created_at_epoch_ms INTEGER NOT NULL" +
		");" +
		"CREATE INDEX IF NOT EXISTS idx_usage_uuid_time ON usage(uuid, created_at_epoch_ms);" +
		"CREATE INDEX IF NOT EXISTS idx_usage_time ON usage(created_at_epoch_ms);"
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化数据库失败: %w", err)
	}
	// 兼容升级前的 usage.db：CREATE TABLE IF NOT EXISTS 不会给旧表补列，
	// 因此启动时执行幂等迁移。两条 DDL 均为代码内固定字符串，不含用户输入。
	for _, migration := range []struct {
		name string
		ddl  string
	}{
		{"cached_write_tokens", "ALTER TABLE usage ADD COLUMN cached_write_tokens INTEGER NOT NULL DEFAULT 0"},
		{"reasoning_tokens", "ALTER TABLE usage ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0"},
	} {
		var exists int
		err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('usage') WHERE name = ?", migration.name).Scan(&exists)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("检查 sqlite 用量列 %s 失败: %w", migration.name, err)
		}
		if exists == 0 {
			if _, err := db.Exec(migration.ddl); err != nil {
				db.Close()
				return nil, fmt.Errorf("迁移 sqlite 用量列 %s 失败: %w", migration.name, err)
			}
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// RecordUsage 记录一笔用量（全部参数化查询，无字符串拼接）。
func (s *Store) RecordUsage(uuid, userKey, model, endpoint string, u meter.Usage, costUnits int64, ts time.Time) error {
	_, err := s.db.Exec("INSERT INTO usage (uuid, user_key, model, endpoint, prompt_tokens, completion_tokens, cached_tokens, cached_write_tokens, reasoning_tokens, total_tokens, cost_units, created_at_epoch_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		uuid, userKey, model, endpoint, u.PromptTokens, u.CompletionTokens, u.CachedTokens, u.CachedWriteTokens, u.ReasoningTokens, u.TotalTokens, costUnits, ts.UnixMilli())
	return err
}

// UserWindowSum 某 uuid 在窗口起点之后的累计消费（微元）。
func (s *Store) UserWindowSum(uuid string, sinceEpochMS int64) (int64, error) {
	var v int64
	err := s.db.QueryRow("SELECT COALESCE(SUM(cost_units), 0) FROM usage WHERE uuid = ? AND created_at_epoch_ms >= ?", uuid, sinceEpochMS).Scan(&v)
	return v, err
}

// TotalWindowSum 全组在窗口起点之后的累计消费（微元）。
func (s *Store) TotalWindowSum(sinceEpochMS int64) (int64, error) {
	var v int64
	err := s.db.QueryRow("SELECT COALESCE(SUM(cost_units), 0) FROM usage WHERE created_at_epoch_ms >= ?", sinceEpochMS).Scan(&v)
	return v, err
}
