package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// Manager 提供可热更新的配置快照。
// 所有读取方通过 Get() 拿到同一时刻一致的配置指针；后台 Watch 轮询配置文件，
// 变化时重新 Parse 校验并原子替换。校验失败保留旧配置，不影响运行。
// 注意：listen 变更无法在运行中热切换，检测到时会记录警告，其余字段立即生效。
type Manager struct {
	path    string
	example []byte
	log     *slog.Logger
	cur     atomic.Pointer[Config]
}

// NewManager 用初始配置创建管理器。path 供 Reload/Watch 使用；example 在自动
// 创建场景下保留备用（当前仅用于日志/占位，不参与热更新写入）。
func NewManager(initial *Config, path string, example []byte, log *slog.Logger) *Manager {
	m := &Manager{path: path, example: example, log: log}
	m.cur.Store(initial)
	return m
}

// Get 返回当前生效的配置快照（指针稳定，只读使用）。
func (m *Manager) Get() *Config { return m.cur.Load() }

// Reload 立即重新读取并校验配置文件；成功则原子替换并返回 nil，
// 失败保留旧配置并返回错误（内部已记录警告日志）。
func (m *Manager) Reload() error {
	if m.path == "" {
		return os.ErrNotExist
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		m.log.Warn("配置热更新读取失败，保留旧配置", "err", err)
		return err
	}
	nc, err := Parse(data)
	if err != nil {
		m.log.Warn("配置热更新校验失败，保留旧配置", "err", err)
		return err
	}
	old := m.cur.Load()
	if old != nil && old.Listen != nc.Listen {
		m.log.Warn("listen 变更需要重启生效，当前仍在原端口服务", "old", old.Listen, "new", nc.Listen)
	}
	m.cur.Store(nc)
	m.log.Info("配置已热更新", "config", m.path)
	return nil
}

// Watch 启动后台轮询（默认间隔 1s），文件内容变化时自动 Reload，直到 ctx 取消。
// 初始 hash 在调用时同步读取：保证 Watch 返回后、goroutine 启动前写入的文件
// 一定会被后续轮询检测到（否则存在"goroutine 启动前文件已变，初始 hash 直接
// 等于新内容，永不触发"的竞态）。
func (m *Manager) Watch(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	lastHash := fileHash(m.path)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h := fileHash(m.path)
				if h == "" || h == lastHash {
					continue
				}
				lastHash = h
				_ = m.Reload()
			}
		}
	}()
}

// fileHash 返回文件内容的 SHA-256（十六进制）；读取失败返回空串。
func fileHash(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
