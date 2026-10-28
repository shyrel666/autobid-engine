package server

// HTTP/SSE 控制面测试：真实 PostgreSQL + httptest。需要 AUTOBID_TEST_DATABASE_URL，未设置时跳过。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/intercept"
)

const testToken = "test-token"

type apiClient struct {
	t   *testing.T
	url string
}

func (c *apiClient) do(method, path string, body any, out any) int {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, c.url+path, rd)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			c.t.Fatalf("%s %s: decode %s: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode
}

// newIdleAPI 建立一个 LLM 未就绪（Resolver 返回 nil）的引擎与 API：任务可启动但不会调用模型，
// 用于测试生命周期与配置接口。
func newIdleAPI(t *testing.T) (*apiClient, *Engine) {
	t.Helper()
	pg := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(pg)
	e := NewEngine(m, func(*Task) (*agent.Planner, *agent.Worker) { return nil, nil },
		Config{Workers: 1, Debounce: 20 * time.Millisecond, WorkerIdlePoll: 20 * time.Millisecond})
	api := NewAPI(ctx, m, e, NewLLMService(pg, LLMServiceConfig{}), nil, NewHub(), APIConfig{Token: testToken})
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		srv.Close()
		e.StopAll()
		cancel()
	})
	return &apiClient{t: t, url: srv.URL}, e
}

func TestHTTPAuthAndValidation(t *testing.T) {
	c, _ := newIdleAPI(t)
	resp, err := http.Get(c.url + "/api/tasks")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request: %d", resp.StatusCode)
	}
	if resp, err := http.Get(c.url + "/healthz"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz must not require auth: %v %v", err, resp)
	}
	if code := c.do("POST", "/api/tasks", map[string]any{"name": "x"}, nil); code != 400 {
		t.Fatalf("missing goal: %d", code)
	}
	if code := c.do("POST", "/api/tasks", map[string]any{"name": "x", "goal": "g", "bogus": 1}, nil); code != 400 {
		t.Fatalf("unknown field: %d", code)
	}
	if code := c.do("GET", "/api/tasks/999999999", nil, nil); code != 404 {
		t.Fatalf("unknown task: %d", code)
	}
	if code := c.do("POST", "/api/intercepts/999999999/decide", map[string]any{"allow": true}, nil); code != 503 {
		t.Fatalf("decide without interceptor: %d", code)
	}
}

func TestHTTPTaskLifecycleAndLLMConfig(t *testing.T) {
	c, e := newIdleAPI(t)
	tender := fmt.Sprintf("ZB-HTTP-%d", time.Now().UnixNano())

	var created struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	if code := c.do("POST", "/api/tasks", map[string]any{
		"name": "http-task", "goal": "完成 A 标段资格审查", "goals": []string{"资格审查无废标风险"},
		"scopes": []map[string]string{{"tender_no": tender, "lot_no": "A"}}, "start": false,
	}, &created); code != 201 || created.Status != db.TaskCreated {
		t.Fatalf("create: %d %+v", code, created)
	}
	base := fmt.Sprintf("/api/tasks/%d", created.ID)

	var view taskView
	if code := c.do("POST", base+"/start", nil, &view); code != 200 || view.Status != db.TaskRunning || !view.Running {
		t.Fatalf("start: %d %+v", code, view)
	}
	if code := c.do("POST", base+"/pause", nil, &view); code != 200 || view.Status != db.TaskPaused || !e.IsPaused(created.ID) {
		t.Fatalf("pause: %d %+v", code, view)
	}
	if code := c.do("POST", base+"/resume", nil, &view); code != 200 || view.Status != db.TaskRunning {
		t.Fatalf("resume: %d %+v", code, view)
	}
	if code := c.do("POST", base+"/revive", nil, nil); code != 409 {
		t.Fatalf("revive non-terminal: %d", code)
	}

	var goal map[string]any
	if code := c.do("POST", base+"/goals", map[string]string{"summary": "报价不超过最高限价"}, &goal); code != 201 {
		t.Fatalf("add goal: %d", code)
	}
	if code := c.do("POST", base+"/hints", map[string]any{"text": "我方 ISO27001 证书 2027 年到期"}, nil); code != 201 {
		t.Fatalf("add hint: %d", code)
	}
	var ov db.Overview
	if code := c.do("GET", base+"/overview", nil, &ov); code != 200 || len(ov.Goals) != 2 || len(ov.Hints) != 1 || ov.Coverage == nil {
		t.Fatalf("overview: %d %+v", code, ov)
	}
	var cov map[string]any
	if code := c.do("GET", base+"/coverage", nil, &cov); code != 200 || cov["target"] == nil {
		t.Fatalf("coverage: %d %v", code, cov)
	}
	var origin struct {
		Node db.Node `json:"node"`
	}
	if code := c.do("GET", fmt.Sprintf("%s/nodes/%d", base, ov.Goals[0].ID), nil, &origin); code != 200 || origin.Node.Kind != db.KindGoal {
		t.Fatalf("node detail: %d %+v", code, origin)
	}
	if code := c.do("POST", fmt.Sprintf("%s/works/%d/control", base, ov.Goals[0].ID), map[string]string{"action": "pause"}, nil); code != 400 {
		t.Fatalf("control on a goal must be rejected: %d", code)
	}

	// LLM Profile CRUD 与任务配额链
	var prof db.LLMProfile
	name := fmt.Sprintf("http-prof-%d", time.Now().UnixNano())
	if code := c.do("POST", "/api/llm/profiles", map[string]any{"name": name, "format": "openai", "model": "deepseek-chat", "enabled": true}, &prof); code != 201 || prof.APIKeyEnv != "OPENAI_API_KEY" {
		t.Fatalf("create profile: %d %+v", code, prof)
	}
	prof.Model = "deepseek-reasoner"
	if code := c.do("PUT", fmt.Sprintf("/api/llm/profiles/%d", prof.ID), prof, &prof); code != 200 || prof.Model != "deepseek-reasoner" {
		t.Fatalf("update profile: %d %+v", code, prof)
	}
	if code := c.do("POST", "/api/llm/profiles", map[string]any{"name": "bad", "format": "gemini", "model": "m"}, nil); code != 400 {
		t.Fatalf("invalid format: %d", code)
	}
	var chain struct {
		Chain  []db.ChainEntry `json:"chain"`
		Active *int64          `json:"active_profile_id"`
	}
	if code := c.do("PUT", base+"/llm-chain", map[string]any{"profile_ids": []int64{prof.ID}}, &chain); code != 200 || len(chain.Chain) != 1 || *chain.Active != prof.ID {
		t.Fatalf("set chain: %d %+v", code, chain)
	}
	if code := c.do("POST", base+"/llm-chain/reset", nil, &chain); code != 200 {
		t.Fatalf("reset chain: %d", code)
	}
	if code := c.do("PUT", "/api/llm/bindings/judge", map[string]any{"profile_id": 0}, nil); code != 200 {
		t.Fatalf("unbind judge: %d", code)
	}
	if code := c.do("PUT", "/api/llm/bindings/hacker", map[string]any{"profile_id": 0}, nil); code != 400 {
		t.Fatalf("invalid agent binding: %d", code)
	}
	var usage, health []map[string]any
	if c.do("GET", base+"/llm-usage", nil, &usage) != 200 || c.do("GET", "/api/llm/health", nil, &health) != 200 {
		t.Fatal("usage/health endpoints failed")
	}

	if code := c.do("DELETE", base, nil, nil); code != 204 {
		t.Fatalf("delete task: %d", code)
	}
	if code := c.do("GET", base, nil, nil); code != 404 {
		t.Fatalf("deleted task still visible: %d", code)
	}
	if code := c.do("DELETE", fmt.Sprintf("/api/llm/profiles/%d", prof.ID), nil, nil); code != 204 {
		t.Fatalf("delete profile: %d", code)
	}
}

// sseReader 在后台读取 SSE 流并收集 activity 事件。
type sseReader struct {
	mu     sync.Mutex
	events []db.Activity
}

func (s *sseReader) has(substr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.events {
		if strings.Contains(a.Summary, substr) {
			return true
		}
	}
	return false
}

func openSSE(t *testing.T, url string) *sseReader {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("sse: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	r := &sseReader{}
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var a db.Activity
				if json.Unmarshal([]byte(data), &a) == nil {
					r.mu.Lock()
					r.events = append(r.events, a)
					r.mu.Unlock()
				}
			}
		}
	}()
	return r
}

// 审批经 HTTP 完成：挂起的工具调用出现在 /api/intercepts，POST decide 放行后任务完成；
// 活动经 SSE 实时推送，审计可查询。
func TestHTTPApprovalFlowAndSSE(t *testing.T) {
	hub := NewHub()
	env := newGuardedEnv(t, db.InterceptRule{Action: intercept.ActionAsk, Message: "提交投标需人工审批"},
		func(o *envOpts) { o.onActivity = hub.Publish })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	api := NewAPI(ctx, env.e.m, env.e, NewLLMService(testDB(t), LLMServiceConfig{}), env.ic, hub, APIConfig{Token: testToken})
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	c := &apiClient{t: t, url: srv.URL}
	base := fmt.Sprintf("/api/tasks/%d", env.task.ID)

	sse := openSSE(t, srv.URL+base+"/events?token="+testToken)

	var pending []db.InterceptPendingRow
	waitFor(t, 15*time.Second, "pending approval listed", func() bool {
		pending = nil
		c.do("GET", fmt.Sprintf("/api/intercepts?task_id=%d", env.task.ID), nil, &pending)
		return len(pending) == 1 && env.ic.Waiting(pending[0].ID)
	})
	if pending[0].ToolName != guardedTool || env.executed.Load() != 0 {
		t.Fatalf("pending: %+v executed=%d", pending[0], env.executed.Load())
	}
	decide := fmt.Sprintf("/api/intercepts/%d/decide", pending[0].ID)
	if code := c.do("POST", decide, map[string]any{}, nil); code != 400 {
		t.Fatalf("decide without allow: %d", code)
	}
	var row db.InterceptPendingRow
	if code := c.do("POST", decide, map[string]any{"allow": true, "reason": "经办人同意"}, &row); code != 200 || row.Status != db.InterceptAllowed {
		t.Fatalf("decide: %d %+v", code, row)
	}
	if code := c.do("POST", decide, map[string]any{"allow": false}, nil); code != 409 {
		t.Fatalf("second decide: %d", code)
	}

	waitFor(t, 15*time.Second, "task done", func() bool {
		var v taskView
		c.do("GET", base, nil, &v)
		return v.Task != nil && v.Status == db.TaskDone
	})
	waitFor(t, 5*time.Second, "sse activity", func() bool { return sse.has("请求审批") && sse.has("任务完成") })

	var audit []db.InterceptAuditRow
	if code := c.do("GET", base+"/intercept-audit", nil, &audit); code != 200 || len(audit) == 0 {
		t.Fatalf("audit: %d %+v", code, audit)
	}
	var findings []db.Finding
	if code := c.do("GET", base+"/findings", nil, &findings); code != 200 {
		t.Fatalf("findings: %d", code)
	}
	var ov db.Overview
	if code := c.do("GET", base+"/overview", nil, &ov); code != 200 || len(ov.RecentFacts) != 1 {
		t.Fatalf("overview after approval: %d %+v", code, ov.RecentFacts)
	}
}
