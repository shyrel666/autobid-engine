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
	EvidenceID   int64                `json:"evidence_id,omitempty"`
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

func hitViews(hits []db.KnowledgeHit, evidenceIDs ...int64) []kbHitView {
	out := make([]kbHitView, 0, len(hits))
	for i, h := range hits {
		var evid int64
		if i < len(evidenceIDs) {
			evid = evidenceIDs[i]
		}
		out = append(out, kbHitView{EvidenceID: evid, Citation: h.Citation, Cite: citationText(h.Citation), Score: h.Score, FTSRank: h.FTSRank, VectorScore: h.VectorScore,
			KeywordScore: h.KeywordScore, MatchedTerms: h.MatchedTerms, Tags: h.Tags, TableCount: h.Chunk.TableCount,
			Mandatory: h.Chunk.IsMandatory, Snippet: kbSnippet(h.Chunk.Content, h.MatchedTerms)})
	}
	return out
}

func kbResult(hits []db.KnowledgeHit, meta ...retrieval.Result) tool.Result {
	var evIDs []int64
	if len(meta) > 0 {
		evIDs = meta[0].EvidenceIDs
	}
	out := map[string]any{"total": len(hits), "hits": hitViews(hits, evIDs...),
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
		out["retrieval_mode"] = meta[0].Diagnostics.Mode
		out["index_state"] = meta[0].Diagnostics.IndexState
		if meta[0].Diagnostics.Mode == "lexical_only" {
			out["degraded_note"] = "semantic channel unavailable; only full-text and keyword recall ran. No hits does not prove the knowledge base lacks the content."
		}
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
		Description: "检索企业方案知识库中的历史中标标书（historical_bid）与标准技术方案（standard_solution），查找匹配技术需求关键词的可复用方案段落，支持按行业标签过滤。" +
			"采用混合检索排序（PostgreSQL 中文分词全文检索 + 向量语义相似度 + 关键词命中）。每条结果均附带完整来源引用 citation（含文档标题、类别、章节路径、起止页码与切片序号 seq）。",
		Schema: obj(map[string]any{
			"query":      str("技术需求检索关键词，多个词以空格分隔（如 \"视频监控 云边协同 等保三级\"）"),
			"tags":       strs("行业或业务标签过滤列表，命中任一标签即符合（如 [\"智慧园区\", \"政务\"]）"),
			"categories": enumStrs("限定知识文档类别（默认同时检索历史标书与标准技术方案）", cats),
			"limit":      integer("返回结果条数上限，默认 8，最大 30"),
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
		Description: "按产品型号名称与技术指标检索企业方案知识库中的产品规格书（product_spec）与技术白皮书（whitepaper）。" +
			"指定 product 时要求产品名在文档标题、元数据、章节或正文中精确出现；specs 为技术参数关键词，含结构化参数表的切片优先排序。" +
			"每条结果附带含页码范围的来源引用；引用具体指标数值前须先调用 kb_read_chunk 读取完整表格 JSON。",
		Schema: obj(map[string]any{
			"product":    str("必须精确出现的产品或设备型号名称（如 \"DS-2CD7A47\" 或 \"统一视频平台\"）"),
			"specs":      strs("技术指标或参数关键词列表（如 [\"分辨率\", \"400万像素\", \"H.265\"]）"),
			"tags":       strs("行业或产品线标签过滤列表，命中任一标签即可"),
			"categories": enumStrs("限定知识文档类别（默认同时检索产品规格书与技术白皮书）", cats),
			"limit":      integer("返回结果条数上限，默认 8，最大 30"),
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
		Description: "按知识库文档 ID（doc_id）与切片序号（seq，来自 kb_search_* 返回的 citation）读取知识库切片完整正文及结构化表格，" +
			"可选附带前后各最多 2 个相邻切片。超长正文与大表支持按偏移量分页读取并保留原始单元格坐标，同时返回复用时必须写入 source_ref 的标准 citation。",
		Schema: obj(map[string]any{
			"doc_id":         integer("知识库文档 ID（来自 citation.doc_id）"),
			"seq":            integer("文档内切片序号（来自 citation.seq）"),
			"before":         integer("附带读取前序相邻切片数量（0~2）"),
			"after":          integer("附带读取后续相邻切片数量（0~2）"),
			"text_offset":    integer("正文 UTF-8 字节分页偏移量（翻页时填入上一页返回的 next_text_offset）"),
			"table_index":    integer("大表分页时指定的表格索引号，默认 0"),
			"row_offset":     integer("大表分页时的起始行偏移量（自动保留表头行）"),
			"context_budget": integer("单次读取的正文字节预算上限，默认 16000，最大 32000"),
			"expected_hash":  str("上一页返回的 content_hash（用于校验翻页期间源文档未被替换）"),
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
