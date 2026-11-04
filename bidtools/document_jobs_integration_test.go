package bidtools

// document-processing 工具对真实 PostgreSQL 的集成测试（需要 AUTOBID_TEST_DATABASE_URL）：提交门禁错误码（FEATURE_DISABLED、
// SCOPE_VIOLATION、INVALID_ARGUMENT、HUMAN_ACTION_REQUIRED、RUNTIME_UNAVAILABLE、CONFLICT_STALE_REVISION、IDEMPOTENCY_KEY_CONFLICT）、
// 受理后强制候选模式并订阅当前意图、同键重放、状态查询、候选分页读取（原件定位与 next_cursor）与取消的意图范围。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
	"github.com/autobid/autobid-engine/filework"
	"github.com/autobid/autobid-engine/ingest"
	"github.com/autobid/autobid-engine/internal/tool"
)

type countingJobs struct{ woke, cancelled int }

func (c *countingJobs) Wake()        { c.woke++ }
func (c *countingJobs) Cancel(int64) { c.cancelled++ }

const docToolsMarkdown = "# 第一章 总则\n\n1.1 本项目为数据中心改造工程。\n\n## 第二章 技术要求\n\n2.1 ★服务器数量不少于12台。\n\n2.2 质保期3年。\n\n## 第三章 商务要求\n\n3.1 付款方式按合同约定。\n"

func TestDocumentParseTools(t *testing.T) {
	d := dbtest.Open(t)
	ctx := context.Background()
	task, err := d.CreateTask(ctx, db.CreateTaskInput{Name: dbtest.Uniq("s5a-tools"), Goal: "file"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := filework.NewStore(d, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	put := func(name, format, body string) *db.TaskFileObject {
		obj, err := store.PutBytes(ctx, filework.PutOptions{TenantID: db.DefaultTenant, TaskID: task.ID, Kind: db.FileKindOriginal, OriginalName: name, Format: format}, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return obj
	}
	first := put("招标文件.md", ingest.FormatMarkdown, docToolsMarkdown)
	supp := put("补充文件.md", ingest.FormatMarkdown, docToolsMarkdown+"\n## 第四章 附件\n\n4.1 附件清单。\n")
	scan := put("扫描件.png", ingest.FormatImage, "\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	proc := &filework.Processor{DB: d, Store: store} // no OCR configured
	es := db.NewExplorationStore(d, task.ExplorationID)
	intent := func(m *db.IntentMandate) context.Context {
		t.Helper()
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
			t.Fatalf("claim intent: %v %v", ok, err)
		}
		return db.WithExecutionScope(db.WithIntentExecution(ctx, id, epoch), m)
	}
	jobs := &countingJobs{}
	newKit := func(withFiles bool) map[string]tool.CoreTool {
		k := New(d, task.ID, db.NewAssetStore(d, task.ID), es)
		if withFiles {
			k.WithFileProcessing(proc, jobs)
		}
		out := map[string]tool.CoreTool{}
		for _, tl := range k.DeferredTools() {
			out[tl.Name()] = tl
		}
		return out
	}
	call := func(tools map[string]tool.CoreTool, c context.Context, name string, in any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(in)
		res, err := tools[name].Call(c, raw, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
			t.Fatalf("%s returned non-JSON %q", name, res.Content)
		}
		if res.IsError != (out["accepted"] == false) {
			t.Fatalf("%s: IsError=%v but body %s", name, res.IsError, res.Content)
		}
		return out
	}
	code := func(out map[string]any) string { s, _ := out["error_code"].(string); return s }
	submit := func(extra map[string]any) map[string]any {
		in := map[string]any{"source_kind": "tender_file", "operation": "import", "request_key": "k"}
		for k, v := range extra {
			in[k] = v
		}
		return in
	}

	bound := intent(nil)
	if out := call(newKit(false), bound, ToolDocumentParseSubmit, submit(map[string]any{"file_id": supp.ID})); code(out) != codeFeatureDisabled {
		t.Fatalf("not configured: %v", out)
	}
	tools := newKit(true)
	if out := call(tools, ctx, ToolDocumentParseSubmit, submit(map[string]any{"file_id": supp.ID})); code(out) != codeScopeViolation {
		t.Fatalf("no host intent: %v", out)
	}
	for _, c := range []struct {
		in   map[string]any
		want string
	}{
		{submit(map[string]any{"source_kind": "company_evidence", "file_id": supp.ID}), codeFeatureDisabled},
		{submit(map[string]any{"file_id": supp.ID, "parser_profile": "enhanced_pdf"}), codeFeatureDisabled},
		{submit(map[string]any{"file_id": supp.ID, "document_id": 9}), codeInvalidArgument},
		{submit(map[string]any{"file_id": supp.ID, "request_key": ""}), codeInvalidArgument},
		{submit(map[string]any{"file_id": supp.ID}), codeHumanAction}, // 本任务还没有正式招标文档
		{submit(map[string]any{"file_id": scan.ID, "tender_no": "X"}), codeHumanAction},
	} {
		if out := call(tools, bound, ToolDocumentParseSubmit, c.in); code(out) != c.want {
			t.Fatalf("%v: %v", c.in, out)
		}
	}
	// 人在页面导入首份招标文件后，Agent 才能以已有编号提交补充文件。
	if _, err := proc.Process(ctx, task.ID, filework.ProcessInput{FileID: first.ID, TenderNo: "T-TOOL", RequestKey: "human-1"}, "operator"); err != nil {
		t.Fatal(err)
	}
	if out := call(tools, bound, ToolDocumentParseSubmit, submit(map[string]any{"file_id": scan.ID})); code(out) != codeRuntimeUnavailable {
		t.Fatalf("image without OCR: %v", out)
	}
	scoped := intent(&db.IntentMandate{AllowedFileIDs: []int64{scan.ID}})
	if out := call(tools, scoped, ToolDocumentParseSubmit, submit(map[string]any{"file_id": supp.ID})); code(out) != codeScopeViolation {
		t.Fatalf("file outside allowed_file_ids: %v", out)
	}
	doc, _ := d.ListTenderDocuments(ctx, task.ID)
	if out := call(tools, bound, ToolDocumentParseSubmit, map[string]any{"source_kind": "tender_file", "operation": "reparse", "document_id": doc[0].ID,
		"expected_parse_revision": doc[0].ParseRevision + 1, "request_key": "re"}); code(out) != codeStaleRevision {
		t.Fatalf("stale parse_revision: %v", out)
	}

	out := call(tools, bound, ToolDocumentParseSubmit, submit(map[string]any{"file_id": supp.ID, "request_key": "supp-1", "doc_role": "attachment"}))
	if out["accepted"] != true || out["candidate_only"] != true || out["status"] != "queued" || out["business_state"] != "pending" || out["tender_no"] != "T-TOOL" ||
		out["executor"] != filework.FixedParserVersion || out["next_action"] != "wait_for_job_event" || jobs.woke != 1 {
		t.Fatalf("accepted: %v", out)
	}
	ref := out["job_ref"].(map[string]any)
	if again := call(tools, bound, ToolDocumentParseSubmit, submit(map[string]any{"file_id": supp.ID, "request_key": "supp-1", "doc_role": "attachment"})); again["replayed"] != true || again["job_ref"].(map[string]any)["id"] != ref["id"] {
		t.Fatalf("replay: %v", again)
	}
	if conflict := call(tools, bound, ToolDocumentParseSubmit, submit(map[string]any{"file_id": supp.ID, "request_key": "supp-1"})); code(conflict) != codeIdempotencyKey {
		t.Fatalf("same key, different input: %v", conflict)
	}
	subs, err := d.ListAgentJobs(bound, db.DefaultTenant, task.ID, db.AgentJobFilter{})
	if err != nil || len(subs) != 1 || subs[0].State != "waiting" {
		t.Fatalf("subscription: %v %+v", err, subs)
	}

	// 执行后：状态给出候选记录引用；结果按 next_cursor 分页并附原件定位。
	runner := &filework.Runner{Processor: proc, Owner: "tools", TaskID: task.ID, Lease: time.Minute}
	if ok, err := runner.RunOnce(ctx); !ok || err != nil {
		t.Fatalf("run: %v %v", ok, err)
	}
	st := call(tools, bound, ToolDocumentParseStatus, map[string]any{"job_ref": ref})
	if st["status"] != "succeeded" || st["business_state"] != "candidate_pending_review" || st["next_action"] != "read_candidate_then_await_human_adoption" {
		t.Fatalf("status: %v", st)
	}
	recRef := st["record_ref"].(map[string]any)
	if bad := call(tools, bound, ToolDocumentParseResult, map[string]any{"record_ref": recRef, "max_chars": 100}); code(bad) != codeInvalidArgument {
		t.Fatalf("max_chars bound: %v", bad)
	}
	res := call(tools, bound, ToolDocumentParseResult, map[string]any{"record_ref": recRef, "limit_sections": 2, "max_chars": 500})
	cand := res["candidate"].(map[string]any)
	secs := cand["sections"].([]any)
	loc := secs[0].(map[string]any)["locator"].(map[string]any)
	if len(secs) != 2 || cand["next_cursor"].(float64) != 2 || loc["sha256"] != supp.SHA256 || res["next_action"] != "await_human_adoption" || res["machine_recognized"] != false {
		t.Fatalf("result page: %v", res)
	}
	next := call(tools, bound, ToolDocumentParseResult, map[string]any{"record_ref": recRef, "cursor": 2, "limit_sections": 20})
	if next["candidate"].(map[string]any)["next_cursor"].(float64) != 0 {
		t.Fatalf("last page: %v", next)
	}
	// 其他意图（限定对象）不能读取、取消本意图的作业与记录。
	other := intent(&db.IntentMandate{RestrictObjects: true, AllowedJobKinds: []string{db.JobKindTenderParse}, AllowedFileIDs: []int64{supp.ID}})
	if o := call(tools, other, ToolDocumentParseStatus, map[string]any{"job_ref": ref}); code(o) != codeScopeViolation {
		t.Fatalf("other intent status: %v", o)
	}
	if o := call(tools, other, ToolDocumentParseResult, map[string]any{"record_ref": recRef}); code(o) != codeScopeViolation {
		t.Fatalf("other intent result: %v", o)
	}
	if o := call(tools, other, ToolDocumentParseCancel, map[string]any{"job_ref": ref}); code(o) != codeScopeViolation {
		t.Fatalf("other intent cancel: %v", o)
	}
	if o := call(tools, bound, ToolDocumentParseStatus, map[string]any{"job_ref": map[string]any{"kind": "chart_render", "id": 1}}); code(o) != codeInvalidArgument {
		t.Fatalf("job_ref kind: %v", o)
	}

	// 取消排队中的作业；已终态的作业原样返回。
	queued := call(tools, bound, ToolDocumentParseSubmit, submit(map[string]any{"file_id": supp.ID, "request_key": "supp-2", "parser_profile": "native"}))
	c := call(tools, bound, ToolDocumentParseCancel, map[string]any{"job_ref": queued["job_ref"]})
	if c["status"] != "cancelled" || c["error_code"] != db.JobErrCancelled || jobs.cancelled != 1 || c["next_action"] != "replan_if_still_needed" {
		t.Fatalf("cancel: %v", c)
	}
	if done := call(tools, bound, ToolDocumentParseCancel, map[string]any{"job_ref": ref}); done["status"] != "succeeded" {
		t.Fatalf("cancel terminal job: %v", done)
	}
	if !strings.Contains(toolDescription(tools[ToolDocumentParseResult]), "未经人工核验") {
		t.Fatal("result tool must state that OCR text is unverified")
	}
}

func toolDescription(tl tool.CoreTool) string { return tl.Description() }
