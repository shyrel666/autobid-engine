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
	TenderNo     string `json:"tender_no,omitempty"`
	LotNo        string `json:"lot_no,omitempty"`
	DocTitle     string `json:"doc_title,omitempty"`
}

func refOf(c *db.DocumentChunk, doc *db.TenderDocument) SourceRef {
	r := SourceRef{DocID: c.DocID, ChunkID: c.ID, SectionCode: c.SectionCode, SectionTitle: c.SectionTitle, PageNo: c.PageNo}
	if doc != nil {
		r.TenderNo, r.LotNo, r.DocTitle = doc.TenderNo, doc.LotNo, doc.Title
	}
	return r
}

func (k *Kit) documentOutlineTool() tool.CoreTool {
	return readTool(tool.Spec{
		Name: ToolDocumentOutline,
		Description: "List the section outline of this task's tender documents (doc_id, chunk_id, section_code, section_title, page_no, level). " +
			"mandatory=true marks ★/*/废标/否决 clauses. Use it to decide which chunks to read.",
		Schema: obj(map[string]any{
			"doc_id":    integer("only this document; omit for all documents"),
			"max_level": integer("only headings up to this level (1 chapter, 2 section, 3+ numbered clauses); omit for all"),
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
			DocID        int64  `json:"doc_id"`
			ChunkID      int64  `json:"chunk_id"`
			Level        int    `json:"level"`
			SectionCode  string `json:"section_code"`
			SectionTitle string `json:"section_title"`
			PageNo       int    `json:"page_no"`
			Mandatory    bool   `json:"mandatory,omitempty"`
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
			entries = append(entries, entry{c.DocID, c.ID, c.Level, c.SectionCode, truncate(c.SectionTitle, 60), c.PageNo, c.IsMandatory})
		}
		type docRow struct {
			DocID    int64  `json:"doc_id"`
			Title    string `json:"title"`
			TenderNo string `json:"tender_no"`
			LotNo    string `json:"lot_no,omitempty"`
			Chunks   int    `json:"chunks"`
		}
		docRows := make([]docRow, 0, len(docs))
		for _, d := range docs {
			if req.DocID == 0 || d.ID == req.DocID {
				docRows = append(docRows, docRow{d.ID, d.Title, d.TenderNo, d.LotNo, d.ChunkCount})
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
	ChunkID      int64  `json:"chunk_id"`
	DocID        int64  `json:"doc_id"`
	SectionCode  string `json:"section_code"`
	SectionTitle string `json:"section_title"`
	PageNo       int    `json:"page_no"`
	Mandatory    bool   `json:"mandatory,omitempty"`
	Snippet      string `json:"snippet"`
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
		matches = append(matches, chunkMatch{c.ID, c.DocID, c.SectionCode, c.SectionTitle, c.PageNo, mandatory, snippet(text, loc)})
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
		Description: "Search this task's tender document chunks by keywords (all must appear, case-insensitive) or a regular expression. " +
			"mandatory_only=true keeps only ★/*/废标/否决/无效投标/实质性 clauses (query may then be empty). " +
			"Returns snippets with chunk_id; call read_chunk for the full text and its source_ref.",
		Schema: obj(map[string]any{
			"query":          str("keywords separated by spaces, or a regex when regex=true"),
			"regex":          boolean("treat query as a Go/RE2 regular expression"),
			"mandatory_only": boolean("only mandatory / disqualification clauses"),
			"doc_id":         integer("only this document"),
			"limit":          integer("max matches returned, default 20, max 100"),
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
		Description: "Read the full text of one tender document chunk, optionally with up to 3 neighbouring chunks before/after it. " +
			"Returns source_ref {doc_id, chunk_id, section_code, page_no, ...}: copy it verbatim into insert_assets.source_ref, " +
			"record_fact.source_ref or report_finding.source_refs so every conclusion cites its origin.",
		Schema: obj(map[string]any{
			"chunk_id": integer("chunk id from document_outline / search_document"),
			"before":   integer("neighbouring chunks before, 0-3"),
			"after":    integer("neighbouring chunks after, 0-3"),
		}, "chunk_id"),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			ChunkID int64 `json:"chunk_id"`
			Before  int   `json:"before"`
			After   int   `json:"after"`
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
		type neighbour struct {
			ChunkID     int64     `json:"chunk_id"`
			SectionCode string    `json:"section_code"`
			Content     string    `json:"content"`
			SourceRef   SourceRef `json:"source_ref"`
		}
		var before, after []neighbour
		for _, c := range window {
			n := neighbour{c.ID, c.SectionCode, c.Content, refOf(c, doc)}
			switch {
			case c.Seq < target.Seq:
				before = append(before, n)
			case c.Seq > target.Seq:
				after = append(after, n)
			}
		}
		out := map[string]any{
			"chunk": map[string]any{
				"chunk_id": target.ID, "doc_id": target.DocID, "section_code": target.SectionCode,
				"section_title": target.SectionTitle, "page_no": target.PageNo, "level": target.Level,
				"mandatory": target.IsMandatory, "content": target.Content,
			},
			"source_ref": refOf(target, doc),
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
