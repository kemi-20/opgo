package config

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedLogBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestManagerReloadValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(exampleJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	initial, err := Parse([]byte(exampleJSON))
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(initial, path, nil, newDiscardLogger())

	// 修改文件：用户 key 与价格变更
	changed := strings.Replace(exampleJSON, "sk-u1", "sk-new-hot", 1)
	changed = strings.Replace(changed, "uuid-1", "uuid-hot", 1)
	changed = strings.Replace(changed, "\"input_per_million\": 0.44", "\"input_per_million\": 0.99", 1)
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload 应成功: %v", err)
	}
	nc := mgr.Get()
	if nc.UserByKey("sk-new-hot") == nil {
		t.Error("热更新后新 key 应可查到")
	}
	if nc.UserByKey("sk-u1") != nil {
		t.Error("热更新后旧 key 不应再存在")
	}
	if p, _ := nc.Price("deepseek-v4-flash"); p.InputPerMillion != 0.99 {
		t.Errorf("热更新后价格 = %v，应 0.99", p.InputPerMillion)
	}
	// 旧快照指针不再等于新快照
	if mgr.Get() == initial {
		t.Error("Get 应返回新配置")
	}
}

func TestManagerReloadInvalidKeepsOld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(exampleJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	initial, err := Parse([]byte(exampleJSON))
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(initial, path, nil, newDiscardLogger())

	bad := strings.Replace(exampleJSON, "sk-REPLACE_ME", "", 1) // master_key 为空 → 校验失败
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Reload(); err == nil {
		t.Fatal("非法配置 Reload 应报错")
	}
	if mgr.Get() != initial {
		t.Error("非法配置应保留旧配置")
	}
}

func TestManagerWatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(exampleJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	initial, err := Parse([]byte(exampleJSON))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := NewManager(initial, path, nil, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Watch(ctx, 50*time.Millisecond)

	changed := strings.Replace(exampleJSON, "sk-u2", "sk-watch-new", 1)
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.Get().UserByKey("sk-watch-new") != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if mgr.Get().UserByKey("sk-watch-new") == nil {
		t.Fatal("Watch 应在文件变化后自动热更新")
	}

	// 非法文件不应替换当前配置
	bad := strings.Replace(exampleJSON, "\"pricing\":", "\"pricing_broken\":", 1)
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if mgr.Get().UserByKey("sk-watch-new") == nil {
		t.Error("非法配置不应替换当前配置")
	}
}

func TestManagerWatchDetectsDeletionAndReloadsRestoredFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(exampleJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	initial, err := Parse([]byte(exampleJSON))
	if err != nil {
		t.Fatal(err)
	}
	var logs lockedLogBuffer
	mgr := NewManager(initial, path, nil, slog.New(slog.NewTextHandler(&logs, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Watch(ctx, 20*time.Millisecond)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logs.String(), "已删除") {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "已删除") {
		t.Fatalf("删除配置后应告警，logs=%s", logs.String())
	}
	if mgr.Get() != initial {
		t.Fatal("配置删除期间必须保留最后有效快照")
	}

	restored := strings.Replace(exampleJSON, "sk-u2", "sk-restored", 1)
	if err := os.WriteFile(path, []byte(restored), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && mgr.Get().UserByKey("sk-restored") == nil {
		time.Sleep(20 * time.Millisecond)
	}
	if mgr.Get().UserByKey("sk-restored") == nil {
		t.Fatal("配置文件恢复后应自动重新载入")
	}
}

func TestManagerListenChangeWarning(t *testing.T) {
	// listen 变更不报错，仅记录警告；验证 Reload 仍成功且新配置生效
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(exampleJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	initial, err := Parse([]byte(exampleJSON))
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(initial, path, nil, newDiscardLogger())
	changed := strings.Replace(exampleJSON, "\":3003\"", "\":4000\"", 1)
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Reload(); err != nil {
		t.Fatalf("listen 变更 Reload 应成功: %v", err)
	}
	if mgr.Get().Listen != ":4000" {
		t.Errorf("新 listen = %q", mgr.Get().Listen)
	}
}
