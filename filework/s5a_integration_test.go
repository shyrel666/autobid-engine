package filework

// S5a 集成测试（真实 PostgreSQL，需要 AUTOBID_TEST_DATABASE_URL）：候选模式导入、人工采用 / 拒绝、来源 epoch 与导出门禁、
// 解析路由与执行版本锁定、OCR 暂时故障重试与预算耗尽、Agent 提交（同事务订阅、幂等、元数据、宿主范围）与意图完成判据。
// 真实 PaddleOCR 端到端用例在设置 AUTOBID_OCR_URL 时执行。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
	"github.com/autobid/autobid-engine/ingest"
	"github.com/autobid/autobid-engine/ingest/ingesttest"
)

const s5aMarkdown = "# 第一章 总则\n\n1.1 本项目为数据中心改造工程。\n\n## 第二章 技术要求\n\n2.1 ★服务器数量不少于12台。\n\n2.2 质保期3年。\n"

func putFile(t *testing.T, store *Store, taskID int64, name, format string, data []byte) *db.TaskFileObject {
	t.Helper()
	obj, err := store.PutBytes(context.Background(), PutOptions{TenantID: db.DefaultTenant, TaskID: taskID, Kind: db.FileKindOriginal, OriginalName: name, Format: format}, data)
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

func runOne(t *testing.T, r *Runner) {
	t.Helper()
	ok, err := r.RunOnce(context.Background())
	if err != nil || !ok {
		t.Fatalf("run once: ok=%v err=%v", ok, err)
	}
}

// runAny runs one claimed job whose processing may fail by design (the error is recorded on the job).
func runAny(t *testing.T, r *Runner) {
	t.Helper()
	if ok, _ := r.RunOnce(context.Background()); !ok {
		t.Fatal("no job was claimed")
	}
}

func jobByID(t *testing.T, d *db.DB, taskID, id int64) *db.FixedFileJob {
	t.Helper()
	j, err := d.GetFixedFileJob(context.Background(), db.DefaultTenant, taskID, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func sourceEpoch(t *testing.T, d *db.DB, taskID int64) int64 {
	t.Helper()
	var n int64
	if err := d.QueryRow(`SELECT COALESCE(max(source_epoch),0) FROM requirement_manifests WHERE task_id=$1`, taskID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func blockerKinds(t *testing.T, d *db.DB, taskID int64) map[string]bool {
	t.Helper()
	r, err := d.ExportReadiness(context.Background(), db.DefaultTenant, taskID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, b := range r.Blockers {
		out[b.Kind] = true
	}
	return out
}

func docCount(t *testing.T, d *db.DB, taskID int64) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM tender_documents WHERE task_id=$1`, taskID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// versionedOCR 模拟本机 OCR 服务：报告执行版本，可切换版本或暂时不可达。
type versionedOCR struct {
	mu      sync.Mutex
	version string
	down    bool
	calls   int
}

func (o *versionedOCR) set(version string, down bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.version, o.down = version, down
}
func (o *versionedOCR) Name() string { return "versioned" }
func (o *versionedOCR) Version(context.Context) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.down {
		return "", fmt.Errorf("%w: connection refused", ingest.ErrOCRTransient)
	}
	return o.version, nil
}
func (o *versionedOCR) Recognize(context.Context, []byte, string) (*ingest.OCRPage, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	if o.down {
		return nil, fmt.Errorf("%w: connection refused", ingest.ErrOCRTransient)
	}
	return &ingest.OCRPage{Width: 800, Height: 1100, Version: o.version, Lines: []ingest.OCRLine{
		{Text: "第三章 技术要求", BBox: [4]float64{80, 60, 400, 100}, Confidence: 0.99},
		{Text: "3.1 服务器数量不少于12台。", BBox: [4]float64{80, 140, 700, 180}, Confidence: 0.95},
	}}, nil
}

var fakePNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR s5a")

func TestCandidateImportAdoptRejectAndSourceEpoch(t *testing.T) {
	d := dbtest.Open(t)
	ctx := context.Background()
	p := dbtest.Ready(t, d, dbtest.Spec{Name: dbtest.Uniq("s5a-candidate"), Reqs: dbtest.DefaultSpec(0).Reqs, Sections: dbtest.DefaultSpec(0).Sections})
	store, err := NewStore(d, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proc := &Processor{DB: d, Store: store}
	runner := &Runner{Processor: proc, Owner: "s5a", TaskID: p.TaskID, Lease: time.Minute}
	md := putFile(t, store, p.TaskID, "补充技术要求.md", ingest.FormatMarkdown, []byte(s5aMarkdown))
	if _, err := d.SaveTaskMaterial(ctx, db.DefaultTenant, p.TaskID, db.TaskMaterial{Name: "补充技术要求", Required: true, FileID: &md.ID}, "planner"); err != nil {
		t.Fatal(err)
	}
	epoch0 := sourceEpoch(t, d, p.TaskID)

	j, err := proc.Enqueue(ctx, p.TaskID, ProcessInput{FileID: md.ID, TenderNo: "T-S5A", RequestKey: "cand-1", CandidateOnly: true, ParserProfile: "auto"}, "tester")
	if err != nil || j.Executor != FixedParserVersion {
		t.Fatalf("enqueue: %v %+v", err, j)
	}
	runOne(t, runner)
	j = jobByID(t, d, p.TaskID, j.ID)
	if j.Status != "succeeded" || j.RecordID == nil || j.RecordStatus != db.RecordReady {
		t.Fatalf("job: %+v", j)
	}
	rec, err := d.GetFileProcessingRecord(ctx, db.DefaultTenant, p.TaskID, *j.RecordID, false)
	if err != nil || !rec.CandidateOnly || rec.ParserProfile != "auto" || rec.ParserVersion != FixedParserVersion || rec.Operation != "import" {
		t.Fatalf("record: %v %+v", err, rec)
	}
	// 候选不是正式文档：不写招标文档、不推进来源 epoch，但必需材料处于“待采用”并阻断导出。
	if n := docCount(t, d, p.TaskID); n != 0 {
		t.Fatalf("candidate wrote %d documents", n)
	}
	if sourceEpoch(t, d, p.TaskID) != epoch0 {
		t.Fatal("candidate advanced source_epoch")
	}
	mats, _ := d.ListTaskMaterials(ctx, db.DefaultTenant, p.TaskID)
	if len(mats) != 1 || mats[0].State != "review" {
		t.Fatalf("material state: %+v", mats[0])
	}
	if b := blockerKinds(t, d, p.TaskID); !b["required_material_incomplete"] || b["requirements_source_changed"] {
		t.Fatalf("blockers before adoption: %v", b)
	}
	page, err := Candidate(rec, 0, 2, 4000, 0)
	if err != nil || page.TotalSections < 2 || len(page.Sections) != 2 || page.NextCursor != 2 || page.Sections[0].Locator.SHA256 != md.SHA256 || page.Document.TenderNo != "T-S5A" {
		t.Fatalf("candidate page: %v %+v", err, page)
	}

	// 人工采用：expected 必须为 0；同事务 PersistTender，推进来源 epoch，要求清单须重新提取（导出阻断）。
	if _, err := proc.Adopt(ctx, p.TaskID, rec.ID, 1, "reviewer", ""); !errors.Is(err, db.ErrFileProcessingConflict) {
		t.Fatalf("adopt import with revision: %v", err)
	}
	adopted, err := proc.Adopt(ctx, p.TaskID, rec.ID, 0, "reviewer", "核对原件无误")
	if err != nil || adopted.Status != db.RecordAdopted || adopted.ApprovedBy != "reviewer" {
		t.Fatalf("adopt: %v %+v", err, adopted)
	}
	if n := docCount(t, d, p.TaskID); n != 1 {
		t.Fatalf("documents after adoption = %d", n)
	}
	if sourceEpoch(t, d, p.TaskID) <= epoch0 {
		t.Fatal("adoption did not advance source_epoch")
	}
	if b := blockerKinds(t, d, p.TaskID); !b["requirements_source_changed"] || b["required_material_incomplete"] {
		t.Fatalf("blockers after adoption: %v", b)
	}
	again, err := proc.Adopt(ctx, p.TaskID, rec.ID, 0, "reviewer", "")
	if err != nil || again.ID != rec.ID || docCount(t, d, p.TaskID) != 1 {
		t.Fatalf("adoption must be idempotent: %v", err)
	}
	if _, err := proc.Reject(ctx, p.TaskID, rec.ID, "reviewer", "改主意"); !errors.Is(err, db.ErrFileProcessingConflict) {
		t.Fatalf("rejecting an adopted candidate: %v", err)
	}

	// 拒绝：须写原因；拒绝人 / 时间持久化；不能再采用；候选正文与输入不可改写。
	other := putFile(t, store, p.TaskID, "答疑.md", ingest.FormatMarkdown, []byte("# 答疑\n\n1. 工期不变。\n"))
	j2, err := proc.Enqueue(ctx, p.TaskID, ProcessInput{FileID: other.ID, TenderNo: "T-S5A", RequestKey: "cand-2", CandidateOnly: true}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	runOne(t, runner)
	j2 = jobByID(t, d, p.TaskID, j2.ID)
	if _, err := proc.Reject(ctx, p.TaskID, *j2.RecordID, "reviewer", "  "); !errors.Is(err, db.ErrInvalidReview) {
		t.Fatalf("reject without reason: %v", err)
	}
	rej, err := proc.Reject(ctx, p.TaskID, *j2.RecordID, "reviewer", "与原件页码不符")
	if err != nil || rej.Status != db.RecordRejected || rej.RejectedBy != "reviewer" || rej.RejectedAt == nil || rej.RejectReason != "与原件页码不符" {
		t.Fatalf("reject: %v %+v", err, rej)
	}
	if again, err := proc.Reject(ctx, p.TaskID, rej.ID, "other", "重复"); err != nil || again.RejectedBy != "reviewer" {
		t.Fatalf("reject must be idempotent: %v %+v", err, again)
	}
	if _, err := proc.Adopt(ctx, p.TaskID, rej.ID, 0, "reviewer", ""); !errors.Is(err, db.ErrFileProcessingConflict) {
		t.Fatalf("adopting a rejected candidate: %v", err)
	}
	if _, err := d.Exec(`UPDATE file_processing_records SET payload='{}'::jsonb WHERE id=$1`, rej.ID); err == nil {
		t.Fatal("candidate payload must be immutable")
	}
	if _, err := d.Exec(`UPDATE file_processing_records SET status='ready' WHERE id=$1`, rej.ID); err == nil {
		t.Fatal("a rejected record must not return to ready")
	}
	if docCount(t, d, p.TaskID) != 1 {
		t.Fatal("rejection must not touch formal documents")
	}
	var biz string
	if err := d.QueryRow(`SELECT business_state FROM agent_job_outcome('tender_parse', $1)`, j2.ID).Scan(&biz); err != nil || biz != "rejected" {
		t.Fatalf("outcome of a rejected candidate: %q %v", biz, err)
	}
	// 已终态的作业不可再改状态（迟到写回被拒）。
	if _, err := d.Exec(`UPDATE fixed_file_jobs SET status='queued' WHERE id=$1`, j2.ID); err == nil {
		t.Fatal("terminal job status must be immutable")
	}
}

func TestParseRoutePinningTransientRetryAndBudget(t *testing.T) {
	d := dbtest.Open(t)
	ctx := context.Background()
	task, err := d.CreateTask(ctx, db.CreateTaskInput{Name: dbtest.Uniq("s5a-route"), Goal: "file"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(d, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 没有 OCR：auto PDF 仍可提交（扫描页记缺口），ocr_only / 图片拒绝，增强后端未交付，native 图片与 ocr_only DOCX 参数错误。
	none := &Processor{DB: d, Store: store}
	for _, c := range []struct {
		format, profile, executor string
		err                       error
	}{
		{ingest.FormatPDF, "", FixedParserVersion, nil},
		{ingest.FormatPDF, "native", FixedParserVersion, nil},
		{ingest.FormatPDF, "ocr_only", "", ErrOCRRequired},
		{ingest.FormatImage, "auto", "", ErrOCRRequired},
		{ingest.FormatImage, "native", "", ingest.ErrInvalidInput},
		{ingest.FormatDOCX, "ocr_only", "", ingest.ErrInvalidInput},
		{ingest.FormatPDF, "enhanced_pdf", "", ErrProfileDisabled},
		{ingest.FormatPDF, "magic", "", ingest.ErrInvalidInput},
	} {
		_, ex, err := none.ResolveRoute(ctx, c.format, c.profile)
		if ex != c.executor || (c.err == nil) != (err == nil) || (c.err != nil && !errors.Is(err, c.err)) {
			t.Fatalf("route %s/%s: %q %v", c.format, c.profile, ex, err)
		}
	}
	if _, _, err := none.ResolveRoute(ctx, ingest.FormatImage, "auto"); !errors.Is(err, ingest.ErrOCRUnavailable) {
		t.Fatalf("missing OCR must stay compatible with the 422 path: %v", err)
	}

	ocr := &versionedOCR{version: "ocr v1"}
	proc := &Processor{DB: d, Store: store, OCR: ocr}
	runner := &Runner{Processor: proc, Owner: "s5a-route", TaskID: task.ID, Lease: time.Minute, RetryDelay: 50 * time.Millisecond}
	img := putFile(t, store, task.ID, "scan.png", ingest.FormatImage, fakePNG)

	// 执行版本在首次提交时固定；执行前 OCR 换版本 → RUNTIME_UNAVAILABLE 失败，不保存候选。
	j, err := proc.Enqueue(ctx, task.ID, ProcessInput{FileID: img.ID, TenderNo: "T-R", RequestKey: "pin-1", CandidateOnly: true}, "tester")
	if err != nil || j.Executor != FixedParserVersion+"+ocr:ocr v1" {
		t.Fatalf("pinned executor: %v %+v", err, j)
	}
	ocr.set("ocr v2", false)
	runAny(t, runner)
	j = jobByID(t, d, task.ID, j.ID)
	if j.Status != "failed" || j.ErrorCode != db.JobErrRuntimeUnavailable || j.RecordID != nil {
		t.Fatalf("version drift: %+v", j)
	}
	// 同键重放返回原作业（不重新探测运行能力），新键按新版本固定。
	if again, err := proc.Enqueue(ctx, task.ID, ProcessInput{FileID: img.ID, TenderNo: "T-R", RequestKey: "pin-1", CandidateOnly: true}, "tester"); err != nil || again.ID != j.ID {
		t.Fatalf("replay: %v", err)
	}
	if _, err := proc.Enqueue(ctx, task.ID, ProcessInput{FileID: img.ID, TenderNo: "T-R", RequestKey: "pin-1", ParserProfile: "ocr_only"}, "tester"); !errors.Is(err, db.ErrIdempotencyKeyConflict) {
		t.Fatalf("same key, different input: %v", err)
	}

	// OCR 暂时不可达：作业回队列（计入尝试、原截止时间不变），恢复后成功生成候选。
	j, err = proc.Enqueue(ctx, task.ID, ProcessInput{FileID: img.ID, TenderNo: "T-R", RequestKey: "pin-2", CandidateOnly: true}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	deadline := j.DeadlineAt
	ocr.set("ocr v2", true)
	runAny(t, runner)
	j = jobByID(t, d, task.ID, j.ID)
	if j.Status != "queued" || j.Attempts != 1 || j.ErrorCode != db.JobErrRuntimeUnavailable || !j.DeadlineAt.Equal(deadline) {
		t.Fatalf("transient release: %+v", j)
	}
	if next, err := d.ClaimFixedFileJob(ctx, "probe", time.Minute, task.ID); err != nil || next != nil {
		t.Fatalf("retry_after must delay the next claim: %+v %v", next, err)
	}
	time.Sleep(80 * time.Millisecond)
	ocr.set("ocr v2", false)
	runOne(t, runner)
	j = jobByID(t, d, task.ID, j.ID)
	if j.Status != "succeeded" || j.Attempts != 2 || j.RecordStatus != db.RecordReady || j.ErrorCode != "" {
		t.Fatalf("recovered job: %+v", j)
	}
	rec, _ := d.GetFileProcessingRecord(ctx, db.DefaultTenant, task.ID, *j.RecordID, false)
	if rec.ParserVersion != FixedParserVersion+"+ocr:ocr v2" {
		t.Fatalf("parser version: %q", rec.ParserVersion)
	}

	// 一直不可达：3 次尝试后领取时以 BUDGET_EXHAUSTED 结束，并保留最近一次原因。
	j, err = proc.Enqueue(ctx, task.ID, ProcessInput{FileID: img.ID, TenderNo: "T-R", RequestKey: "pin-3", CandidateOnly: true}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	ocr.set("ocr v2", true)
	for i := 0; i < 3; i++ {
		runAny(t, runner)
		time.Sleep(80 * time.Millisecond)
	}
	if next, err := d.ClaimFixedFileJob(ctx, "probe", time.Minute, task.ID); err != nil || next != nil {
		t.Fatalf("exhausted job claimed: %+v %v", next, err)
	}
	j = jobByID(t, d, task.ID, j.ID)
	if j.Status != "failed" || j.ErrorCode != db.JobErrBudgetExhausted || !strings.Contains(j.Error, "最近一次") || j.Attempts != 3 {
		t.Fatalf("budget exhausted: %+v", j)
	}
}

// intentCtx 建立一个持有活租约的 Intent，并返回带宿主执行身份与范围的 Context。
func intentCtx(t *testing.T, d *db.DB, task *db.Task, m *db.IntentMandate) (context.Context, *db.ExplorationStore, int64, int64) {
	t.Helper()
	ctx := context.Background()
	es := db.NewExplorationStore(d, task.ExplorationID)
	payload := map[string]any{"summary": "解析招标原件"}
	if m != nil {
		payload["mandate"] = m
	}
	id, err := es.AddNode(ctx, db.KindIntent, db.StateIntentOpen, payload, 1, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	epoch, ok, err := es.ClaimIntentWithLease(ctx, id, "worker", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	return db.WithExecutionScope(db.WithIntentExecution(ctx, id, epoch), m), es, id, epoch
}

func TestAgentParseSubmissionScopeAndCompletion(t *testing.T) {
	d := dbtest.Open(t)
	ctx := context.Background()
	task, err := d.CreateTask(ctx, db.CreateTaskInput{Name: dbtest.Uniq("s5a-agent"), Goal: "file"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(d, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ocr := &versionedOCR{version: "ocr v1"}
	proc := &Processor{DB: d, Store: store, OCR: ocr}
	runner := &Runner{Processor: proc, Owner: "s5a-agent", TaskID: task.ID, Lease: time.Minute}
	first := putFile(t, store, task.ID, "招标文件.md", ingest.FormatMarkdown, []byte(s5aMarkdown))
	scan := putFile(t, store, task.ID, "资格附件.png", ingest.FormatImage, fakePNG)
	other := putFile(t, store, task.ID, "其他.md", ingest.FormatMarkdown, []byte("# 其他\n\n1. 说明。\n"))
	m := &db.IntentMandate{RequiredJobKinds: []string{db.JobKindTenderParse}, AllowedFileIDs: []int64{first.ID, scan.ID}}
	ictx, es, iid, epoch := intentCtx(t, d, task, m)

	// 没有宿主身份、未开放来源、未交付配置、参数组合错误都不插入作业。
	if _, err := proc.SubmitAgentParse(ctx, task.ID, "worker", AgentParseRequest{SourceKind: "tender_file", Operation: "import", FileID: scan.ID, RequestKey: "a"}); !errors.Is(err, db.ErrNoIntentExecution) {
		t.Fatalf("unbound: %v", err)
	}
	for _, c := range []struct {
		req AgentParseRequest
		err error
	}{
		{AgentParseRequest{SourceKind: "knowledge_file", Operation: "import", FileID: scan.ID, RequestKey: "a"}, ErrSourceKindDisabled},
		{AgentParseRequest{SourceKind: "tender_file", Operation: "import", FileID: scan.ID, ParserProfile: "enhanced_pdf", RequestKey: "a"}, ErrProfileDisabled},
		{AgentParseRequest{SourceKind: "tender_file", Operation: "import", FileID: scan.ID, DocumentID: 1, RequestKey: "a"}, ingest.ErrInvalidInput},
		{AgentParseRequest{SourceKind: "tender_file", Operation: "import", FileID: scan.ID, RequestKey: "sys:x"}, ingest.ErrInvalidInput},
		{AgentParseRequest{SourceKind: "tender_file", Operation: "delete", FileID: scan.ID, RequestKey: "a"}, ingest.ErrInvalidInput},
	} {
		if _, err := proc.SubmitAgentParse(ictx, task.ID, "worker", c.req); !errors.Is(err, c.err) {
			t.Fatalf("%+v: %v", c.req, err)
		}
	}
	// 本任务还没有正式招标文档：招标编号须由人补充，不臆造。
	var meta *MetadataRequiredError
	if _, err := proc.SubmitAgentParse(ictx, task.ID, "worker", AgentParseRequest{SourceKind: "tender_file", Operation: "import", FileID: scan.ID, RequestKey: "a"}); !errors.As(err, &meta) || meta.Missing[0] != "tender_no" {
		t.Fatalf("metadata: %v", err)
	}
	if _, err := proc.Process(ctx, task.ID, ProcessInput{FileID: first.ID, TenderNo: "T-AG", LotNo: "1", RequestKey: "human-first"}, "operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := proc.SubmitAgentParse(ictx, task.ID, "worker", AgentParseRequest{SourceKind: "tender_file", Operation: "import", FileID: scan.ID, TenderNo: "T-NEW", RequestKey: "a"}); !errors.As(err, &meta) {
		t.Fatalf("unknown tender_no: %v", err)
	}
	if _, err := proc.SubmitAgentParse(ictx, task.ID, "worker", AgentParseRequest{SourceKind: "tender_file", Operation: "import", FileID: other.ID, RequestKey: "a"}); err == nil || !strings.Contains(err.Error(), "execution scope") {
		t.Fatalf("file outside allowed_file_ids: %v", err)
	}

	// 受理：强制候选模式、固定执行版本、同事务订阅当前意图；同键重放返回原作业，同键异参冲突。
	sub, err := proc.SubmitAgentParse(ictx, task.ID, "worker", AgentParseRequest{SourceKind: "tender_file", Operation: "import", FileID: scan.ID, DocRole: "attachment", RequestKey: "agent-scan-1"})
	if err != nil {
		t.Fatal(err)
	}
	var in ProcessInput
	_ = json.Unmarshal(sub.Job.Input, &in)
	if !in.CandidateOnly || in.TenderNo != "T-AG" || sub.Job.Executor != FixedParserVersion+"+ocr:ocr v1" || sub.Subscription == nil || sub.Subscription.State != "waiting" || sub.Replayed {
		t.Fatalf("submission: %+v %+v", sub, in)
	}
	again, err := proc.SubmitAgentParse(ictx, task.ID, "worker", AgentParseRequest{SourceKind: "tender_file", Operation: "import", FileID: scan.ID, DocRole: "attachment", RequestKey: "agent-scan-1"})
	if err != nil || !again.Replayed || again.Job.ID != sub.Job.ID || again.Subscription.ID != sub.Subscription.ID {
		t.Fatalf("replay: %v %+v", err, again)
	}
	if _, err := proc.SubmitAgentParse(ictx, task.ID, "worker", AgentParseRequest{SourceKind: "tender_file", Operation: "import", FileID: scan.ID, ParserProfile: "ocr_only", RequestKey: "agent-scan-1"}); !errors.Is(err, db.ErrIdempotencyKeyConflict) {
		t.Fatalf("same key, different input: %v", err)
	}

	// 作业成功只是“候选就绪”：订阅收到 candidate_pending_review，不写正式文档。
	docsBefore := docCount(t, d, task.ID)
	runOne(t, runner)
	subs, err := d.ListAgentJobs(ictx, db.DefaultTenant, task.ID, db.AgentJobFilter{})
	if err != nil || len(subs) != 1 || subs[0].Event == nil || subs[0].Event.BusinessState != "candidate_pending_review" {
		t.Fatalf("event: %v %+v", err, subs)
	}
	var payload map[string]any
	_ = json.Unmarshal(subs[0].Event.Payload, &payload)
	if payload["candidate_only"] != true || payload["executor"] != FixedParserVersion+"+ocr:ocr v1" || payload["operation"] != "import" {
		t.Fatalf("event payload: %v", payload)
	}
	if docCount(t, d, task.ID) != docsBefore {
		t.Fatal("agent candidate wrote a formal document")
	}
	job := jobByID(t, d, task.ID, sub.Job.ID)
	if r, err := d.GetTenderParseRecord(ictx, db.DefaultTenant, task.ID, *job.RecordID); err != nil || r.Operation != "import" {
		t.Fatalf("record read: %v", err)
	}
	// 宿主限定对象时，只能读本意图作业产生的记录。
	octx, _, _, _ := intentCtx(t, d, task, &db.IntentMandate{RestrictObjects: true, AllowedJobKinds: []string{db.JobKindTenderParse}, AllowedFileIDs: []int64{scan.ID}})
	if _, err := d.GetTenderParseRecord(octx, db.DefaultTenant, task.ID, *job.RecordID); err == nil || !strings.Contains(err.Error(), "execution scope") {
		t.Fatalf("record of another intent: %v", err)
	}
	if _, err := d.GetTenderParseJob(octx, db.DefaultTenant, task.ID, job.ID); err == nil {
		t.Fatal("job of another intent must not be readable under restrict_objects")
	}

	// 投递并确认事件后收尾：必需作业 tender_parse 的候选仍有效 → done。
	ev, ok, err := es.TakeAgentJobEvent(ctx, iid, epoch)
	if err != nil || !ok {
		t.Fatalf("take event: %v %v", ok, err)
	}
	outcome, detail, err := es.FinalizeIntentTurn(ctx, iid, epoch, []int64{ev.EventID})
	if err != nil || outcome != db.TurnDone {
		t.Fatalf("finalize: %s %s %v", outcome, detail, err)
	}

	// 被人工拒绝的候选不算交付：另一意图的必需作业因候选被拒而阻塞。
	ictx2, es2, iid2, epoch2 := intentCtx(t, d, task, &db.IntentMandate{RequiredJobKinds: []string{db.JobKindTenderParse}})
	docs, _ := d.ListTenderDocuments(ctx, task.ID)
	rev := docs[0].ParseRevision
	if _, err := proc.SubmitAgentParse(ictx2, task.ID, "worker", AgentParseRequest{SourceKind: "tender_file", Operation: "reparse", DocumentID: docs[0].ID, ExpectedRevision: intp(rev + 1), RequestKey: "re-1"}); !errors.Is(err, db.ErrParseRevisionConflict) {
		t.Fatalf("stale parse revision: %v", err)
	}
	if _, err := proc.SubmitAgentParse(ictx2, task.ID, "worker", AgentParseRequest{SourceKind: "tender_file", Operation: "reparse", DocumentID: docs[0].ID, ExpectedRevision: intp(rev), TenderNo: "T-AG", RequestKey: "re-1"}); !errors.Is(err, ingest.ErrInvalidInput) {
		t.Fatalf("reparse with metadata: %v", err)
	}
	re, err := proc.SubmitAgentParse(ictx2, task.ID, "worker", AgentParseRequest{SourceKind: "tender_file", Operation: "reparse", DocumentID: docs[0].ID, ExpectedRevision: intp(rev), RequestKey: "re-1"})
	if err != nil || re.Subscription.ExpectedVersion == nil || *re.Subscription.ExpectedVersion != int64(rev) {
		t.Fatalf("reparse submit: %v %+v", err, re)
	}
	runOne(t, runner)
	reJob := jobByID(t, d, task.ID, re.Job.ID)
	if reJob.RecordStatus != db.RecordReady {
		t.Fatalf("reparse job: %+v", reJob)
	}
	rr, _ := d.GetFileProcessingRecord(ctx, db.DefaultTenant, task.ID, *reJob.RecordID, false)
	if cp, err := Candidate(rr, 0, 5, 4000, 0); err != nil || cp.Diff == nil || cp.Operation != "reparse" {
		t.Fatalf("reparse candidate: %v %+v", err, cp)
	}
	if _, err := proc.Reject(ctx, task.ID, rr.ID, "reviewer", "无需重新解析"); err != nil {
		t.Fatal(err)
	}
	ev2, ok, err := es2.TakeAgentJobEvent(ctx, iid2, epoch2)
	if err != nil || !ok {
		t.Fatalf("take event 2: %v %v", ok, err)
	}
	if outcome, detail, err := es2.FinalizeIntentTurn(ctx, iid2, epoch2, []int64{ev2.EventID}); err != nil || outcome != db.TurnBlocked || !strings.Contains(detail, "no longer valid") {
		t.Fatalf("rejected candidate must not count: %s %s %v", outcome, detail, err)
	}
}

func intp(v int) *int { return &v }

// 真实 PaddleOCR 端到端：扫描件按 auto 路由生成候选（执行版本锁定 PaddleOCR 与模型），候选逐行保留 OCR 识别框与置信度，
// 人工采用后正式入库并推进来源 epoch。
func TestRealOCRCandidateImportEndToEnd(t *testing.T) {
	url := strings.TrimSpace(os.Getenv("AUTOBID_OCR_URL"))
	if url == "" {
		t.Skip("AUTOBID_OCR_URL not set: start tools/paddleocr/service.py to run the real OCR test")
	}
	d := dbtest.Open(t)
	ctx := context.Background()
	scan, err := os.ReadFile("../ingest/testdata/scan_tender_page.jpg")
	if err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTask(ctx, db.CreateTaskInput{Name: dbtest.Uniq("s5a-real-ocr"), Goal: "file"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(d, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proc := &Processor{DB: d, Store: store, OCR: &ingest.HTTPOCR{URL: url, Token: os.Getenv("AUTOBID_OCR_TOKEN")}}
	runner := &Runner{Processor: proc, Owner: "s5a-real", TaskID: task.ID, Lease: time.Minute}
	pdf := ingesttest.BuildPDF("扫描招标件",
		ingesttest.Page{Texts: []ingesttest.Text{{X: 72, Y: 760, Size: 14, S: "第一章 总则"}, {X: 72, Y: 730, Size: 11, S: "1.1 本项目为数据中心改造工程。"}}},
		ingesttest.Page{JPEG: scan, JPEGWidth: 900, JPEGHeight: 1270})
	obj := putFile(t, store, task.ID, "扫描招标件.pdf", ingest.FormatPDF, pdf)
	j, err := proc.Enqueue(ctx, task.ID, ProcessInput{FileID: obj.ID, TenderNo: "T-OCR", RequestKey: "real-1", CandidateOnly: true}, "tester")
	if err != nil || !strings.Contains(j.Executor, "+ocr:paddleocr") {
		t.Fatalf("enqueue: %v %+v", err, j)
	}
	start := time.Now()
	runOne(t, runner)
	j = jobByID(t, d, task.ID, j.ID)
	if j.Status != "succeeded" || j.RecordStatus != db.RecordReady {
		t.Fatalf("job: %+v", j)
	}
	rec, _ := d.GetFileProcessingRecord(ctx, db.DefaultTenant, task.ID, *j.RecordID, false)
	page, err := Candidate(rec, 0, 20, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("real OCR candidate in %v: %s, %d sections, ocr pages %v, gaps %+v", time.Since(start).Round(time.Millisecond), rec.ParserVersion, page.TotalSections, page.OCRPages, page.Gaps)
	if rec.ParserVersion != j.Executor || len(page.OCRPages) != 1 || page.OCRPages[0] != 2 {
		t.Fatalf("candidate: %+v", page)
	}
	ocrLines, text := 0, ""
	for _, s := range page.Sections {
		text += s.Content
		if s.Locator.OCR {
			ocrLines += s.Locator.OCRLines
			if s.Locator.PageStart != 2 || s.Locator.MinConfidence <= 0 || s.Locator.SHA256 != obj.SHA256 || len(s.Locator.Boxes) == 0 || s.Locator.Boxes[0].ImgW != 900 {
				t.Fatalf("OCR locator: %+v", s.Locator)
			}
		}
	}
	if ocrLines < 15 || !strings.Contains(strings.ReplaceAll(text, " ", ""), "服务器数量不少于12台") {
		t.Fatalf("OCR lines=%d text=%q", ocrLines, text)
	}
	epoch0 := sourceEpoch(t, d, task.ID)
	if _, err := proc.Adopt(ctx, task.ID, rec.ID, 0, "reviewer", "对照扫描原件核对"); err != nil {
		t.Fatal(err)
	}
	docs, _ := d.ListTenderDocuments(ctx, task.ID)
	if len(docs) != 1 || docs[0].PageCount != 2 || sourceEpoch(t, d, task.ID) <= epoch0 {
		t.Fatalf("adopted document: %+v", docs)
	}
}
