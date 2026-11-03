package server

// HTTP / SSE 控制面（标准库 net/http + Go 1.22 路由模式）。
// 鉴权：配置了 Token 时所有 /api 请求必须携带 "Authorization: Bearer <token>"
// （EventSource 无法设置请求头，SSE 端点额外接受 ?token=）。

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/compliance"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/evidence"
	"github.com/autobid/autobid-engine/export"
	"github.com/autobid/autobid-engine/filework"
	"github.com/autobid/autobid-engine/ingest"
	"github.com/autobid/autobid-engine/intercept"
	"github.com/autobid/autobid-engine/retrieval"
	"github.com/autobid/autobid-engine/toolhealth"
)

type APIConfig struct {
	Token string
	// SSEHeartbeat 是 SSE 保活注释间隔，默认 15s。
	SSEHeartbeat time.Duration
	// WebDir 为前端构建产物目录（含 index.html）时托管静态文件；为空或不存在时不托管。
	WebDir string
	// OCR 识别扫描件与无文本层 PDF 页面；为 nil 时扫描件上传返回 422。
	OCR ingest.OCRProvider
	// Embedder 为知识库分块与检索生成向量；为 nil 时使用 db.DefaultEmbedder()。
	Embedder  db.Embedder
	Retrieval *retrieval.Service
	// SectionWriter 执行章节 AI 撰写；为 nil 时使用任务 Worker 模型（无可用 Profile 时派发返回 503）。
	SectionWriter SectionWriter
	// ComplianceVerifier 是响应矩阵核验器；为 nil 时使用确定性 compliance.RuleVerifier（LLM 核验未接入生产）。
	ComplianceVerifier compliance.Verifier
	// Export 是图表渲染与导出服务（导出目录、mmdc、文档引擎）；为 nil 时使用系统临时目录下的 autobid-exports，不探测外部程序。
	Export *export.Service
	// Evidence 是 D2 企业证据原件存储（AUTOBID_EVIDENCE_DIR，默认 data/evidence）。
	Evidence *evidence.Store
	// FileStore 是任务招标原件存储（AUTOBID_FILEWORK_DIR）；为 nil 时保持旧上传行为、不留存原件。
	FileStore  *filework.Store
	FileRunner *filework.Runner
	// ExportRunner 非空时创建导出任务后立即唤醒执行者（否则等待其轮询）。
	ExportRunner *export.Runner
	// Runtime 是本机运行能力状态（GET /api/runtime/tools）；为 nil 时按 Export 现状生成，其余能力记为未配置。
	Runtime *toolhealth.Registry
}

type API struct {
	sectionJobPumps sync.Map
	sectionJobSlots chan struct{}
	runCtx          context.Context // 任务运行时的父 Context（服务生命周期）
	m               *Manager
	e               *Engine
	llm             *LLMService
	ic              *intercept.Interceptor
	hub             *Hub
	pg              *db.DB
	cfg             APIConfig
}

func NewAPI(runCtx context.Context, m *Manager, e *Engine, svc *LLMService, ic *intercept.Interceptor, hub *Hub, cfg APIConfig) *API {
	if cfg.SSEHeartbeat <= 0 {
		cfg.SSEHeartbeat = 15 * time.Second
	}
	if cfg.Retrieval == nil {
		cfg.Retrieval = retrieval.New(m.DB(), nil, nil)
	}
	if cfg.Export == nil {
		cfg.Export = export.NewService(m.DB(), filepath.Join(os.TempDir(), "autobid-exports"), "未配置导出服务：没有探测文档引擎与 mmdc")
	}
	if cfg.Evidence == nil {
		cfg.Evidence = evidence.FromEnv()
	}
	if cfg.Runtime == nil {
		cfg.Runtime = fallbackRuntime(cfg.Export)
	}
	return &API{runCtx: runCtx, m: m, e: e, llm: svc, ic: ic, hub: hub, pg: m.DB(), cfg: cfg, sectionJobSlots: make(chan struct{}, 4)}
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/tasks/{id}/section-jobs", a.withTask(a.listSectionJobs))
	mux.HandleFunc("POST /api/tasks/{id}/section-jobs/{jobID}/cancel", a.withTask(a.cancelSectionJob))
	mux.HandleFunc("POST /api/tasks/{id}/files", a.withTask(a.uploadProcessingFile))
	mux.HandleFunc("GET /api/tasks/{id}/materials", a.withTask(a.listTaskMaterials))
	mux.HandleFunc("POST /api/tasks/{id}/materials", a.withTask(a.saveTaskMaterial))
	mux.HandleFunc("POST /api/tasks/{id}/file-jobs", a.withTask(a.enqueueFixedJob))
	mux.HandleFunc("GET /api/tasks/{id}/file-jobs", a.withTask(a.listFixedJobs))
	mux.HandleFunc("GET /api/tasks/{id}/file-jobs/{jobID}", a.withTask(a.getFixedJob))
	mux.HandleFunc("POST /api/tasks/{id}/file-jobs/{jobID}/cancel", a.withTask(a.cancelFixedJob))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.pg.PingContext(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	// 任务生命周期
	mux.HandleFunc("GET /api/tasks", a.listTasks)
	mux.HandleFunc("POST /api/tasks", a.createTask)
	mux.HandleFunc("GET /api/tasks/{id}", a.withTask(a.getTask))
	mux.HandleFunc("DELETE /api/tasks/{id}", a.deleteTask)
	mux.HandleFunc("POST /api/tasks/{id}/start", a.withTask(a.startTask))
	mux.HandleFunc("POST /api/tasks/{id}/pause", a.withTask(a.pauseTask))
	mux.HandleFunc("POST /api/tasks/{id}/resume", a.withTask(a.resumeTask))
	mux.HandleFunc("POST /api/tasks/{id}/revive", a.withTask(a.reviveTask))
	mux.HandleFunc("GET /api/tasks/{id}/coverage", a.withTask(a.coverage))
	mux.HandleFunc("POST /api/tasks/{id}/goals", a.withTask(a.addGoal))
	mux.HandleFunc("POST /api/tasks/{id}/hints", a.withTask(a.addHint))

	// 单工作槽控制
	mux.HandleFunc("POST /api/tasks/{id}/works/{intentID}/control", a.withTask(a.controlWork))
	mux.HandleFunc("POST /api/tasks/{id}/works/{intentID}/steer", a.withTask(a.steerWork))
	mux.HandleFunc("POST /api/tasks/{id}/works/{intentID}/message", a.withTask(a.messageWork))

	// 推演图与产出物
	mux.HandleFunc("GET /api/tasks/{id}/overview", a.withTask(a.overview))
	mux.HandleFunc("GET /api/tasks/{id}/graph", a.withTask(a.taskGraph))
	mux.HandleFunc("GET /api/tasks/{id}/nodes/{nodeID}", a.withTask(a.nodeDetail))
	mux.HandleFunc("GET /api/tasks/{id}/findings", a.withTask(a.findings))
	mux.HandleFunc("GET /api/tasks/{id}/assets", a.withTask(a.assets))
	mux.HandleFunc("GET /api/assets/{assetID}/edges", a.assetEdges)
	mux.HandleFunc("GET /api/tasks/{id}/report", a.withTask(a.report))

	// 招标文档与企业资料库
	mux.HandleFunc("POST /api/tasks/{id}/documents", a.withTask(a.uploadDocument))
	mux.HandleFunc("GET /api/tasks/{id}/files", a.withTask(a.listProcessingFiles))
	mux.HandleFunc("POST /api/tasks/{id}/files/{fileID}/import", a.withTask(a.importProcessingFile))
	mux.HandleFunc("POST /api/tasks/{id}/documents/{docID}/reparse", a.withTask(a.reparseDocument))
	mux.HandleFunc("GET /api/tasks/{id}/file-processing-records", a.withTask(a.listProcessingRecords))
	mux.HandleFunc("GET /api/tasks/{id}/file-processing-records/{recordID}", a.withTask(a.getProcessingRecord))
	mux.HandleFunc("POST /api/tasks/{id}/file-processing-records/{recordID}/adopt", a.withTask(a.adoptProcessingRecord))
	mux.HandleFunc("GET /api/tasks/{id}/documents", a.withTask(a.listDocuments))
	mux.HandleFunc("POST /api/company/credentials", a.upsertCredentials)
	mux.HandleFunc("GET /api/company/credentials", a.listCredentials)
	mux.HandleFunc("PUT /api/company/credentials/{credID}", a.updateCredential)
	mux.HandleFunc("DELETE /api/company/credentials/{credID}", a.deleteCredential)
	// D2 证据可采性、承诺台账与报价（server/d2.go）
	a.registerD2Routes(mux)

	// 企业方案知识库（Phase 4）
	mux.HandleFunc("GET /api/knowledge/documents", a.listKnowledge)
	mux.HandleFunc("POST /api/knowledge/documents", a.uploadKnowledge)
	mux.HandleFunc("GET /api/knowledge/documents/{kid}", a.getKnowledge)
	mux.HandleFunc("DELETE /api/knowledge/documents/{kid}", a.deleteKnowledge)
	mux.HandleFunc("GET /api/knowledge/search", a.searchKnowledge)
	mux.HandleFunc("GET /api/knowledge/indexes", a.listKnowledgeIndexes)
	mux.HandleFunc("POST /api/knowledge/indexes", a.buildKnowledgeIndex)
	mux.HandleFunc("POST /api/knowledge/indexes/{indexID}/activate", a.activateKnowledgeIndex)
	mux.HandleFunc("GET /api/retrieval/runs/{runID}", a.getRetrievalRun)
	mux.HandleFunc("POST /api/tasks/{id}/research", a.withTask(a.researchTask))
	mux.HandleFunc("GET /api/tasks/{id}/retrieval-runs", a.withTask(a.taskRetrievalRuns))
	mux.HandleFunc("POST /api/retrieval/runs/{runID}/cancel", a.cancelRetrievalRun)
	mux.HandleFunc("GET /api/retrieval/runs/{runID}/evidence", a.getRetrievalEvidence)
	mux.HandleFunc("POST /api/retrieval/runs/{runID}/evidence/{evidenceID}/verify", a.verifyRetrievalEvidence)

	// 投标要求清单、技术标大纲审批与章节写作（Phase 5）
	a.registerProposalRoutes(mux)

	// 双层响应核验矩阵、全文一致性核验与导出就绪度（Phase 6A）
	a.registerComplianceRoutes(mux)
	// Phase 6B：章节图表（server/charts.go）与锁定版本导出（server/export.go）
	a.registerChartRoutes(mux)
	a.registerRuntimeRoutes(mux)
	a.registerAgentJobRoutes(mux)
	a.registerExportRoutes(mux)
	// D3：章节故事板（Pink Team 评审）、受控引用渲染与句子级写作核验（server/d3.go）
	a.registerD3Routes(mux)

	// LLM 配置与配额链
	mux.HandleFunc("GET /api/llm/profiles", a.listProfiles)
	mux.HandleFunc("POST /api/llm/profiles", a.createProfile)
	mux.HandleFunc("GET /api/llm/profiles/{pid}", a.getProfile)
	mux.HandleFunc("PUT /api/llm/profiles/{pid}", a.updateProfile)
	mux.HandleFunc("DELETE /api/llm/profiles/{pid}", a.deleteProfile)
	mux.HandleFunc("PUT /api/llm/bindings/{agent}", a.bindAgent)
	mux.HandleFunc("GET /api/llm/health", a.llmHealth)
	mux.HandleFunc("GET /api/llm/bindings", a.getBindings)
	mux.HandleFunc("POST /api/llm/profiles/active", a.activateProfile)
	mux.HandleFunc("GET /api/llm/settings", a.getLLMSettings)
	mux.HandleFunc("PUT /api/llm/settings", a.putLLMSettings)
	mux.HandleFunc("GET /api/llm/pool", a.poolStatus)
	mux.HandleFunc("POST /api/llm/pool/reset", a.resetLLMPool)
	mux.HandleFunc("POST /api/llm/models", a.listModels)
	mux.HandleFunc("POST /api/llm/test", a.testLLM)
	mux.HandleFunc("GET /api/llm/records", a.listLLMRecords)
	mux.HandleFunc("GET /api/llm/records/{recordID}", a.getLLMRecord)
	mux.HandleFunc("DELETE /api/llm/records", a.deleteLLMRecords)
	mux.HandleFunc("GET /api/system/catalog", a.systemCatalog)
	mux.HandleFunc("GET /api/tasks/{id}/llm-chain", a.withTask(a.getChain))
	mux.HandleFunc("PUT /api/tasks/{id}/llm-chain", a.withTask(a.setChain))
	mux.HandleFunc("POST /api/tasks/{id}/llm-chain/reset", a.withTask(a.resetChain))
	mux.HandleFunc("GET /api/tasks/{id}/llm-usage", a.withTask(a.llmUsage))

	// 拦截审批与控制面
	mux.HandleFunc("GET /api/intercepts", a.listIntercepts)
	mux.HandleFunc("POST /api/intercepts/{pendingID}/decide", a.decideIntercept)
	mux.HandleFunc("GET /api/tasks/{id}/intercept-audit", a.withTask(a.interceptAudit))
	mux.HandleFunc("GET /api/intercept/rules", a.listInterceptRules)
	mux.HandleFunc("POST /api/intercept/rules", a.createInterceptRule)
	mux.HandleFunc("PUT /api/intercept/rules/{ruleID}", a.updateInterceptRule)
	mux.HandleFunc("DELETE /api/intercept/rules/{ruleID}", a.deleteInterceptRule)
	mux.HandleFunc("GET /api/intercept/config", a.getInterceptConfig)
	mux.HandleFunc("PUT /api/intercept/config", a.putInterceptConfig)
	mux.HandleFunc("GET /api/intercept/approvals", a.listInterceptApprovals)

	// 实时活动流
	mux.HandleFunc("GET /api/tasks/{id}/events", a.withTask(a.events))

	// 前端工作台静态产物（web/dist）与 SPA 路由回退
	if spa := newSPAHandler(a.cfg.WebDir); spa != nil {
		mux.Handle("/", spa)
	}

	return a.auth(mux)
}

// ---------------------------------------------------------------------------
// 通用
// ---------------------------------------------------------------------------

func (a *API) auth(next http.Handler) http.Handler {
	if a.cfg.Token == "" {
		return next
	}
	want := []byte(a.cfg.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" && strings.HasSuffix(r.URL.Path, "/events") {
			got = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			writeError(w, http.StatusUnauthorized, errors.New("missing or invalid bearer token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

type httpError struct {
	status int
	err    error
}

func (e *httpError) Error() string { return e.err.Error() }

func badRequest(format string, args ...any) error {
	return &httpError{http.StatusBadRequest, fmt.Errorf(format, args...)}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// fail 把领域错误映射为 HTTP 状态码。
func fail(w http.ResponseWriter, err error) {
	var he *httpError
	if errors.As(err, &he) {
		writeError(w, he.status, he.err)
		return
	}
	writeError(w, errorStatus(err), err)
}

func errorStatus(err error) int {
	var archiveErr *ingest.ArchiveError
	if errors.As(err, &archiveErr) {
		return http.StatusUnprocessableEntity
	}
	switch {
	case errors.Is(err, db.ErrInvalidMaterial):
		return http.StatusBadRequest
	case errors.Is(err, db.ErrParseRevisionConflict), errors.Is(err, db.ErrFileProcessingConflict):
		return http.StatusConflict
	case errors.Is(err, db.ErrInvalidParseRevision), errors.Is(err, filework.ErrHashMismatch):
		return http.StatusUnprocessableEntity
	case errors.Is(err, db.ErrNotFound), errors.Is(err, intercept.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, errWorkControlConflict), errors.Is(err, intercept.ErrAlreadyDecided), errors.Is(err, db.ErrCredentialConflict):
		return http.StatusConflict
	case errors.Is(err, intercept.ErrWaiterGone):
		return http.StatusGone
	case errors.Is(err, db.ErrInvalidDigest), errors.Is(err, db.ErrInvalidEdge), errors.Is(err, db.ErrForeignNode),
		errors.Is(err, db.ErrInvalidKnowledge), errors.Is(err, ingest.ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, ingest.ErrUnsupportedFormat):
		return http.StatusUnsupportedMediaType
	case errors.Is(err, ingest.ErrOCRUnavailable), errors.Is(err, ingest.ErrNoText),
		errors.Is(err, ingest.ErrSpreadsheetLimit), errors.Is(err, ingest.ErrSpreadsheetInvalid):
		return http.StatusUnprocessableEntity
	case errors.Is(err, filework.ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, filework.ErrUnsafePath), errors.Is(err, filework.ErrHashMismatch):
		return http.StatusUnprocessableEntity
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

func decodeBody(r *http.Request, v any) error {
	return decodeBodyLimit(r, v, 1<<20)
}

func decodeBodyLimit(r *http.Request, v any, limit int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

func pathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, badRequest("invalid %s %q", name, r.PathValue(name))
	}
	return id, nil
}

// withTask 解析 {id} 并加载任务句柄。
func (a *API) withTask(h func(w http.ResponseWriter, r *http.Request, t *Task)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r, "id")
		if err != nil {
			fail(w, err)
			return
		}
		t, err := a.m.LoadTask(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		h(w, r, t)
	}
}

// ---------------------------------------------------------------------------
// 任务生命周期
// ---------------------------------------------------------------------------

type taskView struct {
	*db.Task
	Goal     string `json:"goal"`
	Running  bool   `json:"running"`
	Settling bool   `json:"settling"`
	Inflight int64  `json:"inflight"`
}

func (a *API) view(ctx context.Context, t *Task) (*taskView, error) {
	row, err := a.pg.GetTask(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	return &taskView{Task: row, Goal: t.Goal, Running: a.e.IsRunning(t.ID), Settling: a.e.isSettling(t.ID),
		Inflight: a.e.inflightCount(t.ID)}, nil
}

func (a *API) listTasks(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := a.pg.ListTasks(r.Context(), r.URL.Query().Get("status"), limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

type scopeReq struct {
	TenderNo string `json:"tender_no"`
	LotNo    string `json:"lot_no"`
}

type createTaskReq struct {
	Name                 string     `json:"name"`
	Description          string     `json:"description"`
	Goal                 string     `json:"goal"`
	Goals                []string   `json:"goals"`
	TimeoutSeconds       int        `json:"timeout_seconds"`
	PlanHeartbeatSeconds int        `json:"plan_heartbeat_seconds"`
	Scopes               []scopeReq `json:"scopes"`
	SourceTaskIDs        []int64    `json:"source_task_ids"`
	LLMProfileIDs        []int64    `json:"llm_profile_ids"`
	Start                bool       `json:"start"`
}

func (a *API) createTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskReq
	if err := decodeBody(r, &req); err != nil {
		fail(w, err)
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Goal) == "" {
		fail(w, badRequest("name and goal are required"))
		return
	}
	if req.TimeoutSeconds < 0 {
		fail(w, badRequest("timeout_seconds must be >= 0"))
		return
	}
	in := db.CreateTaskInput{Name: req.Name, Description: req.Description, Goal: req.Goal,
		TimeoutSeconds: req.TimeoutSeconds, PlanHeartbeatSeconds: req.PlanHeartbeatSeconds, SourceTaskIDs: req.SourceTaskIDs}
	for _, s := range req.Scopes {
		if strings.TrimSpace(s.TenderNo) == "" {
			fail(w, badRequest("scope tender_no is required"))
			return
		}
		in.Scopes = append(in.Scopes, db.TaskScope{TenderNo: s.TenderNo, LotNo: s.LotNo})
	}
	t, err := a.m.CreateTask(r.Context(), in, req.Goals...)
	if err != nil {
		fail(w, err)
		return
	}
	if len(req.LLMProfileIDs) > 0 {
		if err := a.pg.SetTaskLLMChain(r.Context(), t.ID, req.LLMProfileIDs); err != nil {
			fail(w, err)
			return
		}
	}
	if req.Start {
		if err := a.e.Run(a.runCtx, t); err != nil {
			fail(w, err)
			return
		}
	}
	v, err := a.view(r.Context(), t)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (a *API) getTask(w http.ResponseWriter, r *http.Request, t *Task) {
	v, err := a.view(r.Context(), t)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) respondTask(w http.ResponseWriter, r *http.Request, t *Task, err error) {
	if err != nil {
		fail(w, err)
		return
	}
	a.getTask(w, r, t)
}

func (a *API) startTask(w http.ResponseWriter, r *http.Request, t *Task) {
	a.respondTask(w, r, t, a.e.Run(a.runCtx, t))
}

func (a *API) pauseTask(w http.ResponseWriter, r *http.Request, t *Task) {
	err := a.e.Pause(r.Context(), t, agent.AbortPausedByUser)
	if err == nil && a.cfg.FileRunner != nil {
		err = a.cfg.FileRunner.StopTask(r.Context(), t.ID)
	}
	a.respondTask(w, r, t, err)
}

func (a *API) resumeTask(w http.ResponseWriter, r *http.Request, t *Task) {
	a.respondTask(w, r, t, a.e.Resume(r.Context(), t))
}

// reviveTask 让终态任务重新准入；运行时未启动时一并启动。
func (a *API) reviveTask(w http.ResponseWriter, r *http.Request, t *Task) {
	ok, err := a.e.Revive(r.Context(), t)
	if err == nil && !ok {
		err = &httpError{http.StatusConflict, errors.New("task is not in a terminal state")}
	}
	if err == nil {
		err = a.e.Run(a.runCtx, t)
	}
	a.respondTask(w, r, t, err)
}

func (a *API) deleteTask(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, err)
		return
	}
	a.e.StopTask(id)
	if err := a.pg.SetTaskPaused(r.Context(), id, true); err != nil {
		fail(w, err)
		return
	}
	if a.cfg.FileRunner != nil {
		if err := a.cfg.FileRunner.StopTask(r.Context(), id); err != nil {
			fail(w, err)
			return
		}
	}
	if err := a.pg.DeleteTask(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	a.m.Forget(id)
	a.llm.InvalidateTaskAgents(id)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) coverage(w http.ResponseWriter, r *http.Request, t *Task) {
	c, err := a.pg.TaskCoverageWithSources(r.Context(), t.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// addGoal 追加目标；终态任务自动复活（新增目标意味着需要继续推演）。
func (a *API) addGoal(w http.ResponseWriter, r *http.Request, t *Task) {
	var req struct {
		Summary string `json:"summary"`
	}
	if err := decodeBody(r, &req); err != nil {
		fail(w, err)
		return
	}
	if strings.TrimSpace(req.Summary) == "" {
		fail(w, badRequest("summary is required"))
		return
	}
	id, err := a.m.AddGoal(r.Context(), t, req.Summary)
	if err != nil {
		fail(w, err)
		return
	}
	revived, err := a.e.Revive(r.Context(), t)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"goal_id": id, "revived": revived})
}

func (a *API) addHint(w http.ResponseWriter, r *http.Request, t *Task) {
	var req struct {
		Text    string  `json:"text"`
		Anchors []int64 `json:"anchors"`
	}
	if err := decodeBody(r, &req); err != nil {
		fail(w, err)
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		fail(w, badRequest("text is required"))
		return
	}
	id, err := a.m.AddHint(r.Context(), t, req.Text, req.Anchors)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"hint_id": id})
}

// ---------------------------------------------------------------------------
// 单工作槽控制
// ---------------------------------------------------------------------------

// taskIntent 解析 {intentID} 并确认它是该任务的 Intent。
func (a *API) taskIntent(r *http.Request, t *Task) (int64, error) {
	id, err := pathID(r, "intentID")
	if err != nil {
		return 0, err
	}
	n, err := t.Store.GetNode(r.Context(), id)
	if err != nil {
		return 0, err
	}
	if n.Kind != db.KindIntent {
		return 0, badRequest("node %d is not an intent", id)
	}
	return id, nil
}

func (a *API) controlWork(w http.ResponseWriter, r *http.Request, t *Task) {
	id, err := a.taskIntent(r, t)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := decodeBody(r, &req); err != nil {
		fail(w, err)
		return
	}
	if req.Action != "pause" && req.Action != "cancel" {
		fail(w, badRequest("action must be pause or cancel"))
		return
	}
	if err := a.e.ControlWork(r.Context(), id, req.Action); err != nil {
		fail(w, err)
		return
	}
	n, err := t.Store.GetNode(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (a *API) steerWork(w http.ResponseWriter, r *http.Request, t *Task) {
	id, err := a.taskIntent(r, t)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Message string `json:"message"`
	}
	if err := decodeBody(r, &req); err != nil {
		fail(w, err)
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		fail(w, badRequest("message is required"))
		return
	}
	if err := a.e.SteerWork(id, req.Message); err != nil {
		fail(w, &httpError{http.StatusConflict, err})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true})
}

func (a *API) messageWork(w http.ResponseWriter, r *http.Request, t *Task) {
	id, err := a.taskIntent(r, t)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}
	if err := decodeBody(r, &req); err != nil {
		fail(w, err)
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		fail(w, badRequest("message is required"))
		return
	}
	if err := a.e.SendMessage(r.Context(), t, id, req.RequestID, req.Message); err != nil {
		fail(w, &httpError{http.StatusConflict, err})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"resumed": true})
}

// ---------------------------------------------------------------------------
// 推演图与产出物
// ---------------------------------------------------------------------------

func (a *API) overview(w http.ResponseWriter, r *http.Request, t *Task) {
	o, err := t.Store.Overview(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	if c, err := t.Assets.Coverage(r.Context()); err == nil {
		o.Coverage = &c
	}
	writeJSON(w, http.StatusOK, o)
}

func (a *API) nodeDetail(w http.ResponseWriter, r *http.Request, t *Task) {
	id, err := pathID(r, "nodeID")
	if err != nil {
		fail(w, err)
		return
	}
	n, err := t.Store.GetNode(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	edges, err := t.Store.NodeEdges(r.Context(), id, 500)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": n, "edges": edges})
}

func (a *API) taskGraph(w http.ResponseWriter, r *http.Request, t *Task) {
	nodes, err := t.Store.AllNodes(r.Context(), 1000)
	if err != nil {
		fail(w, err)
		return
	}
	edges, err := t.Store.Edges(r.Context(), 2000)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "edges": edges})
}

func (a *API) findings(w http.ResponseWriter, r *http.Request, t *Task) {
	fs, err := a.pg.ListFindings(r.Context(), t.ID, r.URL.Query().Get("category"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fs)
}

func (a *API) assets(w http.ResponseWriter, r *http.Request, t *Task) {
	q := r.URL.Query()
	var types []string
	if v := q.Get("types"); v != "" {
		types = strings.Split(v, ",")
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	as, err := t.Assets.ListScopedAssets(r.Context(), types, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, as)
}

func (a *API) assetEdges(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "assetID")
	if err != nil {
		fail(w, err)
		return
	}
	q := r.URL.Query()
	outgoing := q.Get("dir") != "in"
	edges, err := db.NewAssetStore(a.pg, 0).EntityEdgesOf(r.Context(), id, q.Get("rel"), outgoing, 500)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, edges)
}

// ---------------------------------------------------------------------------
// LLM 配置
// ---------------------------------------------------------------------------

func (a *API) listProfiles(w http.ResponseWriter, r *http.Request) {
	ps, err := a.pg.ListLLMProfiles(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	for _, p := range ps {
		a.profileView(p)
	}
	if ps == nil {
		ps = []*db.LLMProfile{}
	}
	writeJSON(w, http.StatusOK, ps)
}

func validateProfile(p *db.LLMProfile) error {
	if err := p.Validate(); err != nil {
		return badRequest("%s", err)
	}
	return nil
}

func (a *API) createProfile(w http.ResponseWriter, r *http.Request) {
	p, err := a.profileInput(r, 0, false)
	if err != nil {
		fail(w, err)
		return
	}
	id, err := a.pg.CreateLLMProfile(r.Context(), *p)
	if err != nil {
		fail(w, err)
		return
	}
	a.llm.InvalidateProviders()
	created, err := a.pg.GetLLMProfile(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a.profileView(created))
}

func (a *API) getProfile(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "pid")
	if err != nil {
		fail(w, err)
		return
	}
	p, err := a.pg.GetLLMProfile(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.profileView(p))
}

func (a *API) updateProfile(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "pid")
	if err != nil {
		fail(w, err)
		return
	}
	p, err := a.profileInput(r, id, false)
	if err != nil {
		fail(w, err)
		return
	}
	if err := a.pg.UpdateLLMProfile(r.Context(), *p); err != nil {
		fail(w, err)
		return
	}
	a.llm.InvalidateProviders()
	a.getProfile(w, r)
}

func (a *API) deleteProfile(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "pid")
	if err != nil {
		fail(w, err)
		return
	}
	p, err := a.pg.GetLLMProfile(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	if p.IsDefault {
		fail(w, badRequest("activate another profile before deleting the default"))
		return
	}
	if err := a.pg.DeleteLLMProfile(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	a.llm.InvalidateProviders()
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) bindAgent(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("agent")
	if key != agent.AgentPlanner && key != agent.AgentWorker && key != judgeAgentKey && key != agent.AgentGoals && key != agent.AgentWatchdog {
		fail(w, badRequest("agent must be planner, worker, judge, goals or watchdog"))
		return
	}
	var req struct {
		ProfileID int64 `json:"profile_id"` // 0 表示解绑
	}
	if err := decodeBody(r, &req); err != nil {
		fail(w, err)
		return
	}
	if req.ProfileID < 0 {
		fail(w, badRequest("invalid profile_id"))
		return
	}
	if req.ProfileID > 0 {
		p, err := a.pg.GetLLMProfile(r.Context(), req.ProfileID)
		if err != nil {
			fail(w, err)
			return
		}
		if !p.Enabled {
			fail(w, badRequest("cannot bind a disabled profile"))
			return
		}
	}
	if err := a.pg.SetAgentLLMBinding(r.Context(), key, req.ProfileID); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": key, "profile_id": req.ProfileID})
}

func (a *API) llmHealth(w http.ResponseWriter, r *http.Request) {
	hs, err := a.pg.ListLLMHealth(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	now := time.Now()
	out := make([]map[string]any, 0, len(hs))
	for _, h := range hs {
		out = append(out, map[string]any{"profile_id": h.ProfileID, "fails": h.Fails, "trips": h.Trips,
			"open": h.OpenUntil.After(now), "open_until": h.OpenUntil, "last_error": h.LastError, "last_at": h.LastAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) writeChainState(w http.ResponseWriter, r *http.Request, t *Task, reopened int64) {
	chain, err := a.pg.TaskLLMChain(r.Context(), t.ID)
	if err != nil {
		fail(w, err)
		return
	}
	st, err := a.pg.TaskLLMState(r.Context(), t.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chain":             chain,
		"active_profile_id": st.ActiveProfileID,
		"revision":          st.Revision,
		"failover_state":    st.FailoverState,
		"failover_reason":   st.FailoverReason,
		"reopened_intents":  reopened,
	})
}

func (a *API) getChain(w http.ResponseWriter, r *http.Request, t *Task) {
	a.writeChainState(w, r, t, 0)
}

func (a *API) setChain(w http.ResponseWriter, r *http.Request, t *Task) {
	var req struct {
		ProfileIDs      []int64 `json:"profile_ids"`
		ActiveProfileID int64   `json:"active_profile_id"`
	}
	if err := decodeBody(r, &req); err != nil {
		fail(w, err)
		return
	}
	if err := a.pg.SetTaskLLMChainWithActive(r.Context(), t.ID, req.ProfileIDs, req.ActiveProfileID); err != nil {
		fail(w, err)
		return
	}
	reopened, err := t.Store.ReopenIntentsByBlockedReason(r.Context(), db.BlockedReasonQuotaExhausted)
	if err != nil {
		fail(w, err)
		return
	}
	if reopened > 0 {
		t.Notify()
	}
	a.writeChainState(w, r, t, reopened)
}

func (a *API) resetChain(w http.ResponseWriter, r *http.Request, t *Task) {
	if err := a.pg.ResetTaskLLMChain(r.Context(), t.ID); err != nil {
		fail(w, err)
		return
	}
	// 配额恢复后重开因配额链耗尽而阻塞的意图。
	reopened, err := t.Store.ReopenIntentsByBlockedReason(r.Context(), db.BlockedReasonQuotaExhausted)
	if err != nil {
		fail(w, err)
		return
	}
	t.Notify()
	a.writeChainState(w, r, t, reopened)
}

func (a *API) llmUsage(w http.ResponseWriter, r *http.Request, t *Task) {
	totals, err := a.pg.TaskUsageTotals(r.Context(), t.ID)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(totals))
	for _, u := range totals {
		out = append(out, map[string]any{"model": u.Model, "calls": u.Calls, "input_tokens": u.InputTokens,
			"output_tokens": u.OutputTokens, "cache_read": u.CacheRead, "cache_write": u.CacheWrite})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// 拦截审批
// ---------------------------------------------------------------------------

func (a *API) listIntercepts(w http.ResponseWriter, r *http.Request) {
	var taskID int64
	if v := r.URL.Query().Get("task_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			fail(w, badRequest("invalid task_id"))
			return
		}
		taskID = id
	}
	ids, err := a.pg.ListOpenInterceptPending(r.Context(), taskID)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]*db.InterceptPendingRow, 0, len(ids))
	for _, id := range ids {
		row, err := a.pg.GetInterceptPending(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			continue
		}
		if err != nil {
			fail(w, err)
			return
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) decideIntercept(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "pendingID")
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Allow  *bool  `json:"allow"`
		Reason string `json:"reason"`
	}
	if err := decodeBody(r, &req); err != nil {
		fail(w, err)
		return
	}
	if req.Allow == nil {
		fail(w, badRequest("allow is required"))
		return
	}
	if a.ic == nil {
		fail(w, &httpError{http.StatusServiceUnavailable, errors.New("interceptor is not configured")})
		return
	}
	if err := a.ic.Decide(r.Context(), id, *req.Allow, req.Reason); err != nil {
		fail(w, err)
		return
	}
	row, err := a.pg.GetInterceptPending(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (a *API) interceptAudit(w http.ResponseWriter, r *http.Request, t *Task) {
	rows, err := a.pg.InterceptAuditForTask(r.Context(), t.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// ---------------------------------------------------------------------------
// SSE 活动流
// ---------------------------------------------------------------------------

func (a *API) events(w http.ResponseWriter, r *http.Request, t *Task) {
	rc := http.NewResponseController(w)
	sub, cancel := a.hub.Subscribe(t.ID, 512)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, ": subscribed to task %d\n\n", t.ID)
	if err := rc.Flush(); err != nil {
		return
	}

	heartbeat := time.NewTicker(a.cfg.SSEHeartbeat)
	defer heartbeat.Stop()
	var reportedDrops int64
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if d := sub.Dropped(); d > reportedDrops {
				fmt.Fprintf(w, "event: dropped\ndata: {\"dropped\":%d}\n\n", d)
				reportedDrops = d
			} else {
				fmt.Fprint(w, ": keep-alive\n\n")
			}
		case ev := <-sub.C:
			raw, err := json.Marshal(ev.Activity)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "id: %d\nevent: activity\ndata: %s\n\n", ev.Seq, raw)
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// 招标文档、企业资料库与推演报告
// ---------------------------------------------------------------------------

// maxDocumentBody 是招标文档 / 知识库 JSON 文本上传的请求体上限（32 MiB）；multipart 文件上传见 maxUploadBytes。
const maxDocumentBody = 32 << 20

type uploadDocumentReq struct {
	TenderNo    string `json:"tender_no"`
	LotNo       string `json:"lot_no"`
	Title       string `json:"title"`
	Text        string `json:"text"`
	WakePlanner bool   `json:"wake_planner"`
	// DocRole 为招标包内的文件角色（tender / correction / clarification / qa / attachment，空时按标题推断），
	// PublishedAt 为公告发布时间（RFC 3339 或 2006-01-02），决定效力先后。
	DocRole     string `json:"doc_role"`
	PublishedAt string `json:"published_at"`
}

// publishedAt 解析公告发布时间；空串返回 nil。
func (req uploadDocumentReq) publishedAt() (*time.Time, error) {
	raw := strings.TrimSpace(req.PublishedAt)
	if raw == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return &t, nil
	}
	t, err := time.ParseInLocation("2006-01-02", raw, time.FixedZone("CST", 8*3600))
	if err != nil {
		return nil, badRequest("published_at must be RFC 3339 or 2006-01-02")
	}
	return &t, nil
}

// listDocuments 列出任务的招标文档及其章节切分；?content=1 时附带段落正文。
func (a *API) listDocuments(w http.ResponseWriter, r *http.Request, t *Task) {
	docs, err := a.pg.ListTenderDocuments(r.Context(), t.ID)
	if err != nil {
		fail(w, err)
		return
	}
	withContent := r.URL.Query().Get("content") == "1"
	type docView struct {
		*db.TenderDocument
		Chunks []*db.DocumentChunk `json:"chunks"`
	}
	out := make([]docView, 0, len(docs))
	for _, d := range docs {
		var chunks []*db.DocumentChunk
		if withContent {
			chunks, err = a.pg.TaskChunks(r.Context(), t.ID, d.ID, 100000)
		} else {
			chunks, err = a.pg.DocumentOutline(r.Context(), t.ID, d.ID, 100000)
		}
		if err != nil {
			fail(w, err)
			return
		}
		if chunks == nil {
			chunks = []*db.DocumentChunk{}
		}
		out = append(out, docView{d, chunks})
	}
	writeJSON(w, http.StatusOK, out)
}

type credentialReq struct {
	Category      string          `json:"category"`
	Code          string          `json:"code"`
	Title         string          `json:"title"`
	SubjectName   string          `json:"subject_name"`
	SubjectType   string          `json:"subject_type"`
	CertificateNo string          `json:"certificate_no"`
	Issuer        string          `json:"issuer"`
	IssuedOn      *string         `json:"issued_on"`
	Scope         string          `json:"scope"`
	Attributes    json.RawMessage `json:"attributes"`
	ValidUntil    *string         `json:"valid_until"`
}

// upsertCredentials 录入企业资料：请求体为单个对象或对象数组；code 非空的记录按 (category, code) 幂等更新。
func (a *API) upsertCredentials(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if err := decodeBodyLimit(r, &raw, 4<<20); err != nil {
		fail(w, err)
		return
	}
	var reqs []credentialReq
	trimmed := strings.TrimSpace(string(raw))
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var err error
	if strings.HasPrefix(trimmed, "[") {
		err = dec.Decode(&reqs)
	} else {
		var one credentialReq
		err = dec.Decode(&one)
		reqs = []credentialReq{one}
	}
	if err != nil {
		fail(w, badRequest("invalid JSON body: %v", err))
		return
	}
	if len(reqs) == 0 {
		fail(w, badRequest("at least one credential is required"))
		return
	}
	for i, c := range reqs {
		if err := validateCredential(c); err != nil {
			fail(w, badRequest("credential %d: %v", i, err))
			return
		}
	}
	type result struct {
		*db.CompanyCredential
		Created bool `json:"created"`
	}
	out := make([]result, 0, len(reqs))
	for _, c := range reqs {
		saved, created, err := a.pg.UpsertCompanyCredential(r.Context(), &db.CompanyCredential{TenantID: db.DefaultTenant,
			Category: c.Category, Code: c.Code, Title: c.Title, SubjectName: c.SubjectName, SubjectType: c.SubjectType,
			CertificateNo: c.CertificateNo, Issuer: c.Issuer, IssuedOn: c.IssuedOn, Scope: c.Scope,
			Attributes: c.Attributes, ValidUntil: c.ValidUntil, CreatedBy: operator(r)})
		if err != nil {
			fail(w, err)
			return
		}
		out = append(out, result{saved, created})
	}
	writeJSON(w, http.StatusCreated, out)
}

// validateCredential 校验单条企业资料请求的必填项、attributes 形状与有效期格式。
func validateCredential(c credentialReq) error {
	if strings.TrimSpace(c.Category) == "" || strings.TrimSpace(c.Title) == "" {
		return badRequest("category and title are required")
	}
	if attrs := strings.TrimSpace(string(c.Attributes)); attrs != "" && attrs != "null" && !strings.HasPrefix(attrs, "{") {
		return badRequest("attributes must be a JSON object")
	}
	if c.ValidUntil != nil && *c.ValidUntil != "" {
		if _, err := time.Parse(time.DateOnly, *c.ValidUntil); err != nil {
			return badRequest("valid_until must be YYYY-MM-DD")
		}
	}
	if c.IssuedOn != nil && *c.IssuedOn != "" {
		if _, err := time.Parse(time.DateOnly, *c.IssuedOn); err != nil {
			return badRequest("issued_on must be YYYY-MM-DD")
		}
	}
	return nil
}

// updateCredential 按 id 全量更新企业资料：attributes 整体替换，valid_until 省略或为空表示长期有效。
func (a *API) updateCredential(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "credID")
	if err != nil {
		fail(w, err)
		return
	}
	var req credentialReq
	if err := decodeBody(r, &req); err != nil {
		fail(w, err)
		return
	}
	if err := validateCredential(req); err != nil {
		fail(w, err)
		return
	}
	saved, err := a.pg.UpdateCompanyCredential(r.Context(), &db.CompanyCredential{ID: id, TenantID: db.DefaultTenant,
		Category: req.Category, Code: req.Code, Title: req.Title, SubjectName: req.SubjectName, SubjectType: req.SubjectType,
		CertificateNo: req.CertificateNo, Issuer: req.Issuer, IssuedOn: req.IssuedOn, Scope: req.Scope,
		Attributes: req.Attributes, ValidUntil: req.ValidUntil, UpdatedBy: operator(r)})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (a *API) deleteCredential(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "credID")
	if err != nil {
		fail(w, err)
		return
	}
	if err := a.pg.DeleteCompanyCredentialScoped(r.Context(), db.DefaultTenant, id, operator(r)); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listCredentials 查询企业资料：?category=&q=（空格分隔，任一命中）&valid_on=YYYY-MM-DD&task_id=&limit=
func (a *API) listCredentials(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	taskID, _ := strconv.ParseInt(q.Get("task_id"), 10, 64)
	validOn := q.Get("valid_on")
	if validOn != "" {
		if _, err := time.Parse(time.DateOnly, validOn); err != nil {
			fail(w, badRequest("valid_on must be YYYY-MM-DD"))
			return
		}
	}
	creds, err := a.pg.ListCompanyCredentials(r.Context(), db.CredentialFilter{Category: q.Get("category"),
		Terms: strings.Fields(q.Get("q")), ValidOn: validOn, TaskID: taskID, Tenant: db.DefaultTenant, Limit: limit})
	if err != nil {
		fail(w, err)
		return
	}
	if creds == nil {
		creds = []*db.CompanyCredential{}
	}
	writeJSON(w, http.StatusOK, creds)
}

// report 导出任务的结构化推演报告：默认 JSON，?format=markdown 返回 Markdown 审查报告。
func (a *API) report(w http.ResponseWriter, r *http.Request, t *Task) {
	format := r.URL.Query().Get("format")
	if format != "" && format != "json" && format != "markdown" {
		fail(w, badRequest("format must be json or markdown"))
		return
	}
	rep, err := a.pg.TaskReport(r.Context(), t.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if format == "markdown" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="task-%d-report.md"`, t.ID))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, rep.Markdown())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
