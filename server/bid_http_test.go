package server

// Phase 2 招投标领域能力测试：文档上传/切分、企业资料库、推演报告 HTTP 接口，
// 以及经 LLMService.Resolve 装配的文档工具 + Skill 渐进式披露端到端流程。
// 需要 AUTOBID_TEST_DATABASE_URL，未设置时跳过。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/bidtools"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/tool"
)

const bidTenderText = `政务云平台采购项目招标文件
第一章 投标人须知
1.1 投标人资格要求
1.1.1 投标人须具有独立法人资格。
★1.1.2 投标人须具备信息系统集成一级资质，否则投标无效。
- 1 -
第二章 评标办法
2.1 技术方案（30 分）`

func TestHTTPDocumentsCredentialsAndReport(t *testing.T) {
	c, e := newIdleAPI(t)
	tender := fmt.Sprintf("ZB-DOC-%d", time.Now().UnixNano())
	var created struct {
		ID int64 `json:"id"`
	}
	if code := c.do("POST", "/api/tasks", map[string]any{"name": "doc-task", "goal": "完成资格审查",
		"scopes": []map[string]string{{"tender_no": tender}}}, &created); code != 201 {
		t.Fatalf("create task: %d", code)
	}
	base := fmt.Sprintf("/api/tasks/%d", created.ID)

	if code := c.do("POST", base+"/documents", map[string]any{"text": "x"}, nil); code != 400 {
		t.Fatalf("missing tender_no: %d", code)
	}
	var up struct {
		Result struct {
			Document      db.TenderDocument `json:"document"`
			Created       bool              `json:"created"`
			Chunks        int               `json:"chunks"`
			Mandatory     int               `json:"mandatory_chunks"`
			TenderAssetID int64             `json:"tender_asset_id"`
			LotAssetID    int64             `json:"lot_asset_id"`
		} `json:"result"`
		PlannerNotified bool `json:"planner_notified"`
	}
	doc := map[string]any{"tender_no": tender, "lot_no": "A", "title": "招标文件", "text": bidTenderText, "wake_planner": true}
	if code := c.do("POST", base+"/documents", doc, &up); code != 201 || !up.Result.Created || !up.PlannerNotified ||
		up.Result.Chunks != 7 || up.Result.Mandatory != 1 || up.Result.TenderAssetID == 0 || up.Result.LotAssetID == 0 {
		t.Fatalf("upload: %d %+v", code, up)
	}
	triggers := e.m.Task(created.ID).drainTriggers()
	if len(triggers) != 1 || triggers[0].Kind != agent.TriggerDocumentAdded || !strings.Contains(triggers[0].Detail, "7 个") {
		t.Fatalf("planner trigger: %+v", triggers)
	}
	if code := c.do("POST", base+"/documents", doc, &up); code != 200 || up.Result.Created || up.PlannerNotified {
		t.Fatalf("re-upload must be idempotent and must not wake the planner: %d %+v", code, up)
	}

	var docs []struct {
		db.TenderDocument
		Chunks []db.DocumentChunk `json:"chunks"`
	}
	if code := c.do("GET", base+"/documents", nil, &docs); code != 200 || len(docs) != 1 || len(docs[0].Chunks) != 7 || docs[0].Chunks[4].Content != "" {
		t.Fatalf("list documents: %d %+v", code, docs)
	}
	if code := c.do("GET", base+"/documents?content=1", nil, &docs); code != 200 || !strings.Contains(docs[0].Chunks[4].Content, "否则投标无效") ||
		!docs[0].Chunks[4].IsMandatory || docs[0].Chunks[4].SectionCode != "1.1.2" || docs[0].Chunks[6].PageNo != 2 {
		t.Fatalf("list documents with content: %d %+v", code, docs[0].Chunks)
	}

	tag := fmt.Sprintf("cred-%d", time.Now().UnixNano())
	var saved []struct {
		db.CompanyCredential
		Created bool `json:"created"`
	}
	creds := []map[string]any{
		{"category": "qualification", "code": tag, "title": "信息系统集成一级 " + tag, "attributes": map[string]any{"level": "一级"}, "valid_until": "2030-01-01"},
		{"category": "personnel", "title": "项目经理张工 " + tag, "attributes": map[string]any{"title": "高级工程师"}},
	}
	if code := c.do("POST", "/api/company/credentials", creds, &saved); code != 201 || len(saved) != 2 || !saved[0].Created {
		t.Fatalf("create credentials: %d %+v", code, saved)
	}
	saved = nil
	if code := c.do("POST", "/api/company/credentials", map[string]any{"category": "qualification", "code": tag, "title": "换证 " + tag},
		&saved); code != 201 || len(saved) != 1 || saved[0].Created || saved[0].ValidUntil != nil {
		t.Fatalf("upsert single credential: %d %+v", code, saved)
	}
	if code := c.do("POST", "/api/company/credentials", map[string]any{"category": "x", "title": "y", "valid_until": "soon"}, nil); code != 400 {
		t.Fatalf("invalid valid_until: %d", code)
	}
	if code := c.do("POST", "/api/company/credentials", map[string]any{"category": "x", "title": "y", "attributes": []int{1}}, nil); code != 400 {
		t.Fatalf("non-object attributes: %d", code)
	}
	var list []db.CompanyCredential
	if code := c.do("GET", "/api/company/credentials?q="+tag, nil, &list); code != 200 || len(list) != 2 {
		t.Fatalf("query credentials: %d %+v", code, list)
	}
	if code := c.do("GET", "/api/company/credentials?category=personnel&q="+tag, nil, &list); code != 200 || len(list) != 1 {
		t.Fatalf("query credentials by category: %d %+v", code, list)
	}

	var rep db.TaskReport
	if code := c.do("GET", base+"/report", nil, &rep); code != 200 {
		t.Fatalf("report: %d", code)
	}
	if rep.Summary.Documents != 1 || rep.Summary.ElementsByType[db.AssetTender] != 1 || rep.Summary.ElementsByType[db.AssetLot] != 1 ||
		rep.Goal != "完成资格审查" || len(rep.Findings) != 0 || rep.Task.ID != created.ID {
		t.Fatalf("report: %+v", rep.Summary)
	}
	if code := c.do("GET", "/api/tasks/999999999/report", nil, nil); code != 404 {
		t.Fatalf("report of missing task: %d", code)
	}
}

// bidWorker 按工具结果推进的脚本化 Worker：检索 → 精读 → 加载技能 → 取 Schema → 资料比对 → 记录事实 → 报告结论。
type bidWorker struct {
	t   *testing.T
	tag string

	mu        sync.Mutex
	firstTool []string
	sourceRef json.RawMessage
	chunkID   float64
	searchOut string
	factID    int64
}

func lastResultText(req llm.CompletionRequest) string {
	for _, b := range req.Messages[len(req.Messages)-1].Content {
		if b.Type == llm.BlockToolResult {
			return b.Text
		}
	}
	return ""
}

func (w *bidWorker) brain(req llm.CompletionRequest) llm.Message {
	w.mu.Lock()
	defer w.mu.Unlock()
	prev, isResult := lastToolResult(req)
	out := lastResultText(req)
	if strings.Contains(lastUserText(req), "【收尾】") {
		return say("收尾：无新增写回")
	}
	if !isResult {
		if w.firstTool == nil {
			w.firstTool = toolNames(req)
		}
		return use(bidtools.ToolSearchDocument, map[string]any{"query": "信息系统集成", "mandatory_only": true})
	}
	switch prev {
	case bidtools.ToolSearchDocument:
		var r struct {
			Matches []struct {
				ChunkID float64 `json:"chunk_id"`
			} `json:"matches"`
		}
		if json.Unmarshal([]byte(out), &r) != nil || len(r.Matches) == 0 {
			return say("未检索到资质条款：" + out)
		}
		w.chunkID = r.Matches[0].ChunkID
		return use(bidtools.ToolReadChunk, map[string]any{"chunk_id": w.chunkID})
	case bidtools.ToolReadChunk:
		var r struct {
			SourceRef json.RawMessage `json:"source_ref"`
		}
		_ = json.Unmarshal([]byte(out), &r)
		w.sourceRef = r.SourceRef
		return use(tool.SkillToolName, map[string]any{"name": bidtools.SkillQualificationAudit})
	case tool.SkillToolName:
		return use(tool.SearchExtraToolsName, map[string]any{"query": "select:" + bidtools.ToolQueryCredentials})
	case tool.SearchExtraToolsName:
		w.searchOut = out
		return use(bidtools.ToolQueryCredentials, map[string]any{"category": db.CredentialQualification,
			"keywords": []string{w.tag}, "attributes": map[string]any{"level": "一级"}, "requirement": "信息系统集成一级资质"})
	case bidtools.ToolQueryCredentials:
		var r struct {
			Verdict string `json:"verdict"`
		}
		_ = json.Unmarshal([]byte(out), &r)
		return use(agent.ToolRecordFact, map[string]any{"summary": "资质核验结论：" + r.Verdict, "evidence": "★1.1.2",
			"source_ref": w.sourceRef})
	case agent.ToolRecordFact:
		var r struct {
			FactID int64 `json:"fact_id"`
		}
		_ = json.Unmarshal([]byte(out), &r)
		w.factID = r.FactID
		return use(agent.ToolReportFinding, map[string]any{"category": db.FindingCompliance, "severity": db.SeverityInfo,
			"title": "★1.1.2 资质要求满足", "evidence": []int64{r.FactID}, "source_refs": []json.RawMessage{w.sourceRef}})
	}
	return say("核验完成")
}

func TestBidToolsSkillDisclosureEndToEnd(t *testing.T) {
	pg := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano())
	profileID, err := pg.CreateLLMProfile(ctx, db.LLMProfile{Name: "bid-" + suffix, Model: "claude-opus-5-5", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	tag := "cred-e2e-" + suffix
	if _, _, err := pg.UpsertCompanyCredential(ctx, &db.CompanyCredential{Category: db.CredentialQualification, Code: tag,
		Title: "信息系统集成及服务资质 " + tag, Attributes: json.RawMessage(`{"level":"一级"}`)}); err != nil {
		t.Fatal(err)
	}

	m := NewManager(pg)
	acts := &activityLog{}
	var task *Task
	worker := &bidWorker{t: t, tag: tag}
	svc := NewLLMService(pg, LLMServiceConfig{
		Factory: func(*db.LLMProfile, *http.Client) (llm.Provider, error) {
			return brain(func(req llm.CompletionRequest) llm.Message {
				if strings.Contains(req.System, "Planner") {
					return plannerBrain(t, &task, true)(req)
				}
				return worker.brain(req)
			}), nil
		},
		OnActivity: func(_ int64, a db.Activity) { acts.add(a) },
	})
	e := NewEngine(m, svc.Resolve, Config{Workers: 1, Debounce: 30 * time.Millisecond, WorkerIdlePoll: 30 * time.Millisecond, ModelErrorRetries: -1, OnActivity: acts.add})
	svc.Bind(e)
	api := NewAPI(ctx, m, e, svc, nil, NewHub(), APIConfig{Token: testToken})
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()
	c := &apiClient{t: t, url: srv.URL}

	task, err = m.CreateTask(ctx, db.CreateTaskInput{Name: "bid-e2e-" + suffix, Goal: "完成资格审查推演"}, "资格审查无废标风险")
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.SetTaskLLMChain(ctx, task.ID, []int64{profileID}); err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("/api/tasks/%d", task.ID)
	if code := c.do("POST", base+"/documents", map[string]any{"tender_no": "ZB-E2E-" + suffix, "text": bidTenderText}, nil); code != 201 {
		t.Fatalf("upload: %d", code)
	}
	if err := e.Run(ctx, task); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.StopTask(task.ID) })
	t.Cleanup(func() {
		if t.Failed() {
			acts.mu.Lock()
			for _, a := range acts.list {
				t.Logf("activity %s %s: %s | %s", a.Worker, a.Kind, a.Summary, a.Detail)
			}
			acts.mu.Unlock()
		}
	})
	waitFor(t, 20*time.Second, "task done", func() bool {
		r, _ := pg.GetTask(context.Background(), task.ID)
		return r.Status == db.TaskDone
	})

	worker.mu.Lock()
	first, searchOut := worker.firstTool, worker.searchOut
	worker.mu.Unlock()
	for _, want := range []string{bidtools.ToolDocumentOutline, bidtools.ToolSearchDocument, bidtools.ToolReadChunk, tool.SkillToolName, tool.SearchExtraToolsName} {
		if !slices.Contains(first, want) {
			t.Fatalf("worker tools missing %s: %v", want, first)
		}
	}
	for _, hidden := range []string{bidtools.ToolQueryCredentials, bidtools.ToolSimulateScoring, bidtools.ToolVerifyCommercial} {
		if slices.Contains(first, hidden) {
			t.Fatalf("deferred tool %s schema must not be sent before unlock", hidden)
		}
	}
	if !strings.Contains(searchOut, "input_schema") || strings.Contains(searchOut, `"locked"`) {
		t.Fatalf("SearchExtraTools after Skill: %s", searchOut)
	}

	var rep db.TaskReport
	if code := c.do("GET", base+"/report", nil, &rep); code != 200 {
		t.Fatalf("report: %d", code)
	}
	if rep.Summary.Findings != 1 || rep.Summary.GoalsMet != rep.Summary.GoalsTotal || rep.Summary.Facts != 1 {
		t.Fatalf("report summary: %+v", rep.Summary)
	}
	f := rep.Findings[0]
	if len(f.SourceRefs) != 1 || len(f.Evidence) != 1 {
		t.Fatalf("finding: %+v", f)
	}
	var findingRef, factRef bidtools.SourceRef
	_ = json.Unmarshal(f.SourceRefs[0], &findingRef)
	ev := f.Evidence[0]
	_ = json.Unmarshal(ev.SourceRef, &factRef)
	if findingRef.ChunkID != int64(worker.chunkID) || findingRef.SectionCode != "1.1.2" {
		t.Fatalf("finding source_ref: %s", f.SourceRefs[0])
	}
	if ev.NodeID != worker.factID || ev.Summary != "资质核验结论："+bidtools.VerdictSatisfied || factRef != findingRef {
		t.Fatalf("evidence: %+v", ev)
	}
}
