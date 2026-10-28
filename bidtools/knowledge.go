package bidtools

// 企业方案知识库检索工具（经 knowledge-reuse 技能解锁）：所有结果强制附带 citation 来源引用
// （文档标题、类别、章节号/路径、页码范围），Worker 引用知识库内容时必须原样写入 source_ref。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/tool"
	"github.com/autobid/autobid-engine/retrieval"
)

const (
	defaultKBHits = 8
	maxKBHits     = 30
	maxKBWindow   = 2
	kbSnippetLen  = 80
)

func enumStrs(desc string, values []string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": values}, "description": desc}
}

type kbHitView struct {
	Citation     db.KnowledgeCitation `json:"citation"`
	Cite         string               `json:"cite"`
	Score        float64              `json:"score"`
	FTSRank      float64              `json:"fts_rank"`
	VectorScore  float64              `json:"vector_score"`
	KeywordScore float64              `json:"keyword_score"`
	MatchedTerms []string             `json:"matched_terms"`
	Tags         []string             `json:"tags,omitempty"`
	TableCount   int                  `json:"table_count,omitempty"`
	Mandatory    bool                 `json:"mandatory,omitempty"`
	Snippet      string               `json:"snippet"`
}

// kbSnippet 截取首个命中词附近的正文。
func kbSnippet(content string, terms []string) string {
	lower := strings.ToLower(content)
	for _, t := range terms {
		if i := strings.Index(lower, strings.ToLower(t)); i >= 0 {
			return snippetRadiusN(content, []int{i, i + len(t)}, kbSnippetLen)
		}
	}
	return snippetRadiusN(content, nil, kbSnippetLen)
}

func snippetRadiusN(text string, loc []int, radius int) string {
	if loc == nil {
		return strings.TrimSpace(truncate(strings.ReplaceAll(text, "\n", " "), 2*radius))
	}
	r := []rune(text)
	start := len([]rune(text[:loc[0]]))
	end := start + len([]rune(text[loc[0]:loc[1]]))
	from, to := max(start-radius, 0), min(end+radius, len(r))
	s := strings.ReplaceAll(string(r[from:to]), "\n", " ")
	if from > 0 {
		s = "…" + s
	}
	if to < len(r) {
		s += "…"
	}
	return strings.TrimSpace(s)
}

func hitViews(hits []db.KnowledgeHit) []kbHitView {
	out := make([]kbHitView, 0, len(hits))
	for _, h := range hits {
		out = append(out, kbHitView{Citation: h.Citation, Cite: citationText(h.Citation), Score: h.Score, FTSRank: h.FTSRank, VectorScore: h.VectorScore,
			KeywordScore: h.KeywordScore, MatchedTerms: h.MatchedTerms, Tags: h.Tags, TableCount: h.Chunk.TableCount,
			Mandatory: h.Chunk.IsMandatory, Snippet: kbSnippet(h.Chunk.Content, h.MatchedTerms)})
	}
	return out
}

func kbResult(hits []db.KnowledgeHit, meta ...retrieval.Result) tool.Result {
	out := map[string]any{"total": len(hits), "hits": hitViews(hits),
		"citation_rule": "quote knowledge only with its citation {doc_title, category, section_code/section_path, page_start-page_end}; " +
			"call kb_read_chunk(doc_id, seq) for full text and table JSON before reusing it"}
	if len(hits) == 0 {
		out["note"] = "no knowledge base matches; try fewer or broader keywords, or drop tag filters"
	}
	if len(meta) > 0 {
		out["run_id"] = meta[0].RunID
		out["evidence_ids"] = meta[0].EvidenceIDs
		out["degraded"] = meta[0].Degraded
		out["strategy"] = meta[0].Strategy
		out["verification"] = "unverified: retrieval relevance is not evidence of compliance"
	}
	return tool.JSON(out)
}

func (k *Kit) searchKnowledge(ctx context.Context, q db.KnowledgeQuery) (retrieval.Result, error) {
	if k.retrieval != nil {
		return k.retrieval.Search(ctx, q, k.taskID, "worker")
	}
	hits, err := k.pg.SearchKnowledge(ctx, q, k.emb)
	return retrieval.Result{Hits: hits, Total: len(hits), Strategy: "legacy"}, err
}

// kbErr 把参数错误转为工具错误结果，其余错误上抛。
func kbErr(err error) (tool.Result, error) {
	if errors.Is(err, db.ErrInvalidKnowledge) {
		return tool.Errorf("%v", err), nil
	}
	return tool.Result{}, err
}

func (k *Kit) kbSearchProposalsTool() tool.CoreTool {
	cats := []string{db.KnowledgeHistoricalBid, db.KnowledgeStandardSolution}
	return readTool(tool.Spec{
		Name: ToolKBSearchProposals,
		Description: "Search the enterprise knowledge base of historical bids (historical_bid) and standard technical solutions (standard_solution) " +
			"for reusable proposal passages matching technical requirement keywords, optionally filtered by industry tags. " +
			"Hybrid ranking: PostgreSQL full-text (CJK bigram) + vector similarity + keyword hits. Every hit carries a citation " +
			"{doc_id, doc_title, category, section_code, section_path, page_start, page_end, seq}.",
		Schema: obj(map[string]any{
			"query":      str("technical requirement keywords, space separated (e.g. \"视频监控 云边协同 等保三级\")"),
			"tags":       strs("industry tags; a document matching any tag qualifies (e.g. [\"智慧园区\",\"政务\"])"),
			"categories": enumStrs("restrict to these categories; default both", cats),
			"limit":      integer("max hits, default 8, max 30"),
		}),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			Query      string   `json:"query"`
			Tags       []string `json:"tags"`
			Categories []string `json:"categories"`
			Limit      int      `json:"limit"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		if strings.TrimSpace(req.Query) == "" {
			return tool.Errorf("query is required"), nil
		}
		if len(req.Categories) == 0 {
			req.Categories = cats
		}
		for _, c := range req.Categories {
			if c != db.KnowledgeHistoricalBid && c != db.KnowledgeStandardSolution {
				return tool.Errorf("category %q is not a proposal category; use kb_search_products for product_spec / whitepaper", c), nil
			}
		}
		result, err := k.searchKnowledge(ctx, db.KnowledgeQuery{TenantID: k.tenant, Text: req.Query, Categories: req.Categories,
			Tags: req.Tags, Limit: clampLimit(req.Limit)})
		if err != nil {
			return kbErr(err)
		}
		return kbResult(result.Hits, result), nil
	})
}

func clampLimit(n int) int {
	if n <= 0 {
		return defaultKBHits
	}
	return min(n, maxKBHits)
}

func (k *Kit) kbSearchProductsTool() tool.CoreTool {
	cats := []string{db.KnowledgeProductSpec, db.KnowledgeWhitepaper}
	return readTool(tool.Spec{
		Name: ToolKBSearchProducts,
		Description: "Search product specification sheets (product_spec) and technical whitepapers (whitepaper) by product name and technical indicators. " +
			"product (if given) must literally appear in the document title, metadata, section or text; specs are ranked keywords. " +
			"Chunks containing structured parameter tables rank higher. Every hit carries a citation with page range; " +
			"call kb_read_chunk to get the full table JSON before quoting indicator values.",
		Schema: obj(map[string]any{
			"product":    str("product / model name that must appear (e.g. \"DS-2CD7A47\" or \"统一视频平台\")"),
			"specs":      strs("technical indicators or parameter keywords (e.g. [\"分辨率\",\"400万像素\",\"H.265\"])"),
			"tags":       strs("industry / product line tags; any match qualifies"),
			"categories": enumStrs("restrict to these categories; default both", cats),
			"limit":      integer("max hits, default 8, max 30"),
		}),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			Product    string   `json:"product"`
			Specs      []string `json:"specs"`
			Tags       []string `json:"tags"`
			Categories []string `json:"categories"`
			Limit      int      `json:"limit"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		product := strings.TrimSpace(req.Product)
		if product == "" && len(req.Specs) == 0 {
			return tool.Errorf("product or specs is required"), nil
		}
		if len(req.Categories) == 0 {
			req.Categories = cats
		}
		for _, c := range req.Categories {
			if c != db.KnowledgeProductSpec && c != db.KnowledgeWhitepaper {
				return tool.Errorf("category %q is not a product category; use kb_search_proposals for historical_bid / standard_solution", c), nil
			}
		}
		q := db.KnowledgeQuery{TenantID: k.tenant, Text: strings.Join(req.Specs, " "), Categories: req.Categories, Tags: req.Tags,
			Limit: clampLimit(req.Limit), PreferTables: true}
		if product != "" {
			q.MustTerms = []string{product}
		}
		result, err := k.searchKnowledge(ctx, q)
		if err != nil {
			return kbErr(err)
		}
		return kbResult(result.Hits, result), nil
	})
}

func (k *Kit) kbReadChunkTool() tool.CoreTool {
	return readTool(tool.Spec{
		Name: ToolKBReadChunk,
		Description: "Read one knowledge base chunk by document id and chunk sequence number (seq from a kb_search_* citation), " +
			"with up to 2 neighbouring chunks before/after it. Large text/tables are paginated with explicit offsets and original cell coordinates. " +
			"and the citation to copy into source_ref when reusing the content.",
		Schema: obj(map[string]any{
			"doc_id":         integer("knowledge document id (citation.doc_id)"),
			"seq":            integer("chunk sequence number within the document (citation.seq)"),
			"before":         integer("neighbouring chunks before, 0-2"),
			"after":          integer("neighbouring chunks after, 0-2"),
			"text_offset":    integer("UTF-8 byte offset; use next_text_offset from previous page"),
			"table_index":    integer("table index for large table pagination; default 0"),
			"row_offset":     integer("original row offset; repeated headers retained"),
			"context_budget": integer("conservative text byte/token bound, default 16000, max 32000"),
			"expected_hash":  str("content_hash from previous page; reject changed source"),
		}, "doc_id", "seq"),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			DocID        int64  `json:"doc_id"`
			Seq          int    `json:"seq"`
			Before       int    `json:"before"`
			After        int    `json:"after"`
			TextOffset   int    `json:"text_offset"`
			TableIndex   int    `json:"table_index"`
			RowOffset    int    `json:"row_offset"`
			Budget       int    `json:"context_budget"`
			ExpectedHash string `json:"expected_hash"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		doc, target, window, err := k.pg.KnowledgeChunkWindow(ctx, k.tenant, req.DocID, req.Seq,
			min(max(req.Before, 0), maxKBWindow), min(max(req.After, 0), maxKBWindow))
		if errors.Is(err, db.ErrNotFound) {
			return tool.Errorf("knowledge chunk doc_id=%d seq=%d not found", req.DocID, req.Seq), nil
		}
		if err != nil {
			return tool.Result{}, err
		}
		hash := db.ChunkFingerprint(doc, target)
		if req.ExpectedHash != "" && req.ExpectedHash != hash {
			return tool.Errorf("source changed between pages; retrieve a new evidence snapshot"), nil
		}
		budget := req.Budget
		if budget <= 0 {
			budget = 16000
		}
		budget = min(max(budget, 2048), 32000)
		text, next, err := retrieval.TextPage(target.Content, req.TextOffset, budget/2)
		if err != nil {
			return tool.Errorf("%v", err), nil
		}
		paged := len(target.Tables) > budget/4 || req.RowOffset > 0 || req.TableIndex > 0
		var tablePage *retrieval.TablePage
		tables := json.RawMessage(nonNullJSON(target.Tables, "[]"))
		if paged {
			p, e := retrieval.PageTable(tables, req.TableIndex, req.RowOffset, 100, budget/4)
			if e != nil {
				return tool.Errorf("%v", e), nil
			}
			tablePage = &p
			tables = json.RawMessage(`[]`)
		}
		type neighbour struct {
			Seq      int                  `json:"seq"`
			Content  string               `json:"content"`
			Citation db.KnowledgeCitation `json:"citation"`
		}
		var before, after []neighbour
		for _, c := range window {
			excerpt, _, _ := retrieval.TextPage(c.Content, 0, max(budget/32, 256))
			n := neighbour{c.Seq, excerpt, c.Cite(doc)}
			switch {
			case c.Seq < target.Seq:
				before = append(before, n)
			case c.Seq > target.Seq:
				after = append(after, n)
			}
		}
		out := map[string]any{
			"content_hash": hash, "next_text_offset": next, "text_complete": next == len(target.Content), "tables_complete": !paged, "table_page": tablePage, "neighbour_context": "bounded excerpts; read each neighbour separately for full text",
			"document": map[string]any{"doc_id": doc.ID, "title": doc.Title, "category": doc.Category, "tags": doc.Tags,
				"file_name": doc.FileName, "page_count": doc.PageCount},
			"chunk": map[string]any{"seq": target.Seq, "level": target.Level, "section_code": target.SectionCode,
				"section_title": target.SectionTitle, "section_path": target.SectionPath, "page_start": target.PageStart,
				"page_end": target.PageEnd, "mandatory": target.IsMandatory, "content": text,
				"tables": tables},
			"citation": target.Cite(doc),
			"cite":     citationText(target.Cite(doc)),
		}
		if len(before) > 0 {
			out["before"] = before
		}
		if len(after) > 0 {
			out["after"] = after
		}
		raw, _ := json.Marshal(out)
		if len(raw) > budget {
			return tool.Errorf("context metadata exceeds budget; reduce neighbours or raise context_budget (max 32000)"), nil
		}
		return tool.JSON(out), nil
	})
}

func nonNullJSON(raw json.RawMessage, def string) string {
	if len(raw) == 0 || string(raw) == "null" {
		return def
	}
	return string(raw)
}

// citationText 把引用格式化为可直接写入标书或事实摘要的一行出处。
func citationText(c db.KnowledgeCitation) string {
	return fmt.Sprintf("《%s》(%s) %s 第%d-%d页", c.DocTitle, c.Category, strings.TrimSpace(c.SectionCode+" "+c.SectionTitle), c.PageStart, c.PageEnd)
}
