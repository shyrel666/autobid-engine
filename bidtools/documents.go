package bidtools

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/ingest"
	"github.com/autobid/autobid-engine/internal/tool"
	"github.com/autobid/autobid-engine/retrieval"
)

const (
	maxOutline      = 800
	maxSearchScan   = 20000
	maxReadWindow   = 3
	snippetRadius   = 60
	defaultMatches  = 20
	maxMatchesLimit = 100
)

// SourceRef 是原文出处的标准结构；Worker 把它原样写入 insert_assets / record_fact / report_finding。
type SourceRef struct {
	DocID        int64  `json:"doc_id"`
	ChunkID      int64  `json:"chunk_id"`
	SectionCode  string `json:"section_code"`
	SectionTitle string `json:"section_title,omitempty"`
	PageNo       int    `json:"page_no"`
	PageStart    int    `json:"page_start,omitempty"`
	PageEnd      int    `json:"page_end,omitempty"`
	// PageIsPhysical 为 false 时 page_no 只是兼容占位值，来源是表格工作表/区域。
	PageIsPhysical bool                  `json:"page_is_physical"`
	SourceLocator  *ingest.SourceLocator `json:"source_locator,omitempty"`
	TenderNo       string                `json:"tender_no,omitempty"`
	FileName       string                `json:"file_name,omitempty"`
	LotNo          string                `json:"lot_no,omitempty"`
	DocTitle       string                `json:"doc_title,omitempty"`
	OriginalFileID *int64                `json:"original_file_id,omitempty"`
	SHA256         string                `json:"sha256,omitempty"`
}

func refOf(c *db.DocumentChunk, doc *db.TenderDocument) SourceRef {
	r := SourceRef{DocID: c.DocID, ChunkID: c.ID, SectionCode: c.SectionCode, SectionTitle: c.SectionTitle, PageNo: c.PageNo,
		PageStart: c.PageStart, PageEnd: c.PageEnd, PageIsPhysical: c.PageIsPhysical}
	if len(c.BBoxMeta) > 0 {
		var meta struct {
			SourceLocator *ingest.SourceLocator `json:"source_locator"`
		}
		if json.Unmarshal(c.BBoxMeta, &meta) == nil {
			r.SourceLocator = meta.SourceLocator
		}
	}
	if doc != nil {
		r.TenderNo, r.LotNo, r.DocTitle, r.FileName = doc.TenderNo, doc.LotNo, doc.Title, doc.FileName
		r.OriginalFileID, r.SHA256 = doc.OriginalFileID, doc.ContentHash
	}
	return r
}

func (k *Kit) documentOutlineTool() tool.CoreTool {
	return readTool(tool.Spec{
		Name: ToolDocumentOutline,
		Description: "列出当前投标项目下各招标文件（含更正公告、澄清、答疑）的章节目录大纲（返回 doc_id、chunk_id、section_code、section_title、page_no、page_end、level）。" +
			"mandatory=true 标记含 ★/*/废标/否决 等强制性条款的切片；tables>0 标记含结构化表格的切片（可通过 read_chunk 读取完整表格）。用于快速定位需要精读的章节切片。",
		Schema: obj(map[string]any{
			"doc_id":    integer("仅返回指定招标文档 ID 的章节目录（不填则返回全部文档）"),
			"max_level": integer("限制返回的最大标题层级（1 为一级章，2 为二级节，3+ 为编号条款；不填则返回全部层级）"),
		}),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			DocID    int64 `json:"doc_id"`
			MaxLevel int   `json:"max_level"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		docs, err := k.pg.ListTenderDocuments(ctx, k.taskID)
		if err != nil {
			return tool.Result{}, err
		}
		if len(docs) == 0 {
			return tool.Text("no tender documents have been uploaded for this task"), nil
		}
		chunks, err := k.pg.DocumentOutline(ctx, k.taskID, req.DocID, maxSearchScan)
		if err != nil {
			return tool.Result{}, err
		}
		type entry struct {
			DocID          int64  `json:"doc_id"`
			ChunkID        int64  `json:"chunk_id"`
			Level          int    `json:"level"`
			SectionCode    string `json:"section_code"`
			SectionTitle   string `json:"section_title"`
			PageNo         int    `json:"page_no"`
			PageEnd        int    `json:"page_end,omitempty"`
			PageIsPhysical bool   `json:"page_is_physical"`
			Mandatory      bool   `json:"mandatory,omitempty"`
			Tables         int    `json:"tables,omitempty"`
		}
		entries := []entry{}
		omitted := 0
		for _, c := range chunks {
			if req.MaxLevel > 0 && c.Level > req.MaxLevel {
				continue
			}
			if len(entries) >= maxOutline {
				omitted++
				continue
			}
			pageEnd := 0
			if c.PageEnd > c.PageNo {
				pageEnd = c.PageEnd
			}
			entries = append(entries, entry{c.DocID, c.ID, c.Level, c.SectionCode, truncate(c.SectionTitle, 60), c.PageNo, pageEnd, c.PageIsPhysical, c.IsMandatory, c.TableCount})
		}
		type docRow struct {
			DocID    int64  `json:"doc_id"`
			Title    string `json:"title"`
			TenderNo string `json:"tender_no"`
			LotNo    string `json:"lot_no,omitempty"`
			FileName string `json:"file_name,omitempty"`
			Pages    int    `json:"pages,omitempty"`
			Chunks   int    `json:"chunks"`
		}
		docRows := make([]docRow, 0, len(docs))
		for _, d := range docs {
			if req.DocID == 0 || d.ID == req.DocID {
				docRows = append(docRows, docRow{d.ID, d.Title, d.TenderNo, d.LotNo, d.FileName, d.PageCount, d.ChunkCount})
			}
		}
		out := map[string]any{"documents": docRows, "outline": entries}
		if omitted > 0 {
			out["omitted"] = omitted
			out["note"] = "outline truncated; pass doc_id or max_level to narrow it"
		}
		return tool.JSON(out), nil
	})
}

// chunkMatch 是一条检索命中。
type chunkMatch struct {
	ChunkID        int64  `json:"chunk_id"`
	DocID          int64  `json:"doc_id"`
	SectionCode    string `json:"section_code"`
	SectionTitle   string `json:"section_title"`
	PageNo         int    `json:"page_no"`
	PageIsPhysical bool   `json:"page_is_physical"`
	Mandatory      bool   `json:"mandatory,omitempty"`
	Snippet        string `json:"snippet"`
}

// compileQuery 把查询编译为正则：regex 模式原样编译（不区分大小写）；
// 关键词模式按空白切分，每个词都必须出现。返回的第一个正则用于定位摘要。
func compileQuery(query string, regex bool) ([]*regexp.Regexp, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if regex {
		re, err := regexp.Compile("(?i)" + query)
		if err != nil {
			return nil, err
		}
		return []*regexp.Regexp{re}, nil
	}
	var res []*regexp.Regexp
	for _, term := range strings.Fields(query) {
		res = append(res, regexp.MustCompile("(?i)"+regexp.QuoteMeta(term)))
	}
	return res, nil
}

// matchChunks 在段落中检索；mandatoryOnly 只保留强制性条款（切分时标记或正文含强制标记）。
func matchChunks(chunks []*db.DocumentChunk, res []*regexp.Regexp, mandatoryOnly bool, limit int) ([]chunkMatch, int) {
	matches := []chunkMatch{}
	total := 0
	for _, c := range chunks {
		text := c.SectionTitle + "\n" + c.Content
		loc := []int(nil)
		ok := true
		for i, re := range res {
			l := re.FindStringIndex(text)
			if l == nil {
				ok = false
				break
			}
			if i == 0 {
				loc = l
			}
		}
		if !ok {
			continue
		}
		mandatoryLoc := ingest.MandatoryPattern.FindStringIndex(c.Content)
		mandatory := c.IsMandatory || mandatoryLoc != nil
		if mandatoryOnly && !mandatory {
			continue
		}
		total++
		if len(matches) >= limit {
			continue
		}
		if loc == nil && mandatoryLoc != nil {
			off := len(c.SectionTitle) + 1
			loc = []int{mandatoryLoc[0] + off, mandatoryLoc[1] + off}
		}
		matches = append(matches, chunkMatch{c.ID, c.DocID, c.SectionCode, c.SectionTitle, c.PageNo, c.PageIsPhysical, mandatory, snippet(text, loc)})
	}
	return matches, total
}

// snippet 截取命中位置前后各 snippetRadius 个字符；loc 为 nil 时取开头。
func snippet(text string, loc []int) string {
	if loc == nil {
		return strings.TrimSpace(truncate(strings.ReplaceAll(text, "\n", " "), 2*snippetRadius))
	}
	startRune := utf8.RuneCountInString(text[:loc[0]])
	endRune := startRune + utf8.RuneCountInString(text[loc[0]:loc[1]])
	r := []rune(text)
	from, to := max(startRune-snippetRadius, 0), min(endRune+snippetRadius, len(r))
	s := strings.ReplaceAll(string(r[from:to]), "\n", " ")
	if from > 0 {
		s = "…" + s
	}
	if to < len(r) {
		s += "…"
	}
	return strings.TrimSpace(s)
}

func (k *Kit) searchDocumentTool() tool.CoreTool {
	return readTool(tool.Spec{
		Name: ToolSearchDocument,
		Description: "按关键词（空格分隔，大小写不敏感且须全部命中）或正则表达式全文检索当前项目的招标文件切片。" +
			"设置 mandatory_only=true 时仅保留含 ★/*/废标/否决/无效投标/实质性 的条款（此时 query 可为空）。" +
			"返回匹配片段与 chunk_id；需完整正文及结构化表格时请继续调用 read_chunk。",
		Schema: obj(map[string]any{
			"query":          str("检索关键词（多个词以空格分隔，须同时命中），或当 regex=true 时为正则表达式"),
			"regex":          boolean("是否将 query 作为 Go/RE2 正则表达式解析"),
			"mandatory_only": boolean("是否仅检索 ★/否决/废标/实质性响应等强制条款"),
			"doc_id":         integer("仅在指定招标文档 ID 内检索"),
			"limit":          integer("返回匹配条数上限，默认 20，最大 100"),
		}),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			Query         string `json:"query"`
			Regex         bool   `json:"regex"`
			MandatoryOnly bool   `json:"mandatory_only"`
			DocID         int64  `json:"doc_id"`
			Limit         int    `json:"limit"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		res, err := compileQuery(req.Query, req.Regex)
		if err != nil {
			return tool.Errorf("invalid regex: %v", err), nil
		}
		if len(res) == 0 && !req.MandatoryOnly {
			return tool.Errorf("query is required unless mandatory_only is true"), nil
		}
		if req.Limit <= 0 {
			req.Limit = defaultMatches
		}
		req.Limit = min(req.Limit, maxMatchesLimit)
		chunks, err := k.pg.TaskChunks(ctx, k.taskID, req.DocID, maxSearchScan)
		if err != nil {
			return tool.Result{}, err
		}
		matches, total := matchChunks(chunks, res, req.MandatoryOnly, req.Limit)
		out := map[string]any{"total": total, "matches": matches}
		if total > len(matches) {
			out["note"] = "more matches exist; refine the query or raise limit"
		}
		return tool.JSON(out), nil
	})
}

func (k *Kit) readChunkTool() tool.CoreTool {
	return readTool(tool.Spec{
		Name: ToolReadChunk,
		Description: "读取单个招标文件切片的完整正文，可选附带前后各最多 3 个相邻切片上下文。" +
			"结构化表格（行列结构、合并单元格、Markdown 及页码范围）在 chunk.tables 中返回；超长正文与大表支持按偏移量分页读取（Inline & Spill 预算控制）。" +
			"同时返回标准溯源引用 source_ref（含 doc_id、chunk_id、section_code、page_no、page_start、page_end 等），在调用 insert_assets、record_fact 或 report_finding 时必须原样填入以确保结论有据可查。",
		Schema: obj(map[string]any{
			"chunk_id":       integer("要读取的切片 ID（来自 document_outline 或 search_document）"),
			"before":         integer("附带读取前序相邻切片数量（0~3）"),
			"after":          integer("附带读取后续相邻切片数量（0~3）"),
			"text_offset":    integer("正文 UTF-8 字节分页偏移量（翻页时填入上一页返回的 next_text_offset）"),
			"table_index":    integer("大表分页时指定的表格索引号，默认 0"),
			"row_offset":     integer("大表分页时的起始行偏移量（自动保留表头行）"),
			"context_budget": integer("单次读取的正文字节预算上限，默认 16000，最大 32000"),
		}, "chunk_id"),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			ChunkID    int64 `json:"chunk_id"`
			Before     int   `json:"before"`
			After      int   `json:"after"`
			TextOffset int   `json:"text_offset"`
			TableIndex int   `json:"table_index"`
			RowOffset  int   `json:"row_offset"`
			Budget     int   `json:"context_budget"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		target, window, err := k.pg.ChunkWindow(ctx, k.taskID, req.ChunkID, min(max(req.Before, 0), maxReadWindow), min(max(req.After, 0), maxReadWindow))
		if errors.Is(err, db.ErrNotFound) {
			return tool.Errorf("chunk %d not found in this task's documents", req.ChunkID), nil
		}
		if err != nil {
			return tool.Result{}, err
		}
		doc, err := k.pg.GetTenderDocument(ctx, k.taskID, target.DocID)
		if err != nil {
			return tool.Result{}, err
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
		tables := json.RawMessage(nonNullJSON(target.Tables, "[]"))
		paged := len(tables) > budget/4 || req.RowOffset > 0 || req.TableIndex > 0
		var tablePage *retrieval.TablePage
		if paged {
			p, e := retrieval.PageTable(tables, req.TableIndex, req.RowOffset, 100, budget/4)
			if e != nil {
				return tool.Errorf("%v", e), nil
			}
			tablePage = &p
			tables = json.RawMessage(`[]`)
		}
		type neighbour struct {
			ChunkID     int64     `json:"chunk_id"`
			SectionCode string    `json:"section_code"`
			Content     string    `json:"content"`
			SourceRef   SourceRef `json:"source_ref"`
		}
		var before, after []neighbour
		for _, c := range window {
			excerpt, _, _ := retrieval.TextPage(c.Content, 0, max(budget/8, 512))
			n := neighbour{c.ID, c.SectionCode, excerpt, refOf(c, doc)}
			switch {
			case c.Seq < target.Seq:
				before = append(before, n)
			case c.Seq > target.Seq:
				after = append(after, n)
			}
		}
		out := map[string]any{
			"next_text_offset": next,
			"text_complete":    next == len(target.Content),
			"tables_complete":  !paged,
			"chunk": map[string]any{
				"chunk_id": target.ID, "doc_id": target.DocID, "section_code": target.SectionCode,
				"section_title": target.SectionTitle, "page_no": target.PageNo, "page_start": target.PageStart, "page_end": target.PageEnd,
				"page_is_physical": target.PageIsPhysical,
				"level":            target.Level, "mandatory": target.IsMandatory, "content": text,
				"tables": tables,
			},
			"source_ref": refOf(target, doc),
		}
		if tablePage != nil {
			out["table_page"] = tablePage
		}
		if len(before) > 0 {
			out["before"] = before
		}
		if len(after) > 0 {
			out["after"] = after
		}
		return tool.JSON(out), nil
	})
}
