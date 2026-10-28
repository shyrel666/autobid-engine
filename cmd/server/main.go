// Command server 是 AutoBid Engine 服务主入口：迁移 -> 启动恢复 -> 引擎 -> HTTP/SSE 控制面，支持信号优雅退出。
//
// 环境变量：
//
//	AUTOBID_DATABASE_URL   PostgreSQL 连接串（必填）
//	AUTOBID_HTTP_ADDR      监听地址，默认 127.0.0.1:8080
//	AUTOBID_API_TOKEN      非空时 /api 需要 Bearer 鉴权（监听非回环地址时强烈建议设置）
//	AUTOBID_WORKERS        每任务 Worker 槽数，默认 3
//	AUTOBID_RECORD_BODIES  为 1/true 时录制 LLM 原始请求/响应正文（不含请求头）
//	AUTOBID_WEB_DIR        前端工作台构建产物目录，默认 web/dist；目录内无 index.html 时不托管
//	模型密钥按各 Profile 的 api_key_env 从环境变量读取。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/guard"
	"github.com/autobid/autobid-engine/intercept"
	"github.com/autobid/autobid-engine/internal/harness"
	"github.com/autobid/autobid-engine/server"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func envBool(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	return v == "1" || v == "true" || v == "yes"
}

func run(logger *slog.Logger) error {
	dsn := os.Getenv("AUTOBID_DATABASE_URL")
	if dsn == "" {
		return errors.New("AUTOBID_DATABASE_URL is not set")
	}
	addr := os.Getenv("AUTOBID_HTTP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	token := os.Getenv("AUTOBID_API_TOKEN")
	if host, _, err := net.SplitHostPort(addr); err == nil && token == "" {
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			logger.Warn("listening on a non-loopback address without AUTOBID_API_TOKEN; the control API is unauthenticated", "addr", addr)
		}
	}
	workers, _ := strconv.Atoi(os.Getenv("AUTOBID_WORKERS"))
	webDir := os.Getenv("AUTOBID_WEB_DIR")
	if webDir == "" {
		webDir = "web/dist"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 引擎使用独立 Context：退出时先 StopAll（以 task_deleted 原因取消，在途 Intent 回 open 供下次重领），
	// 再取消它。若与信号 Context 共用，信号会先取消执行树，在途 Intent 将被误判为执行失败（blocked）。
	engineCtx, cancelEngine := context.WithCancel(context.Background())
	defer cancelEngine()

	// 1. 数据库与迁移
	openCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	pg, err := db.Open(openCtx, dsn)
	if err == nil {
		err = pg.Migrate(openCtx)
	}
	cancel()
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pg.Close()

	// 2. 控制面组件
	hub := server.NewHub()
	var recordBodies atomic.Bool
	recordBodies.Store(envBool("AUTOBID_RECORD_BODIES"))
	svc := server.NewLLMService(pg, server.LLMServiceConfig{
		OnActivity:   func(_ int64, a db.Activity) { hub.Publish(a) },
		OnLedgerErr:  func(err error) { logger.Warn("llm ledger write failed", "err", err) },
		RecordBodies: recordBodies.Load,
	})
	hostname, _ := os.Hostname()
	ic := intercept.New(pg, intercept.Options{
		Reviewer:   svc.Reviewer(),
		Waiter:     fmt.Sprintf("%s/%d", hostname, os.Getpid()),
		OnActivity: hub.Publish,
	})
	m := server.NewManager(pg)
	m.SetHookFactory(func(*server.Task) harness.HookRunner { return guard.New(ic) })
	e := server.NewEngine(m, svc.Resolve, server.Config{Workers: workers, OnActivity: hub.Publish})
	svc.Bind(e)

	// 3. 启动恢复：熔断状态、遗留审批、过期租约，然后恢复未终态任务的运行时。
	if err := svc.RestoreHealth(ctx); err != nil {
		logger.Warn("restore llm health failed", "err", err)
	}
	ic.Start(engineCtx) // 立即回收一次等待方已失联的 pending，此后周期回收
	if ids, err := pg.ReapExpiredIntents(ctx); err != nil {
		logger.Warn("reap expired intents failed", "err", err)
	} else if len(ids) > 0 {
		logger.Info("reaped expired intent leases", "explorations", ids)
	}
	e.Start(engineCtx)
	resumed := 0
	for _, status := range []string{db.TaskRunning, db.TaskPaused} {
		rows, err := pg.ListTasks(ctx, status, 10000)
		if err != nil {
			return fmt.Errorf("list %s tasks: %w", status, err)
		}
		for _, row := range rows {
			t, err := m.LoadTask(ctx, row.ID)
			if err == nil {
				err = e.Run(engineCtx, t)
			}
			if err != nil {
				logger.Warn("resume task failed", "task", row.ID, "err", err)
				continue
			}
			resumed++
		}
	}
	logger.Info("engine started", "resumed_tasks", resumed)

	// 4. HTTP/SSE 控制面。WriteTimeout 为 0：SSE 是长连接，单个处理器自行控制生命周期。
	api := server.NewAPI(engineCtx, m, e, svc, ic, hub, server.APIConfig{Token: token, WebDir: webDir})
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() {
		logger.Info("http listening", "addr", addr, "web_dir", webDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
		close(errc)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-errc:
		if err != nil {
			stop()
			e.StopAll()
			return fmt.Errorf("http: %w", err)
		}
	}

	// 5. 优雅退出：先停 HTTP（SSE 连接随 BaseContext 取消而结束），再停引擎运行时。
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown", "err", err)
	}
	e.StopAll()
	logger.Info("stopped")
	return nil
}
