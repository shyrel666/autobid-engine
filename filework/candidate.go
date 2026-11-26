package filework

// S5a：解析候选的有界分页读取（document_parse_result 与材料处理页候选对照共用）。
// 候选来自服务端保存的完整 PreparedTender（file_processing_records.payload），不接受客户端提交的解析内容。
// 每段附原件定位：留存原件 file_id 与 SHA-256、物理页码（表格类文档没有物理页码）、版面框（PDF 点坐标或
// OCR 图像像素坐标与图像尺寸）、OCR 置信度，以及表格类文档的工作表区域。OCR 文字是机器识别结果，未经人工核验。

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/ingest"
)

// CandidateDocument 是候选的文档元数据（采用后写入正式招标文档）。
type CandidateDocument struct {
	TenderNo    string     `json:"tender_no"`
	LotNo       string     `json:"lot_no,omitempty"`
	Title       string     `json:"title"`
	DocRole     string     `json:"doc_role,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	FileName    string     `json:"file_name,omitempty"`
	Format      string     `json:"format,omitempty"`
	PageCount   int        `json:"page_count,omitempty"`
}

// CandidateLocator 是一段候选在原件中的位置。
type CandidateLocator struct {
	FileID         int64           `json:"file_id"`
	SHA256         string          `json:"sha256"`
	PageStart      int             `json:"page_start,omitempty"`
	PageEnd        int             `json:"page_end,omitempty"`
	PageIsPhysical bool            `json:"page_is_physical"`
	OCR            bool            `json:"ocr,omitempty"`
	OCRLines       int             `json:"ocr_lines,omitempty"`
	MinConfidence  float64         `json:"min_confidence,omitempty"`
	BoxCount       int             `json:"box_count,omitempty"`
	Boxes          []ingest.BBox   `json:"boxes,omitempty"`
	SourceLocator  json.RawMessage `json:"source_locator,omitempty"`
}

// CandidateSection 是一段候选正文。
type CandidateSection struct {
	Seq          int              `json:"seq"`
	SectionCode  string           `json:"section_code,omitempty"`
	SectionTitle string           `json:"section_title,omitempty"`
	Mandatory    bool             `json:"is_mandatory,omitempty"`
	Tables       int              `json:"tables,omitempty"`
	Chars        int              `json:"chars"`
	Content      string           `json:"content"`
	Truncated    bool             `json:"truncated,omitempty"`
	Locator      CandidateLocator `json:"locator"`
}

// CandidatePage 是一页候选。NextCursor 为 0 表示没有更多段落。
type CandidatePage struct {
	RecordID      int64              `json:"record_id"`
	Operation     string             `json:"operation"`
	Status        string             `json:"status"`
	FileID        int64              `json:"file_id"`
	SourceHash    string             `json:"source_hash"`
	ParserProfile string             `json:"parser_profile,omitempty"`
	ParserVersion string             `json:"parser_version,omitempty"`
	Document      CandidateDocument  `json:"document"`
	TotalSections int                `json:"total_sections"`
	OCRPages      []int              `json:"ocr_pages,omitempty"`
	Gaps          []ingest.ParseGap  `json:"gaps"`
	Warnings      []string           `json:"warnings,omitempty"`
	Sections      []CandidateSection `json:"sections"`
	Cursor        int                `json:"cursor"`
	NextCursor    int                `json:"next_cursor"`
	Diff          *db.ParsePreview   `json:"diff,omitempty"`
}

// ErrNoCandidate 表示记录没有可读的候选（解析失败）。
var ErrNoCandidate = fmt.Errorf("filework: the record has no parse candidate")

const maxBoxesPerSection = 6

// Candidate 读取记录候选的一页：从 cursor（段序号，0 起）开始最多 limit 段，正文总长不超过 maxChars，
// 单段不超过 perSection（非正值关闭对应上限）。只有首段可以因总字数上限截断，后续装不下的段留到下一页。
func Candidate(r *db.FileProcessingRecord, cursor, limit, maxChars, perSection int) (*CandidatePage, error) {
	if r.Status == db.RecordFailed {
		return nil, ErrNoCandidate
	}
	var prepared ingest.PreparedTender
	if err := json.Unmarshal(r.Payload, &prepared); err != nil || len(prepared.Chunks) == 0 {
		return nil, ErrNoCandidate
	}
	cursor = max(0, min(cursor, len(prepared.Chunks)))
	limit = max(1, limit)
	out := &CandidatePage{RecordID: r.ID, Operation: r.Operation, Status: r.Status, FileID: r.FileID, SourceHash: r.SourceHash,
		ParserProfile: r.ParserProfile, ParserVersion: r.ParserVersion, TotalSections: len(prepared.Chunks), Cursor: cursor,
		OCRPages: prepared.Summary.OCRPages, Gaps: prepared.Summary.Gaps, Warnings: prepared.Summary.Warnings, Sections: []CandidateSection{}}
	if out.Gaps == nil {
		out.Gaps = []ingest.ParseGap{}
	}
	d := prepared.Document
	out.Document = CandidateDocument{TenderNo: d.TenderNo, LotNo: d.LotNo, Title: d.Title, DocRole: d.DocRole, PublishedAt: d.PublishedAt,
		FileName: d.FileName, Format: prepared.Summary.Format, PageCount: d.PageCount}
	if r.Operation == "reparse" && len(r.Preview) > 2 {
		var diff db.ParsePreview
		if json.Unmarshal(r.Preview, &diff) == nil {
			diff.Changes = nil
			out.Diff = &diff
		}
	}
	budget := maxChars
	for i := cursor; i < len(prepared.Chunks) && len(out.Sections) < limit; i++ {
		if maxChars > 0 && budget == 0 {
			break
		}
		c := prepared.Chunks[i]
		text := []rune(c.Content)
		take := len(text)
		if perSection > 0 {
			take = min(take, perSection)
		}
		if maxChars > 0 && take > budget {
			if i > cursor {
				break
			}
			take = budget
		}
		s := CandidateSection{Seq: c.Seq, SectionCode: c.SectionCode, SectionTitle: c.SectionTitle, Mandatory: c.IsMandatory,
			Tables: tableCount(c.Tables), Chars: len(text), Content: string(text[:take]), Truncated: take < len(text), Locator: locate(r, c)}
		budget -= take
		out.Sections = append(out.Sections, s)
		out.NextCursor = i + 1
	}
	if out.NextCursor >= len(prepared.Chunks) {
		out.NextCursor = 0
	}
	return out, nil
}

func tableCount(raw json.RawMessage) int {
	var ts []json.RawMessage
	if json.Unmarshal(raw, &ts) != nil {
		return 0
	}
	return len(ts)
}

func locate(r *db.FileProcessingRecord, c db.DocumentChunk) CandidateLocator {
	loc := CandidateLocator{FileID: r.FileID, SHA256: r.SourceHash, PageStart: c.PageStart, PageEnd: c.PageEnd, PageIsPhysical: c.PageStart > 0}
	var meta struct {
		Boxes          []ingest.BBox   `json:"boxes"`
		OCRLines       []ingest.BBox   `json:"ocr_lines"`
		PageIsPhysical *bool           `json:"page_is_physical"`
		SourceLocator  json.RawMessage `json:"source_locator"`
	}
	if json.Unmarshal(c.BBoxMeta, &meta) != nil {
		return loc
	}
	if meta.PageIsPhysical != nil && !*meta.PageIsPhysical {
		loc.PageIsPhysical, loc.PageStart, loc.PageEnd = false, 0, 0
	}
	loc.SourceLocator = meta.SourceLocator
	// OCR sections locate by line (pixel boxes with per-line confidence); text-layer sections by their page box.
	boxes := meta.Boxes
	if len(meta.OCRLines) > 0 {
		boxes, loc.OCR, loc.OCRLines = meta.OCRLines, true, len(meta.OCRLines)
	}
	loc.BoxCount = len(boxes)
	for i, b := range boxes {
		if b.Coord == ingest.CoordOCRPixels {
			loc.OCR = true
			if b.Conf > 0 && (loc.MinConfidence == 0 || b.Conf < loc.MinConfidence) {
				loc.MinConfidence = b.Conf
			}
		}
		if i < maxBoxesPerSection {
			loc.Boxes = append(loc.Boxes, b)
		}
	}
	return loc
}
