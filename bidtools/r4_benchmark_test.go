package bidtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
	"github.com/autobid/autobid-engine/ingest"
	"github.com/autobid/autobid-engine/ingest/ingesttest"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/llm/openai"
)

// Use the normal outline and revision APIs for the current draft. Seed extra conflict
// candidates without changing the main revision pointer, as concurrent writers can do.
func sectionContextFixture(tb testing.TB, candidates int) (*db.DB, int64, int64) {
	tb.Helper()
	pg := dbtest.Open(tb)
	ctx := context.Background()
	task, err := pg.CreateTask(ctx, db.CreateTaskInput{Name: dbtest.Uniq("r4-context"), Goal: "章节上下文基准", LegacyWriting: true})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := pg.DeleteTask(cleanup, task.ID); err != nil {
			tb.Error(err)
		}
	})
	p := dbtest.ReadyTask(tb, pg, task.ID, dbtest.DefaultSpec(0))
	sectionID := p.Sections["1"]
	if candidates > 0 {
		body := strings.Repeat("候选稿正文仅在人工查询历史时需要返回。\n", 640)
		_, err := pg.ExecContext(ctx, `INSERT INTO section_revisions
			(tenant_id, task_id, section_id, rev_no, base_rev_no, based_on_outline_version, source, content_md, status)
			SELECT $1, $2, $3, n + 1, 0, $4, 'ai_rewrite', $5, 'conflict_candidate'
			FROM generate_series(1, $6::integer) AS n`, db.DefaultTenant, task.ID, sectionID, p.OutlineVersion, body, candidates)
		if err != nil {
			tb.Fatal(err)
		}
	}
	return pg, task.ID, sectionID
}

func TestSectionWritingContextUsesCurrentRevisionWithConflictCandidates(t *testing.T) {
	pg, taskID, sectionID := sectionContextFixture(t, 2)
	ctx := context.Background()
	detail, err := pg.GetSection(ctx, db.DefaultTenant, taskID, sectionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Revisions) != 3 || detail.Revisions[0].RevNo != 3 || detail.Current == nil || detail.Current.RevNo != 1 {
		t.Fatalf("detail must retain the candidates and current revision: %+v", detail)
	}
	sc, err := BuildSectionContext(ctx, pg, db.DefaultTenant, taskID, sectionID)
	if err != nil {
		t.Fatal(err)
	}
	if sc.BaseRevNo != 1 || sc.BasedOnOutlineVersion != detail.ApprovedVersion || !sc.Writable ||
		sc.Current == nil || sc.Current.RevNo != 1 || sc.Current.ContentMD != detail.Current.ContentMD || len(sc.Requirements) != 2 {
		t.Fatalf("unexpected writing context: %+v", sc)
	}
	if _, err := BuildSectionContext(ctx, pg, "other-tenant", taskID, sectionID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("cross-tenant context read: %v", err)
	}
}

func BenchmarkR4SectionContext(b *testing.B) {
	for _, candidates := range []int{0, 80} {
		b.Run(fmt.Sprintf("candidates-%d", candidates), func(b *testing.B) {
			pg, taskID, sectionID := sectionContextFixture(b, candidates)
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				sc, err := BuildSectionContext(ctx, pg, db.DefaultTenant, taskID, sectionID)
				if err != nil {
					b.Fatal(err)
				}
				if sc.Current == nil || sc.Current.RevNo != 1 || len(sc.Requirements) != 2 {
					b.Fatalf("wrong main revision or requirements: %+v", sc)
				}
			}
		})
	}
}

func BenchmarkR4NativeParse(b *testing.B) {
	blocks := make([]any, 250)
	for i := range blocks {
		blocks[i] = ingesttest.Para{Text: fmt.Sprintf("第%d项 设备巡检、故障响应与验收记录。", i+1)}
	}
	pages := make([]ingesttest.Page, 20)
	for i := range pages {
		for row := range 25 {
			pages[i].Texts = append(pages[i].Texts, ingesttest.Text{
				X: 45, Y: 790 - float64(row)*24, Size: 12,
				S: fmt.Sprintf("第%d页第%d项 设备巡检、故障响应与验收记录。", i+1, row+1),
			})
		}
	}
	files := []ingest.File{
		{Name: "synthetic.docx", Data: ingesttest.BuildDOCX("合成招标工作量", blocks...)},
		{Name: "synthetic.pdf", Data: ingesttest.BuildPDF("合成招标工作量", pages...)},
	}
	for _, file := range files {
		b.Run(file.Name, func(b *testing.B) {
			b.SetBytes(int64(len(file.Data)))
			b.ReportAllocs()
			for b.Loop() {
				parsed, err := ingest.Parse(context.Background(), file, ingest.ParseOptions{})
				if err != nil || parsed.Text == "" {
					b.Fatalf("native parse returned no text: %v", err)
				}
			}
		})
	}
}

func BenchmarkR4TenderChunkPagination(b *testing.B) {
	pg := dbtest.Open(b)
	ctx := context.Background()
	task, err := pg.CreateTask(ctx, db.CreateTaskInput{Name: dbtest.Uniq("r4-page"), Goal: "招标原文分页基准"})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := pg.DeleteTask(ctx, task.ID); err != nil {
			b.Error(err)
		}
	})
	body := strings.Repeat("设备巡检、故障响应与验收记录。\n", 1500)
	doc := &db.TenderDocument{TaskID: task.ID, TenderNo: "R4-PAGE", Title: "合成招标原文", RawText: body, PageCount: 8}
	docID, _, err := pg.InsertTenderDocument(ctx, doc, []db.DocumentChunk{{Seq: 0, PageNo: 1, PageStart: 1, PageEnd: 8, Content: body}})
	if err != nil {
		b.Fatal(err)
	}
	chunks, err := pg.DocumentOutline(ctx, task.ID, docID, 1)
	if err != nil || len(chunks) != 1 {
		b.Fatalf("load benchmark chunk: %v", err)
	}
	read := New(pg, task.ID, nil, nil).readChunkTool()
	first := json.RawMessage(fmt.Sprintf(`{"chunk_id":%d,"context_budget":4096}`, chunks[0].ID))
	b.ReportAllocs()
	for b.Loop() {
		res, err := read.Call(ctx, first, nil)
		if err != nil || res.IsError {
			b.Fatalf("first page: %v %s", err, res.Content)
		}
		var page struct {
			Next int `json:"next_text_offset"`
		}
		if err := json.Unmarshal([]byte(res.Content), &page); err != nil || page.Next <= 0 {
			b.Fatalf("bad continuation offset: %v %d", err, page.Next)
		}
		second := json.RawMessage(fmt.Sprintf(`{"chunk_id":%d,"text_offset":%d,"context_budget":4096}`, chunks[0].ID, page.Next))
		if res, err = read.Call(ctx, second, nil); err != nil || res.IsError {
			b.Fatalf("second page: %v %s", err, res.Content)
		}
	}
}

func BenchmarkR4LexicalRetrieval(b *testing.B) {
	pg := dbtest.Open(b)
	ctx := context.Background()
	tenant := dbtest.Uniq("r4-retrieval")
	chunks := make([]db.KnowledgeChunk, 256)
	for i := range chunks {
		chunks[i] = db.KnowledgeChunk{Seq: i, SectionTitle: "实施方案",
			Content: fmt.Sprintf("设备巡检条目 %d。故障响应与验收记录。", i+1)}
	}
	doc := &db.KnowledgeDocument{TenantID: tenant, Category: db.KnowledgeHistoricalBid, Title: "合成实施文件", RawText: "设备巡检与验收"}
	_, _, err := pg.InsertKnowledgeDocument(ctx, doc, chunks, db.DefaultEmbedder())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		for _, table := range []string{"knowledge_documents", "knowledge_catalogs"} {
			if _, err := pg.ExecContext(ctx, "DELETE FROM "+table+" WHERE tenant_id = $1", tenant); err != nil {
				b.Error(err)
			}
		}
	})
	query := db.KnowledgeQuery{TenantID: tenant, Text: "设备巡检 故障响应", Limit: 20}
	b.ReportAllocs()
	for b.Loop() {
		hits, err := pg.RetrievalCandidates(ctx, query, 0, nil)
		if err != nil || len(hits) != query.Limit {
			b.Fatalf("lexical query returned %d hits: %v", len(hits), err)
		}
	}
}

func BenchmarkR4ModelAdapter(b *testing.B) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, "request incomplete", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"合成草稿。"},"finish_reason":"stop"}],"usage":{"prompt_tokens":6000,"completion_tokens":4}}`)
	}))
	b.Cleanup(endpoint.Close)
	provider := openai.New(openai.Config{BaseURL: endpoint.URL, Model: "synthetic-test", HTTPClient: endpoint.Client()})
	req := llm.CompletionRequest{Messages: []llm.Message{llm.UserText(strings.Repeat("设备巡检、故障响应与验收记录。\n", 500))}}
	b.ReportAllocs()
	for b.Loop() {
		message, _, usage, err := provider.Complete(context.Background(), req)
		if err != nil || message.Text() != "合成草稿。" || usage.InputTokens != 6000 {
			b.Fatalf("model adapter output: %v, usage=%+v, message=%+v", err, usage, message)
		}
	}
}
