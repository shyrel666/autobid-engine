package server

// Phase 5 HTTP 集成测试（需要 AUTOBID_TEST_DATABASE_URL）：要求清单 CRUD / 提取 / 确认，大纲提交审批跨 API 实例重建保留，
// 派发门禁（未批准、依赖未就绪、重复派发）、异步 AI 写回在人工编辑与大纲变更后被归档为候选稿，版本批准、撤销与回滚。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/bidtools"
	"github.com/autobid/autobid-engine/db"
)

// gatedWriter 在收到 release 前阻塞，用于制造「Worker 在途」窗口。
type gatedWriter struct {
	started chan int64
	release chan string
}

func newGatedWriter() *gatedWriter {
	return &gatedWriter{started: make(chan int64, 4), release: make(chan string, 4)}
}

func (g *gatedWriter) WriteSection(ctx context.Context, _ *Task, sc *bidtools.SectionContext, instruction string) (*bidtools.SectionDraft, error) {
	g.started <- sc.SectionID
	select {
	case content := <-g.release:
		return &bidtools.SectionDraft{ContentMD: content + instruction, SummaryNote: "AI 摘要", DiffSummary: "AI"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func newProposalAPI(t *testing.T, writer SectionWriter) *apiClient {
	t.Helper()
	pg := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(pg)
	e := NewEngine(m, func(*Task) (*agent.Planner, *agent.Worker) { return nil, nil },
		Config{Workers: 1, Debounce: 20 * time.Millisecond, WorkerIdlePoll: 20 * time.Millisecond})
	api := testAPI(ctx, m, e, NewLLMService(pg, LLMServiceConfig{}), nil, NewHub(), APIConfig{SectionWriter: writer})
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		srv.Close()
		e.StopAll()
		cancel()
	})
	return &apiClient{t: t, url: srv.URL}
}

// raw 发送请求并返回状态码与原始响应体（错误响应也解码）。
func (c *apiClient) raw(method, path string, body any, out any) int {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.url+path, rd)
	req.Header.Set("X-Test-Actor", "http-reviewer")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			c.t.Fatalf("%s %s: decode %s: %v", method, path, data, err)
		}
	}
	return resp.StatusCode
}

type apiErr struct {
	Error    string              `json:"error"`
	Code     string              `json:"code"`
	Outcome  string              `json:"outcome"`
	Revision *db.SectionRevision `json:"revision"`
	Section  *db.ProposalSection `json:"section"`
}

func waitSection(t *testing.T, c *apiClient, base string, id int64, done func(*db.SectionDetail) bool) *db.SectionDetail {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var det db.SectionDetail
		if code := c.raw("GET", fmt.Sprintf("%s/sections/%d", base, id), nil, &det); code != 200 {
			t.Fatalf("get section: %d", code)
		}
		if done(&det) {
			return &det
		}
		if time.Now().After(deadline) {
			t.Fatalf("section %d did not reach expected state: %+v", id, det.Section)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func codesOf(det *db.OutlineDetail) map[string]int64 {
	out := map[string]int64{}
	for _, s := range det.Sections {
		out[s.Code] = s.ID
	}
	return out
}

func TestHTTPProposalWorkflow(t *testing.T) {
	writer := newGatedWriter()
	c := newProposalAPI(t, writer)
	var task struct {
		ID int64 `json:"id"`
	}
	// Phase 5 流程不建故事板，显式沿用旧写作门禁；新任务默认强制 D3 的行为由 db.TestD3GateOnByDefaultForNewTasks 覆盖。
	if code := c.raw("POST", "/api/tasks", map[string]any{"name": "phase5", "goal": "编写技术标", "legacy_writing": true}, &task); code != 201 {
		t.Fatalf("create task: %d", code)
	}
	base := fmt.Sprintf("/api/tasks/%d", task.ID)
	text := strings.Join([]string{
		"第一章 投标人须知", "1.1 资格要求", "★投标人须具备安防工程一级资质，否则投标无效。",
		"第三章 评分标准", "3.1 技术方案", "技术方案评分：总体架构合理、技术路线先进得10分。",
		"第四章 投标文件格式", "4.1 格式要求", "投标文件正本一份、副本四份，须逐页签字盖章。",
	}, "\n")
	if code := c.raw("POST", base+"/documents", map[string]any{"tender_no": fmt.Sprintf("ZB-P5-%d", time.Now().UnixNano()), "title": "招标文件", "text": text}, nil); code != 201 {
		t.Fatalf("upload: %d", code)
	}

	// 要求清单：提取、人工增删改。
	var extracted struct {
		Result       db.ExtractResult    `json:"result"`
		Requirements db.RequirementsView `json:"requirements"`
	}
	if code := c.raw("POST", base+"/requirements/extract", nil, &extracted); code != 200 || extracted.Result.Created < 2 {
		t.Fatalf("extract: %d %+v", code, extracted.Result)
	}
	mandatory := false
	for _, it := range extracted.Requirements.Items {
		mandatory = mandatory || (it.Category == db.ReqMandatoryClause && it.IsMandatory && it.SourceChunkID != nil)
	}
	if !mandatory {
		t.Fatalf("mandatory clause not extracted: %+v", extracted.Requirements.Items)
	}
	var manual db.RequirementItem
	if code := c.raw("POST", base+"/requirements", map[string]any{"code": "MAN-1", "category": "non_scoring_required", "title": "售后承诺"}, &manual); code != 201 || manual.CreatedBy != "http-reviewer" {
		t.Fatalf("create requirement: %d %+v", code, manual)
	}
	if code := c.raw("POST", base+"/requirements", map[string]any{"code": "MAN-1", "category": "non_scoring_required", "title": "x"}, nil); code != 409 {
		t.Fatalf("duplicate requirement: %d", code)
	}
	if code := c.raw("POST", base+"/requirements", map[string]any{"code": "X", "category": "bogus", "title": "x"}, nil); code != 400 {
		t.Fatalf("invalid category: %d", code)
	}
	if code := c.raw("PUT", fmt.Sprintf("%s/requirements/%d", base, manual.ID), map[string]any{"evidence_gap": "暂无售后承诺证据"}, &manual); code != 200 || manual.EvidenceGap == "" {
		t.Fatalf("update requirement: %d", code)
	}
	tmp := db.RequirementItem{}
	c.raw("POST", base+"/requirements", map[string]any{"code": "TMP", "category": "format_attachment", "title": "临时"}, &tmp)
	if code := c.raw("DELETE", fmt.Sprintf("%s/requirements/%d", base, tmp.ID), nil, nil); code != 204 {
		t.Fatalf("delete requirement: %d", code)
	}

	// 大纲草案：派发与写回均被拒绝；清单未确认时不能提交审批。
	var outline db.OutlineDetail
	if code := c.raw("POST", base+"/outlines", map[string]any{"note": "默认目录"}, &outline); code != 201 || outline.Outline.Status != db.OutlineDraft {
		t.Fatalf("create outline: %d %+v", code, outline.Outline)
	}
	ids := codesOf(&outline)
	var e apiErr
	if code := c.raw("POST", fmt.Sprintf("%s/sections/%d/dispatch", base, ids["1"]), map[string]any{}, &e); code != 422 || e.Code != "outline_not_approved" {
		t.Fatalf("dispatch on draft: %d %+v", code, e)
	}
	if code := c.raw("POST", fmt.Sprintf("%s/sections/%d/revisions", base, ids["1"]), map[string]any{"base_rev_no": 0, "based_on_outline_version": 1, "content_md": "x"}, &e); code != 422 {
		t.Fatalf("save on draft: %d", code)
	}
	if code := c.raw("POST", base+"/outlines/1/submit", nil, &e); code != 422 || e.Code != "requirements_not_confirmed" {
		t.Fatalf("submit unconfirmed: %d %+v", code, e)
	}
	if code := c.raw("POST", base+"/requirements/confirm", map[string]any{}, nil); code != 200 {
		t.Fatalf("confirm: %d", code)
	}
	if code := c.raw("POST", base+"/outlines/1/submit", nil, &outline); code != 200 || outline.Outline.Status != db.OutlinePending {
		t.Fatalf("submit: %d %+v", code, outline.Outline)
	}
	if code := c.raw("POST", fmt.Sprintf("%s/sections/%d/dispatch", base, ids["1"]), nil, &e); code != 422 {
		t.Fatalf("dispatch on pending: %d", code)
	}

	// 「重启」：全新的 Manager / Engine / API 实例读取同一数据库，待审批状态保留，批准后放行。
	c = newProposalAPI(t, writer)
	if code := c.raw("GET", base+"/outlines/1", nil, &outline); code != 200 || outline.Outline.Status != db.OutlinePending {
		t.Fatalf("pending outline after restart: %d %+v", code, outline.Outline)
	}
	if code := c.raw("POST", base+"/outlines/1/approve", nil, &outline); code != 200 || outline.Outline.Status != db.OutlineApproved || outline.Outline.ApprovedBy != "http-reviewer" {
		t.Fatalf("approve: %d %+v", code, outline.Outline)
	}
	dependent := int64(0)
	for _, s := range outline.Sections {
		if len(s.DependsOn) > 0 {
			dependent = s.ID
		}
	}
	if code := c.raw("POST", fmt.Sprintf("%s/sections/%d/dispatch", base, dependent), nil, &e); code != 422 || e.Code != "dependency_unmet" {
		t.Fatalf("dependent dispatch: %d %+v", code, e)
	}

	// 场景 4：AI 在途期间人工先保存，AI 写回只能成为冲突候选稿。
	var claim db.SectionClaim
	if code := c.raw("POST", fmt.Sprintf("%s/sections/%d/dispatch", base, ids["1"]), map[string]any{"instruction": "（初稿）"}, &claim); code != 202 ||
		claim.BaseRevNo != 0 || claim.BasedOnOutlineVersion != 1 {
		t.Fatalf("dispatch: %d %+v", code, claim)
	}
	<-writer.started
	if code := c.raw("POST", fmt.Sprintf("%s/sections/%d/dispatch", base, ids["1"]), nil, &e); code != 409 || e.Code != "section_busy" {
		t.Fatalf("double dispatch: %d %+v", code, e)
	}
	var saved db.SaveRevisionResult
	if code := c.raw("POST", fmt.Sprintf("%s/sections/%d/revisions", base, ids["1"]), map[string]any{"base_rev_no": 0, "based_on_outline_version": 1,
		"content_md": "人工撰写的总体方案", "summary_note": "人工摘要"}, &saved); code != 201 || saved.Revision.RevNo != 1 {
		t.Fatalf("human save: %d %+v", code, saved)
	}
	writer.release <- "AI 总体方案"
	det := waitSection(t, c, base, ids["1"], func(d *db.SectionDetail) bool { return len(d.Revisions) == 2 })
	if det.Section.CurrentRevNo != 1 || det.Current.ContentMD != "人工撰写的总体方案" || det.Revisions[0].Status != db.RevConflict ||
		det.Revisions[0].ContentMD != "AI 总体方案（初稿）" || det.Section.Writing {
		t.Fatalf("lagging AI overwrote human edit: %+v current=%+v revisions=%+v %+v", det.Section, det.Current, *det.Revisions[0], *det.Revisions[1])
	}
	if code := c.raw("POST", fmt.Sprintf("%s/sections/%d/revisions", base, ids["1"]), map[string]any{"base_rev_no": 0, "based_on_outline_version": 1,
		"content_md": "过期视图上的编辑"}, &e); code != 409 || e.Outcome != db.SaveConflict || e.Revision == nil || e.Section.CurrentRevNo != 1 {
		t.Fatalf("stale human save: %d %+v", code, e)
	}

	// 依赖章节在前置章节有效稿后放行。
	if code := c.raw("POST", fmt.Sprintf("%s/sections/%d/dispatch", base, dependent), map[string]any{"base_rev_no": 0}, &claim); code != 202 {
		t.Fatalf("dependent dispatch after prerequisite: %d", code)
	}
	<-writer.started
	writer.release <- "依赖章节"
	waitSection(t, c, base, dependent, func(d *db.SectionDetail) bool { return d.Section.CurrentRevNo == 1 && !d.Section.Writing })

	// 场景 3：AI 在途期间人工调整目录使大纲升版，旧写回归档为 stale_outline。
	if code := c.raw("POST", fmt.Sprintf("%s/sections/%d/dispatch", base, ids["1"]), map[string]any{"base_rev_no": 1, "instruction": "重写"}, &claim); code != 202 || claim.BaseRevNo != 1 {
		t.Fatalf("rewrite dispatch: %d %+v", code, claim)
	}
	<-writer.started
	secs := []db.SectionInput{}
	for _, s := range outline.Sections {
		secs = append(secs, db.SectionInput{Code: s.Code, Title: s.Title + "（调整）", RequirementIDs: s.RequirementIDs, DependsOn: s.DependsOnCodes})
	}
	var v2 db.OutlineDetail
	if code := c.raw("PUT", base+"/outlines/1", map[string]any{"sections": secs}, &v2); code != 200 || v2.Outline.Version != 2 || v2.Outline.Status != db.OutlineDraft {
		t.Fatalf("restructure approved outline: %d %+v", code, v2.Outline)
	}
	writer.release <- "基于 v1 的重写"
	det = waitSection(t, c, base, ids["1"], func(d *db.SectionDetail) bool { return len(d.Revisions) == 4 })
	if det.Revisions[0].Status != db.RevStaleOut || det.Section.CurrentRevNo != 1 || det.Section.Status != db.SectionStaleOutline || det.Writable {
		t.Fatalf("stale outline write-back promoted: %+v %+v", det.Section, det.Revisions[0])
	}
	var list db.OutlineList
	if code := c.raw("GET", base+"/outlines", nil, &list); code != 200 || len(list.Outlines) != 2 || list.Outlines[1].Status != db.OutlineSuperseded || list.ApprovedVersion != 0 {
		t.Fatalf("outline list: %d %+v", code, list)
	}

	// 场景 5：在 v2 上批准绑定版本、修改自动撤销、一键回滚。
	if code := c.raw("POST", base+"/outlines/2/submit", nil, nil); code != 200 {
		t.Fatalf("submit v2: %d", code)
	}
	if code := c.raw("POST", base+"/outlines/2/approve", nil, &v2); code != 200 {
		t.Fatalf("approve v2: %d", code)
	}
	sid := codesOf(&v2)["1"]
	revPath := fmt.Sprintf("%s/sections/%d/revisions", base, sid)
	for i, content := range []string{"v2 rev1", "v2 rev2"} {
		if code := c.raw("POST", revPath, map[string]any{"base_rev_no": i, "based_on_outline_version": 2, "content_md": content}, nil); code != 201 {
			t.Fatalf("save %s: %d", content, code)
		}
	}
	if code := c.raw("POST", revPath+"/1/approve", nil, &e); code != 409 || e.Code != "revision_conflict" {
		t.Fatalf("approve non-current: %d %+v", code, e)
	}
	var sec db.ProposalSection
	if code := c.raw("POST", revPath+"/2/approve", nil, &sec); code != 200 || sec.ApprovedRevNo == nil || *sec.ApprovedRevNo != 2 {
		t.Fatalf("approve rev2: %d %+v", code, sec)
	}
	if code := c.raw("POST", revPath, map[string]any{"base_rev_no": 2, "based_on_outline_version": 2, "content_md": "v2 rev3"}, &saved); code != 201 ||
		saved.Section.ApprovedRevNo != nil || saved.Section.Status != db.SectionDraft {
		t.Fatalf("edit after approval: %d %+v", code, saved.Section)
	}
	if code := c.raw("POST", revPath+"/1/rollback", map[string]any{"base_rev_no": 3}, &saved); code != 201 || saved.Revision.RevNo != 4 ||
		saved.Revision.ContentMD != "v2 rev1" || saved.Revision.Source != db.RevRollback {
		t.Fatalf("rollback: %d %+v", code, saved.Revision)
	}
	if code := c.raw("POST", revPath+"/2/rollback", map[string]any{"base_rev_no": 3}, &e); code != 409 {
		t.Fatalf("rollback with stale base: %d", code)
	}
	if code := c.raw("GET", fmt.Sprintf("%s/sections/%d", base, 1<<40), nil, nil); code != 404 {
		t.Fatalf("unknown section: %d", code)
	}
}
