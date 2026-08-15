package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"opgo/internal/audit"
	"opgo/internal/balance"
	"opgo/internal/config"
	"opgo/internal/proxy"
	"opgo/internal/store"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfgPath := flag.String("config", config.DefaultConfigPath(), "配置文件路径")
	listen := flag.String("listen", "", "监听地址（覆盖配置文件）")
	dbPath := flag.String("db", config.DefaultDBPath(), "sqlite 数据库路径")
	auditMode := flag.Bool("audit", false, "运行密钥防泄露自检后退出（不修改任何配置与数据）")
	flag.Parse()

	if *auditMode {
		cfg, err := config.LoadStrict(*cfgPath, logger)
		if err != nil {
			logger.Error("审计前置检查失败", "err", err)
			os.Exit(1)
		}
		if err := audit.Run(cfg, indexHTML, logger); err != nil {
			logger.Error("审计未通过", "err", err)
			os.Exit(1)
		}
		logger.Info("审计全部通过")
		return
	}

	cfg, err := config.Load(*cfgPath, exampleConfig, logger)
	if err != nil {
		logger.Error("配置加载失败", "err", err)
		os.Exit(1)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 可热更新的配置：后台轮询配置文件，变更（users/pricing/limits/master_key 等）即时生效；
	// 仅 listen 变更需重启，检测到时会记录警告。
	mgr := config.NewManager(cfg, *cfgPath, exampleConfig, logger)
	mgr.Watch(ctx, time.Second)

	st, err := store.Open(*dbPath)
	if err != nil {
		logger.Error("数据库打开失败", "err", err, "db", *dbPath)
		os.Exit(1)
	}
	defer st.Close()

	bal := balance.New(func() *config.Config { return mgr.Get() }, 10*time.Second, logger)
	bal.Start(ctx)

	handler := proxy.New(mgr, st, indexHTML, bal, logger)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()

	logger.Info("opgo 启动", "listen", cfg.Listen, "config", *cfgPath, "db", *dbPath)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("服务异常退出", "err", err)
		os.Exit(1)
	}
	logger.Info("opgo 已退出")
}
