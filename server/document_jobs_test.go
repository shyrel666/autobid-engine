package server

// S5a 招标解析 Skill 的 HTTP 与引擎集成测试（需要 AUTOBID_TEST_DATABASE_URL）：
//   - /file-jobs 解析配置与候选模式（202）、ocr_only 无 OCR 503、enhanced_pdf 422、同键异参 409；
//   - 候选详情按段分页并附原件定位；/reject 须写原因、持久化拒绝人与原因，已采用不能拒绝、已拒绝不能采用；导入候选按 expected_revision=0 采用；
//   - Worker：经 document_parse_submit 提交并订阅 → 意图等待后台作业 → 候选就绪唤醒 → 必需作业（tender_parse）校验通过后 done，
//     Planner 未收到“已结束”。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/bidtools"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/filework"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/tool"
)

const parseMarkdown = "# 第一章 总则\n\n1.1 本项目为数据中心改造工程。\n\n## 第二章 技术要求\n\n2.1 ★服务器数量不少于12台。\n\n2.2 质保期3年。\n"

func TestHTTPTenderParseCandidates(t *testing.T) {
	c := newFileAPI(t)
	ctx := context.Background()
	task := c.createTask(t)
	base := fmt.Sprintf("/api/tasks/%d", task)
	md := streamFile(t, c, task, "补充技术要求.md", []byte(parseMarkdown))
	scan := streamFile(t, c, task, "scan.pdf", []byte("%PDF-1.7\n%%EOF\n"))

	type errBody struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	var e errBody
	if code := c.raw("POST", base+"/file-jobs", map[string]any{"file_id": scan.ID, "tender_no": "T-H", "request_key": "ocr-only", "parser_profile": "ocr_only"}, &e); code != 503 || e.Code != "runtime_unavailable" {
		t.Fatalf("ocr_only without OCR: %d %+v", code, e)
	}
	if code := c.raw("POST", base+"/file-jobs", map[string]any{"file_id": scan.ID, "tender_no": "T-H", "request_key": "enh", "parser_profile": "enhanced_pdf"}, &e); code != 422 || e.Code != "feature_disabled" {
		t.Fatalf("enhanced_pdf: %d %+v", code, e)
	}
	var job db.FixedFileJob
	if code := c.do("POST", base+"/file-jobs", map[string]any{"file_id": md.ID, "tender_no": "T-H", "request_key": "cand-1", "candidate_only": true, "parser_profile": "auto"}, &job); code != 202 || job.Executor != filework.FixedParserVersion {
		t.Fatalf("enqueue candidate: %d %+v", code, job)
	}
	if code := c.raw("POST", base+"/file-jobs", map[string]any{"file_id": md.ID, "tender_no": "T-H", "request_key": "cand-1"}, &e); code != 409 || e.Code != "idempotency_conflict" {
		t.Fatalf("same key, different input: %d %+v", code, e)
	}
	runner := fixedRunner(c, task)
	if ok, err := runner.RunOnce(ctx); !ok || err != nil {
		t.Fatalf("run: %v %v", ok, err)
	}
	if code := c.do("GET", fmt.Sprintf("%s/file-jobs/%d", base, job.ID), nil, &job); code != 200 || job.Status != "succeeded" || job.RecordStatus != db.RecordReady {
		t.Fatalf("job: %d %+v", code, job)
	}
	var detail struct {
		Record    db.FileProcessingRecord `json:"record"`
		Stale     bool                    `json:"stale"`
		Candidate *filework.CandidatePage `json:"candidate"`
		Gaps      []map[string]any        `json:"gaps"`
	}
	if code := c.do("GET", fmt.Sprintf("%s/file-processing-records/%d", base, *job.RecordID), nil, &detail); code != 200 || detail.Stale || detail.Candidate == nil ||
		!detail.Record.CandidateOnly || detail.Record.ParserProfile != "auto" || len(detail.Candidate.Sections) == 0 || detail.Candidate.Sections[0].Locator.SHA256 != md.SHA256 {
		t.Fatalf("detail: %d %+v", code, detail)
	}
	var docs []db.TenderDocument
	c.do("GET", base+"/documents", nil, &docs)
	if len(docs) != 0 {
		t.Fatalf("candidate imported a formal document: %+v", docs)
	}

	// 拒绝须写原因，拒绝后不能采用；另一份候选按 expected_revision=0 采用，之后不能再拒绝。
	recPath := fmt.Sprintf("%s/file-processing-records/%d", base, *job.RecordID)
	if code := c.do("POST", recPath+"/reject", map[string]any{"reason": ""}, nil); code != 400 {
		t.Fatalf("reject without reason: %d", code)
	}
	var rejected db.FileProcessingRecord
	if code := c.do("POST", recPath+"/reject", map[string]any{"reason": "页码与原件不符"}, &rejected); code != 200 || rejected.Status != db.RecordRejected || rejected.RejectReason != "页码与原件不符" || rejected.RejectedBy == "" {
		t.Fatalf("reject: %d %+v", code, rejected)
	}
	if code := c.do("POST", recPath+"/adopt", map[string]any{"expected_revision": 0}, nil); code != 409 {
		t.Fatalf("adopt rejected: %d", code)
	}
	var job2 db.FixedFileJob
	if code := c.do("POST", base+"/file-jobs", map[string]any{"file_id": md.ID, "tender_no": "T-H", "request_key": "cand-2", "candidate_only": true}, &job2); code != 202 {
		t.Fatalf("enqueue second: %d", code)
	}
	if ok, err := runner.RunOnce(ctx); !ok || err != nil {
		t.Fatalf("run: %v %v", ok, err)
	}
	c.do("GET", fmt.Sprintf("%s/file-jobs/%d", base, job2.ID), nil, &job2)
	rec2 := fmt.Sprintf("%s/file-processing-records/%d", base, *job2.RecordID)
	var adopted db.FileProcessingRecord
	if code := c.do("POST", rec2+"/adopt", map[string]any{"expected_revision": 0, "review_note": "已对照原件"}, &adopted); code != 200 || adopted.Status != db.RecordAdopted {
		t.Fatalf("adopt import candidate: %d %+v", code, adopted)
	}
	if code := c.do("POST", rec2+"/reject", map[string]any{"reason": "撤回"}, nil); code != 409 {
		t.Fatalf("reject adopted: %d", code)
	}
	c.do("GET", base+"/documents", nil, &docs)
	if len(docs) != 1 || docs[0].OriginalFileID == nil || *docs[0].OriginalFileID != md.ID {
		t.Fatalf("documents after adoption: %+v", docs)
	}
}

// Worker 路径：document_parse_submit 提交并订阅 → 等待 → 候选就绪唤醒 → 必需作业（tender_parse）校验通过后 done。
func TestEngineDocumentParseSubmitWaitsForCandidate(t *testing.T) {
	pg := testDB(t)
	store, err := filework.NewStore(pg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proc := &filework.Processor{DB: pg, Store: store}
	var (
		envRef    atomic.Pointer[harnessEnv]
		fileID    int64
		plannerMu sync.Mutex
		seen      []string
	)
	jobs := &filework.Runner{Processor: proc, Owner: "engine-test", Poll: 100 * time.Millisecond}
	submit := tool.New(tool.Spec{Name: bidtools.ToolDocumentParseSubmit, Description: "提交招标解析作业"},
		func(ctx context.Context, in json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
			task := envRef.Load().task
			for _, tl := range bidtools.New(pg, task.ID, task.Assets, task.Store).WithFileProcessing(proc, jobs).DocumentJobTools() {
				if tl.Name() == bidtools.ToolDocumentParseSubmit {
					return tl.Call(ctx, in, tc)
				}
			}
			return tool.Errorf("document_parse_submit not registered"), nil
		})
	planner := func(env *harnessEnv) brain {
		envRef.Store(env)
		return func(req llm.CompletionRequest) llm.Message {
			plannerMu.Lock()
			seen = append(seen, lastUserText(req))
			plannerMu.Unlock()
			if _, ok := lastToolResult(req); ok {
				return say("本轮规划完成")
			}
			if ns, _ := env.task.Store.NodesByKind(context.Background(), db.KindIntent, nil, 1); len(ns) == 0 {
				return use(agent.ToolAddIntent, map[string]any{"summary": "解析招标包中的补充技术要求附件",
					"mandate": map[string]any{"require_writeback": true, "allowed_job_kinds": []string{db.JobKindTenderParse},
						"required_job_kinds": []string{db.JobKindTenderParse}, "allowed_file_ids": []int64{fileID}}})
			}
			return say("暂无新方向")
		}
	}
	var lastResult atomic.Value
	worker := brain(func(req llm.CompletionRequest) llm.Message {
		prev, isResult := lastToolResult(req)
		switch {
		case strings.Contains(lastUserText(req), "后台作业事件 #"):
			return say("解析候选已就绪，等待人工在材料处理页采用")
		case isResult && prev == bidtools.ToolDocumentParseSubmit:
			lastResult.Store(lastToolResultText(req))
			return say("已提交解析作业，等待后台处理")
		case isResult:
			return say("工具已返回")
		default:
			return use(bidtools.ToolDocumentParseSubmit, map[string]any{"source_kind": "tender_file", "operation": "import", "file_id": fileID,
				"doc_role": "attachment", "request_key": "agent-parse-1"})
		}
	})
	env := newEnvWith(t, 0, false, worker, Config{JobWakePoll: 100 * time.Millisecond}, envOpts{extraTools: []tool.CoreTool{submit}, planner: planner,
		beforeRun: func(task *Task) {
			ctx := context.Background()
			first, err := store.PutBytes(ctx, filework.PutOptions{TenantID: db.DefaultTenant, TaskID: task.ID, Kind: db.FileKindOriginal, OriginalName: "招标文件.md", Format: "markdown"}, []byte(parseMarkdown))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := proc.Process(ctx, task.ID, filework.ProcessInput{FileID: first.ID, TenderNo: "T-ENG", RequestKey: "human-1"}, "operator"); err != nil {
				t.Fatal(err)
			}
			supp, err := store.PutBytes(ctx, filework.PutOptions{TenantID: db.DefaultTenant, TaskID: task.ID, Kind: db.FileKindOriginal, OriginalName: "补充技术要求.md", Format: "markdown"},
				[]byte(parseMarkdown+"\n## 第三章 附件\n\n3.1 设备清单。\n"))
			if err != nil {
				t.Fatal(err)
			}
			fileID = supp.ID
			jobs.TaskID = task.ID
		}})
	ctx := context.Background()

	var in *db.Node
	waitFor(t, 15*time.Second, "intent waiting for its parse job", func() bool {
		ns, _ := env.task.Store.NodesByKind(ctx, db.KindIntent, nil, 1)
		if len(ns) == 1 && db.IsWaitingIntent(ns[0]) {
			in = ns[0]
		}
		return in != nil
	})
	if s, _ := lastResult.Load().(string); !strings.Contains(s, `"accepted":true`) || !strings.Contains(s, `"kind":"tender_parse"`) || !strings.Contains(s, `"candidate_only":true`) {
		t.Fatalf("submit result: %s", s)
	}
	jobs.OnChange = func(*db.FixedFileJob) { env.e.WakeAgentJobs() }
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go jobs.Run(runCtx)
	waitFor(t, 30*time.Second, "intent done after the candidate became ready", func() bool {
		n, _ := env.task.Store.GetNode(ctx, in.ID)
		return n != nil && n.State == db.StateIntentDone
	})
	subs, err := pg.ListAgentJobs(ctx, db.DefaultTenant, env.task.ID, db.AgentJobFilter{IntentID: in.ID})
	if err != nil || len(subs) != 1 || subs[0].Job.Kind != db.JobKindTenderParse || subs[0].Event == nil ||
		subs[0].Event.BusinessState != "candidate_pending_review" || subs[0].State != "acknowledged" {
		t.Fatalf("subscription: %+v %v", subs, err)
	}
	j, err := pg.GetFixedFileJob(ctx, db.DefaultTenant, env.task.ID, subs[0].Job.ID)
	if err != nil || j.RecordStatus != db.RecordReady || j.CreatedBy != "worker" {
		t.Fatalf("job: %+v %v", j, err)
	}
	docs, _ := pg.ListTenderDocuments(ctx, env.task.ID)
	if len(docs) != 1 {
		t.Fatalf("the agent candidate must stay out of formal documents until adopted: %d documents", len(docs))
	}
	plannerMu.Lock()
	defer plannerMu.Unlock()
	for _, s := range seen {
		if strings.Contains(s, fmt.Sprintf("意图 #%d 已结束（blocked", in.ID)) {
			t.Fatalf("planner told a waiting intent had ended:\n%s", s)
		}
	}
}
