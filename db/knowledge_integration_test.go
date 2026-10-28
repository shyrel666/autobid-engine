package db

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func kbChunk(seq int, code, title, path, content string, pages [2]int, tables string) KnowledgeChunk {
	return KnowledgeChunk{Seq: seq, Level: 3, SectionCode: code, SectionTitle: title, SectionPath: path, PageStart: pages[0], PageEnd: pages[1],
		Content: content, Tables: json.RawMessage(tables)}
}

func TestKnowledgeTokensAndQuery(t *testing.T) {
	toks := KnowledgeTokens("视频监控 H.265 CPU≥32核")
	want := []string{"视", "视频", "频", "频监", "监", "监控", "控", "h", "265", "cpu", "32", "核"}
	if strings.Join(toks, " ") != strings.Join(want, " ") {
		t.Fatalf("tokens = %v", toks)
	}
	if q := KnowledgeTSQuery(QueryTerms("视频监控，H.265 云")); q != "('视频' & '频监' & '监控') | ('h':* & '265':*) | '云'" {
		t.Fatalf("tsquery = %s", q)
	}
	if KnowledgeTSQuery(QueryTerms("、，。")) != "" {
		t.Fatal("punctuation-only query must compile to empty")
	}
	e := DefaultEmbedder()
	v, _ := e.Embed(context.Background(), []string{"视频监控平台", "监控视频平台", "财务报表审计"})
	cos := func(a, b []float64) float64 {
		s := 0.0
		for i := range a {
			s += a[i] * b[i]
		}
		return s
	}
	if near, far := cos(v[0], v[1]), cos(v[0], v[2]); near < 0.5 || far > 0.2 {
		t.Fatalf("hash embedding similarity near=%.3f far=%.3f", near, far)
	}
}

func TestKnowledgeStoreDedupSearchAndCitations(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	tag := uniq("行业")
	tenant := uniq("tenant")
	emb := DefaultEmbedder()

	bid := &KnowledgeDocument{TenantID: tenant, Category: KnowledgeHistoricalBid, Title: "某市智慧园区安防项目技术标", Tags: []string{tag, "安防", tag},
		RawText: uniq("bid-raw"), FileName: "historical.docx", PageCount: 12, CreatedBy: "alice"}
	bidChunks := []KnowledgeChunk{
		kbChunk(0, "第一章", "总体方案", "第一章 总体方案", "本项目采用云边协同架构，前端视频监控统一接入视频云平台。", [2]int{1, 2}, `[]`),
		kbChunk(1, "1.2", "系统架构设计", "第一章 总体方案 / 1.2 系统架构设计", "平台满足等保三级要求，支持H.265编码与国标GB/T28181接入。", [2]int{3, 4},
			`[{"rows":2,"cols":2,"grid":[["指标","要求"],["并发路数","1000"]]}]`),
		kbChunk(2, "第二章", "实施计划", "第二章 实施计划", "项目分两期实施，总工期90天。", [2]int{5, 5}, `[]`),
	}
	id, created, err := d.InsertKnowledgeDocument(ctx, bid, bidChunks, emb)
	if err != nil || !created {
		t.Fatalf("insert: %v created=%v", err, created)
	}
	again, created, err := d.InsertKnowledgeDocument(ctx, bid, bidChunks, emb)
	if err != nil || created || again != id {
		t.Fatalf("dedupe: id=%d again=%d created=%v err=%v", id, again, created, err)
	}
	spec := &KnowledgeDocument{TenantID: tenant, Category: KnowledgeProductSpec, Title: "星光级网络摄像机规格书", Tags: []string{"安防"},
		RawText: uniq("spec-raw"), Extra: json.RawMessage(`{"product":"XG-4000","vendor":"测试厂商"}`)}
	specID, _, err := d.InsertKnowledgeDocument(ctx, spec, []KnowledgeChunk{
		kbChunk(0, "1", "技术参数", "1 技术参数", "分辨率400万像素，最低照度0.0005Lux，支持H.265。", [2]int{2, 3}, `[{"rows":3,"cols":2}]`),
	}, emb)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.InsertKnowledgeDocument(ctx, &KnowledgeDocument{TenantID: tenant, Category: "bogus", Title: "x", RawText: "x"}, nil, nil); !errors.Is(err, ErrInvalidKnowledge) {
		t.Fatalf("invalid category: %v", err)
	}

	docs, err := d.ListKnowledgeDocuments(ctx, KnowledgeDocFilter{TenantID: tenant, Tags: []string{tag}})
	if err != nil || len(docs) != 1 || docs[0].ID != id || docs[0].ChunkCount != 3 || docs[0].TableCount != 1 ||
		len(docs[0].Tags) != 2 || docs[0].CreatedBy != "alice" || docs[0].UpdatedBy != "alice" {
		t.Fatalf("list by tag: %v %+v", err, docs)
	}
	if docs, _ := d.ListKnowledgeDocuments(ctx, KnowledgeDocFilter{TenantID: tenant, Category: KnowledgeProductSpec}); len(docs) != 1 || docs[0].ID != specID {
		t.Fatalf("list by category: %+v", docs)
	}
	if docs, _ := d.ListKnowledgeDocuments(ctx, KnowledgeDocFilter{TenantID: uniq("other")}); len(docs) != 0 {
		t.Fatalf("tenant isolation (list): %+v", docs)
	}

	hits, err := d.SearchKnowledge(ctx, KnowledgeQuery{TenantID: tenant, Text: "等保三级 H.265 国标"}, emb)
	if err != nil || len(hits) < 2 {
		t.Fatalf("search: %v %+v", err, hits)
	}
	top := hits[0]
	if top.Citation.DocID != id || top.Citation.Seq != 1 || top.Citation.DocTitle != bid.Title || top.Citation.Category != KnowledgeHistoricalBid ||
		top.Citation.SectionCode != "1.2" || top.Citation.SectionPath != "第一章 总体方案 / 1.2 系统架构设计" ||
		top.Citation.PageStart != 3 || top.Citation.PageEnd != 4 || top.Citation.FileName != "historical.docx" || top.FTSRank <= 0 || top.Chunk.TableCount != 1 {
		t.Fatalf("top hit: %+v", top)
	}
	for _, h := range hits {
		if h.Citation.DocTitle == "" || h.Citation.Category == "" || h.Citation.PageStart <= 0 || h.Citation.PageEnd < h.Citation.PageStart || h.Chunk.Content == "" {
			t.Fatalf("incomplete citation: %+v", h)
		}
	}

	// 标签过滤、产品名必含与表格加分。
	hits, err = d.SearchKnowledge(ctx, KnowledgeQuery{TenantID: tenant, Text: "H.265", MustTerms: []string{"XG-4000"}, PreferTables: true}, emb)
	if err != nil || len(hits) != 1 || hits[0].Citation.DocID != specID {
		t.Fatalf("must term via extra: %v %+v", err, hits)
	}
	hits, _ = d.SearchKnowledge(ctx, KnowledgeQuery{TenantID: tenant, Text: "H.265", Categories: []string{KnowledgeHistoricalBid}, Tags: []string{tag}}, emb)
	if len(hits) != 1 || hits[0].Citation.DocID != id {
		t.Fatalf("category + tag filter: %+v", hits)
	}
	if hits, _ := d.SearchKnowledge(ctx, KnowledgeQuery{TenantID: uniq("other"), Text: "等保三级"}, emb); len(hits) != 0 {
		t.Fatalf("tenant isolation (search): %+v", hits)
	}
	if _, err := d.SearchKnowledge(ctx, KnowledgeQuery{TenantID: tenant, Text: "  "}, emb); !errors.Is(err, ErrInvalidKnowledge) {
		t.Fatalf("empty query: %v", err)
	}
	// 词序不同、全文 AND 不命中的检索靠向量召回。
	hits, err = d.SearchKnowledge(ctx, KnowledgeQuery{TenantID: tenant, Text: "实施项目两期"}, emb)
	if err != nil || len(hits) == 0 || hits[0].Citation.Seq != 2 || hits[0].VectorScore < minVectorRecall {
		t.Fatalf("vector recall: %v %+v", err, hits)
	}

	doc, target, window, err := d.KnowledgeChunkWindow(ctx, tenant, id, 1, 1, 1)
	if err != nil || doc.ID != id || target.Seq != 1 || len(window) != 3 || !strings.Contains(string(target.Tables), "并发路数") {
		t.Fatalf("window: %v %+v %+v", err, target, window)
	}
	if _, _, _, err := d.KnowledgeChunkWindow(ctx, uniq("other"), id, 1, 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant isolation (read): %v", err)
	}
	if err := d.DeleteKnowledgeDocument(ctx, uniq("other"), id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant isolation (delete): %v", err)
	}
	if err := d.DeleteKnowledgeDocument(ctx, tenant, id); err != nil {
		t.Fatal(err)
	}
	if chunks, _ := d.KnowledgeChunks(ctx, tenant, id, false); len(chunks) != 0 {
		t.Fatalf("chunks must cascade: %+v", chunks)
	}
	if err := d.DeleteKnowledgeDocument(ctx, tenant, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}
