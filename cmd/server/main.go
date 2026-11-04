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
//	AUTOBID_EXPORT_DIR     图表渲染产物、上传模板与导出成稿目录，默认 data/exports（按租户 / 任务隔离，已加入 .gitignore）
//	AUTOBID_FILE_PROCESSING_ENABLED 为 1/true 时启用招标原件留存（F1 固定处理）；默认关闭
//	AUTOBID_FILEWORK_DIR   原件内容寻址存储目录，默认 data/filework（已加入 .gitignore）
//	AUTOBID_OFFICE_ENGINE  导出文档引擎：auto（默认，WPS 优先、Word 备选）/ wps / word；启动时用小样本实际刷新自检
//	AUTOBID_OFFICE_TIMEOUT_SECONDS  文档引擎单次刷新超时（默认 180）；同一引擎同一时间只处理一个文档
//	AUTOBID_MMDC_PATH      mermaid-cli（mmdc 或其 cli.js）路径；未设置时查 PATH，均不可用或自检失败时图表为 unrendered
//	AUTOBID_MMDC_BROWSER   mmdc 使用的本机 Chrome / Chromium 可执行文件（puppeteer executablePath）
//	AUTOBID_MMDC_TIMEOUT_SECONDS / AUTOBID_MMDC_CONCURRENCY  单次渲染超时（默认 90）与并发上限（默认 2）
//	运行能力（文档引擎、mmdc、OCR、embedding、reranker）在启动时用固定小样本实际自检，状态见 GET /api/runtime/tools
//	模型密钥优先使用 Profile 加密保存的 Key，否则按 api_key_env 读取环境变量。
package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/autobid/autobid-engine/retrieval"
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
	"github.com/autobid/autobid-engine/export"
	"github.com/autobid/autobid-engine/filework"
	"github.com/autobid/autobid-engine/guard"
	"github.com/autobid/autobid-engine/ingest"
	"github.com/autobid/autobid-engine/intercept"
	"github.com/autobid/autobid-engine/internal/harness"
	"github.com/autobid/autobid-engine/server"
	"github.com/autobid/autobid-engine/toolhealth"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && v > 0 {
		return v
	}
	return def
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
	embedding, reranker, err := retrieval.ModelsFromEnv()
	if err != nil {
		return err
	}
	retriever := retrieval.New(pg, embedding, reranker)
	hub := server.NewHub()
	var recordBodies atomic.Bool
	recordBodies.Store(envBool("AUTOBID_RECORD_BODIES"))
	exportDir := os.Getenv("AUTOBID_EXPORT_DIR")
	if exportDir == "" {
		exportDir = "data/exports"
	}
	// 启动时并行自检文档引擎（WPS / Word）与 mermaid-cli 并记日志；不可用时 PDF 任务明确失败、DOCX 标注目录页码未刷新，
	// 图表保持 unrendered。OCR / embedding / reranker 在后台自检（见下方 ProbeRuntime）。
	runtimeHealth := toolhealth.New()
	exporter := export.NewServiceFromEnv(pg, exportDir, logger, runtimeHealth)
	svc := server.NewLLMService(pg, server.LLMServiceConfig{
		MultiRetrieval: envBool("AUTOBID_RETRIEVAL_MULTI_AGENT"),
		Retrieval:      retriever,
		Charts:         exporter,
		Runtime:        runtimeHealth,
		OnActivity:     func(_ int64, a db.Activity) { hub.Publish(a) },
		OnLedgerErr:    func(err error) { logger.Warn("llm ledger write failed", "err", err) },
		RecordBodies:   recordBodies.Load,
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
	// 扫描件 OCR：AUTOBID_OCR_URL（HTTP 服务）→ PATH 中的 tesseract → 不可用（扫描件上传返回 422）。
	ocr := ingest.DefaultOCR()
	server.ProbeRuntime(engineCtx, runtimeHealth, ocr, embedding, reranker)
	// 导出执行者：先回收租约已过期的 running 任务（上次进程中断），再轮询 queued 任务。
	runner := &export.Runner{Service: exporter, Owner: fmt.Sprintf("%s/%d", hostname, os.Getpid()), Logger: logger,
		Workers: envInt("AUTOBID_EXPORT_WORKERS", 2),
		Lease:   export.OfficeTimeout() + 60*time.Second,
		OnChange: func(j *db.ExportJob, summary string) {
			hub.Publish(db.Activity{TaskID: j.TaskID, Worker: "export", Kind: "state", At: time.Now(), Summary: summary})
			e.WakeAgentJobs() // 终态事件已在作业事务内持久化；这里只加速等待中 Intent 的唤醒扫描
		}}
	runner.Start(engineCtx)
	// 图表渲染执行者（S3）：领取 chart_render_jobs，在数据库事务外调用 mmdc；租约过期的作业在领取时回收。
	chartRunner := &export.ChartRunner{Service: exporter, Owner: fmt.Sprintf("%s/%d/charts", hostname, os.Getpid()), Logger: logger,
		Workers: envInt("AUTOBID_CHART_WORKERS", 2),
		Lease:   export.MMDCTimeout() + 60*time.Second,
		OnChange: func(j *db.ChartRenderJob, summary string) {
			hub.Publish(db.Activity{TaskID: j.TaskID, Worker: "chart-render", Kind: "state", At: time.Now(), Summary: summary})
			e.WakeAgentJobs()
		}}
	exporter.ChartWake = chartRunner.Wake
	chartRunner.Start(engineCtx)
	var fileStore *filework.Store
	if envBool("AUTOBID_FILE_PROCESSING_ENABLED") {
		dir := os.Getenv("AUTOBID_FILEWORK_DIR")
		if dir == "" {
			dir = filework.DefaultDir
		}
		fs, err := filework.NewStore(pg, dir)
		if err != nil {
			logger.Warn("file processing disabled: filework store unavailable", "err", err)
		} else {
			fileStore = fs
			logger.Info("file processing enabled", "dir", fs.Root())
			// 延迟 GC：只清理 24 小时前、且没有任何 task_file_objects 行引用的孤立对象。
			go func() {
				prune := func() {
					n, err := fs.Cleanup(engineCtx, db.DefaultTenant, time.Now().Add(-24*time.Hour))
					if err != nil {
						logger.Warn("filework cleanup failed", "err", err)
					} else if n > 0 {
						logger.Info("filework orphan objects removed", "count", n)
					}
				}
				prune()
				ticker := time.NewTicker(6 * time.Hour)
				defer ticker.Stop()
				for {
					select {
					case <-engineCtx.Done():
						return
					case <-ticker.C:
						prune()
					}
				}
			}()
		}
	}
	var fileRunner *filework.Runner
	fileRunnerDone := make(chan struct{})
	if fileStore != nil {
		fileRunner = &filework.Runner{Processor: &filework.Processor{DB: pg, Store: fileStore, OCR: ocr}, Owner: fmt.Sprintf("%s/%d/files", hostname, os.Getpid()), Workers: envInt("AUTOBID_FILE_WORKERS", 2),
			OnChange: func(j *db.FixedFileJob) {
				hub.Publish(db.Activity{TaskID: j.TaskID, Worker: "file-processing", Kind: "state", At: time.Now(), Summary: fmt.Sprintf("材料处理 #%d：%s", j.ID, j.Status)})
				e.WakeAgentJobs()
			}}
		go func() { defer close(fileRunnerDone); fileRunner.Run(engineCtx) }()
	} else {
		close(fileRunnerDone)
	}
	api := server.NewAPI(engineCtx, m, e, svc, ic, hub, server.APIConfig{Token: token, WebDir: webDir, OCR: ocr, Embedder: db.DefaultEmbedder(),
		Retrieval: retriever, Export: exporter, ExportRunner: runner, FileStore: fileStore, FileRunner: fileRunner, Runtime: runtimeHealth})
	go api.RunSectionJobs(engineCtx)
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() {
		logger.Info("http listening", "addr", addr, "web_dir", webDir, "ocr", ocr.Name())
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
	cancelEngine()
	select {
	case <-fileRunnerDone:
	case <-shutdownCtx.Done():
		logger.Warn("fixed file workers still stopping; leases will recover on restart")
	}
	logger.Info("stopped")
	return nil
}
