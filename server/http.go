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
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/intercept"
)

type APIConfig struct {
	Token string
	// SSEHeartbeat 是 SSE 保活注释间隔，默认 15s。
	SSEHeartbeat time.Duration
}

type API struct {
	runCtx context.Context // 任务运行时的父 Context（服务生命周期）
	m      *Manager
	e      *Engine
	llm    *LLMService
	ic     *intercept.Interceptor
	hub    *Hub
	pg     *db.DB
	cfg    APIConfig
}

func NewAPI(runCtx context.Context, m *Manager, e *Engine, svc *LLMService, ic *intercept.Interceptor, hub *Hub, cfg APIConfig) *API {
	if cfg.SSEHeartbeat <= 0 {
		cfg.SSEHeartbeat = 15 * time.Second
	}
	return &API{runCtx: runCtx, m: m, e: e, llm: svc, ic: ic, hub: hub, pg: m.DB(), cfg: cfg}
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
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
	mux.HandleFunc("GET /api/tasks/{id}/nodes/{nodeID}", a.withTask(a.nodeDetail))
	mux.HandleFunc("GET /api/tasks/{id}/findings", a.withTask(a.findings))
	mux.HandleFunc("GET /api/tasks/{id}/assets", a.withTask(a.assets))
	mux.HandleFunc("GET /api/assets/{assetID}/edges", a.assetEdges)

	// LLM 配置与配额链
	mux.HandleFunc("GET /api/llm/profiles", a.listProfiles)
	mux.HandleFunc("POST /api/llm/profiles", a.createProfile)
	mux.HandleFunc("GET /api/llm/profiles/{pid}", a.getProfile)
	mux.HandleFunc("PUT /api/llm/profiles/{pid}", a.updateProfile)
	mux.HandleFunc("DELETE /api/llm/profiles/{pid}", a.deleteProfile)
	mux.HandleFunc("PUT /api/llm/bindings/{agent}", a.bindAgent)
	mux.HandleFunc("GET /api/llm/health", a.llmHealth)
	mux.HandleFunc("GET /api/tasks/{id}/llm-chain", a.withTask(a.getChain))
	mux.HandleFunc("PUT /api/tasks/{id}/llm-chain", a.withTask(a.setChain))
	mux.HandleFunc("POST /api/tasks/{id}/llm-chain/reset", a.withTask(a.resetChain))
	mux.HandleFunc("GET /api/tasks/{id}/llm-usage", a.withTask(a.llmUsage))

	// 拦截审批
	mux.HandleFunc("GET /api/intercepts", a.listIntercepts)
	mux.HandleFunc("POST /api/intercepts/{pendingID}/decide", a.decideIntercept)
	mux.HandleFunc("GET /api/tasks/{id}/intercept-audit", a.withTask(a.interceptAudit))

	// 实时活动流
	mux.HandleFunc("GET /api/tasks/{id}/events", a.withTask(a.events))

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
	switch {
	case errors.As(err, &he):
		writeError(w, he.status, he.err)
	case errors.Is(err, db.ErrNotFound), errors.Is(err, intercept.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, errWorkControlConflict), errors.Is(err, intercept.ErrAlreadyDecided):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, intercept.ErrWaiterGone):
		writeError(w, http.StatusGone, err)
	case errors.Is(err, db.ErrInvalidDigest), errors.Is(err, db.ErrInvalidEdge), errors.Is(err, db.ErrForeignNode):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
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
	a.respondTask(w, r, t, a.e.Pause(r.Context(), t, agent.AbortPausedByUser))
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
	writeJSON(w, http.StatusOK, ps)
}

func validateProfile(p *db.LLMProfile) error {
	if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Model) == "" {
		return badRequest("name and model are required")
	}
	if p.Format != "" && p.Format != "anthropic" && p.Format != "openai" {
		return badRequest("format must be anthropic or openai")
	}
	return nil
}

func (a *API) createProfile(w http.ResponseWriter, r *http.Request) {
	var p db.LLMProfile
	if err := decodeBody(r, &p); err != nil {
		fail(w, err)
		return
	}
	if err := validateProfile(&p); err != nil {
		fail(w, err)
		return
	}
	id, err := a.pg.CreateLLMProfile(r.Context(), p)
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
	writeJSON(w, http.StatusCreated, created)
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
	writeJSON(w, http.StatusOK, p)
}

func (a *API) updateProfile(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "pid")
	if err != nil {
		fail(w, err)
		return
	}
	var p db.LLMProfile
	if err := decodeBody(r, &p); err != nil {
		fail(w, err)
		return
	}
	p.ID = id
	if p.Format == "" {
		p.Format = "anthropic"
	}
	if err := validateProfile(&p); err != nil {
		fail(w, err)
		return
	}
	if err := a.pg.UpdateLLMProfile(r.Context(), p); err != nil {
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
	if err := a.pg.DeleteLLMProfile(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	a.llm.InvalidateProviders()
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) bindAgent(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("agent")
	if key != agent.AgentPlanner && key != agent.AgentWorker && key != judgeAgentKey {
		fail(w, badRequest("agent must be planner, worker or judge"))
		return
	}
	var req struct {
		ProfileID int64 `json:"profile_id"` // 0 表示解绑
	}
	if err := decodeBody(r, &req); err != nil {
		fail(w, err)
		return
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

func (a *API) getChain(w http.ResponseWriter, r *http.Request, t *Task) {
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
	writeJSON(w, http.StatusOK, map[string]any{"chain": chain, "active_profile_id": st.ActiveProfileID, "revision": st.Revision})
}

func (a *API) setChain(w http.ResponseWriter, r *http.Request, t *Task) {
	var req struct {
		ProfileIDs []int64 `json:"profile_ids"`
	}
	if err := decodeBody(r, &req); err != nil {
		fail(w, err)
		return
	}
	if err := a.pg.SetTaskLLMChain(r.Context(), t.ID, req.ProfileIDs); err != nil {
		fail(w, err)
		return
	}
	a.getChain(w, r, t)
}

func (a *API) resetChain(w http.ResponseWriter, r *http.Request, t *Task) {
	if err := a.pg.ResetTaskLLMChain(r.Context(), t.ID); err != nil {
		fail(w, err)
		return
	}
	// 配额恢复后重开因配额链耗尽而阻塞的意图。
	if _, err := t.Store.ReopenIntentsByBlockedReason(r.Context(), db.BlockedReasonQuotaExhausted); err != nil {
		fail(w, err)
		return
	}
	t.Notify()
	a.getChain(w, r, t)
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
