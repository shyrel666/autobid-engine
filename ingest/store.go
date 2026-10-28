package ingest

// 写库：招标文档（域 A：tender_documents / document_chunks + 根 tender/lot 实体）与企业知识库（域 D）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/autobid/autobid-engine/db"
)

// ErrInvalidInput 表示入库请求缺少必填项（映射为 HTTP 400）。
var ErrInvalidInput = errors.New("ingest: invalid input")

// Input 是一次招标文档入库：Text 为纯文本（JSON 上传），File 为原始文件（multipart 上传），二者取其一，File 优先。
type Input struct {
	TenderNo  string
	LotNo     string
	Title     string
	Text      string
	File      *File
	OCR       OCRProvider
	TenantID  string
	CreatedBy string
}

type Result struct {
	Document      *db.TenderDocument `json:"document"`
	Created       bool               `json:"created"`
	Chunks        int                `json:"chunks"`
	Mandatory     int                `json:"mandatory_chunks"`
	Tables        int                `json:"tables"`
	PageCount     int                `json:"page_count"`
	Format        string             `json:"format"`
	Warnings      []string           `json:"warnings,omitempty"`
	TenderAssetID int64              `json:"tender_asset_id"`
	LotAssetID    int64              `json:"lot_asset_id,omitempty"`
}

// parsedInput 解析文件或文本，返回段落与解析元数据；纯文本沿用 Split 规则。
func parsedInput(ctx context.Context, text string, f *File, ocr OCRProvider) (*Parsed, []Section, error) {
	if f != nil {
		p, err := Parse(ctx, *f, ParseOptions{OCR: ocr})
		if err != nil {
			return nil, nil, err
		}
		return p, p.Sections(), nil
	}
	if strings.TrimSpace(text) == "" {
		return nil, nil, fmt.Errorf("%w: text is empty", ErrInvalidInput)
	}
	p := &Parsed{Format: FormatText, Text: text, Lines: TextLines(text), Meta: ParseMeta{Parser: "text"}}
	p.finish()
	p.Text = text // 纯文本原样保存，保持 content_hash 与 Phase 2 一致
	return p, Split(text), nil
}

// sectionJSON 返回段落的表格 JSON 数组与版面溯源 JSON。
func sectionJSON(s Section, format string) (tables, bbox json.RawMessage) {
	ts := s.Tables
	if ts == nil {
		ts = []*Table{}
	}
	tables, _ = json.Marshal(ts)
	meta := map[string]any{"source": format}
	if s.BlockStart >= 0 {
		meta["block_start"], meta["block_end"] = s.BlockStart, s.BlockEnd
	}
	if len(s.Boxes) > 0 {
		meta["boxes"] = s.Boxes
	}
	bbox, _ = json.Marshal(meta)
	return tables, bbox
}

// Ingest 解析、切分并写入一份招标文档，幂等创建根 tender（LotNo 非空时再建 lot 与 tender -contains-> lot 边）。
// 实体经任务 AssetStore 写入，因此会归集 task_ids 并自动补任务范围。相同原文（文件为相同字节）重复上传不会重复切分。
func Ingest(ctx context.Context, pg *db.DB, as *db.AssetStore, in Input) (*Result, error) {
	in.TenderNo, in.LotNo = strings.TrimSpace(in.TenderNo), strings.TrimSpace(in.LotNo)
	if in.TenderNo == "" {
		return nil, fmt.Errorf("%w: tender_no is required", ErrInvalidInput)
	}
	if as == nil || as.TaskID() <= 0 {
		return nil, errors.New("ingest: a task-bound asset store is required")
	}
	parsed, sections, err := parsedInput(ctx, in.Text, in.File, in.OCR)
	if err != nil {
		return nil, err
	}
	if in.Title == "" {
		in.Title = parsed.Title
	}
	if in.Title == "" && in.File != nil {
		in.Title = in.File.Name
	}
	docRef := map[string]any{"doc_title": in.Title}
	if in.File != nil {
		docRef["file_name"] = in.File.Name
	}
	res := &Result{Format: parsed.Format, PageCount: parsed.PageCount, Tables: len(parsed.Tables), Warnings: parsed.Meta.Warnings}
	res.TenderAssetID, _, err = as.UpsertAsset(ctx, &db.Asset{Type: db.AssetTender, TenderNo: in.TenderNo, Title: in.Title, SourceRef: mustJSON(docRef)}, 0)
	if err != nil {
		return nil, fmt.Errorf("ingest: upsert tender: %w", err)
	}
	if in.LotNo != "" {
		res.LotAssetID, _, err = as.UpsertAsset(ctx, &db.Asset{Type: db.AssetLot, TenderNo: in.TenderNo, LotNo: in.LotNo, Title: in.Title, SourceRef: mustJSON(docRef)}, 0)
		if err != nil {
			return nil, fmt.Errorf("ingest: upsert lot: %w", err)
		}
		if _, err := as.UpsertEntityEdge(ctx, res.TenderAssetID, db.EntityRelContains, res.LotAssetID, as.TaskID(), nil, 1, nil); err != nil {
			return nil, fmt.Errorf("ingest: link lot: %w", err)
		}
	}

	chunks := make([]db.DocumentChunk, 0, len(sections))
	for i, s := range sections {
		tables, bbox := sectionJSON(s, parsed.Format)
		chunks = append(chunks, db.DocumentChunk{Seq: i, Level: s.Level, SectionCode: s.Code, SectionTitle: s.Title,
			PageNo: s.PageStart, PageStart: s.PageStart, PageEnd: s.PageEnd, Content: s.Content, TokenEstimate: EstimateTokens(s.Content),
			IsMandatory: s.Mandatory, Tables: tables, BBoxMeta: bbox})
		if s.Mandatory {
			res.Mandatory++
		}
	}
	doc := &db.TenderDocument{TenantID: in.TenantID, TaskID: as.TaskID(), TenderNo: in.TenderNo, LotNo: in.LotNo, Title: in.Title,
		RawText: parsed.Text, CreatedBy: in.CreatedBy}
	if in.File != nil {
		meta, _ := json.Marshal(parsed.Meta)
		doc.FileName, doc.MimeType, doc.PageCount, doc.ParseMeta = in.File.Name, parsed.MIME, parsed.PageCount, meta
		doc.ContentHash = db.ContentHash(string(in.File.Data))
	}
	docID, created, err := pg.InsertTenderDocument(ctx, doc, chunks)
	if err != nil {
		return nil, fmt.Errorf("ingest: %w", err)
	}
	res.Created = created
	if res.Document, err = pg.GetTenderDocument(ctx, as.TaskID(), docID); err != nil {
		return nil, err
	}
	res.Chunks = res.Document.ChunkCount
	if !created {
		res.Mandatory, res.Tables = 0, 0
		outline, err := pg.DocumentOutline(ctx, as.TaskID(), docID, 100000)
		if err != nil {
			return nil, err
		}
		for _, c := range outline {
			if c.IsMandatory {
				res.Mandatory++
			}
			res.Tables += c.TableCount
		}
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// 企业方案知识库
// ---------------------------------------------------------------------------

// KnowledgeInput 是一次知识库入库：Text 与 File 二选一，File 优先。Extra 为产品名、厂商、项目年份等可选元数据。
type KnowledgeInput struct {
	TenantID  string
	Category  string
	Title     string
	Tags      []string
	Extra     map[string]any
	Text      string
	FileName  string // Text 上传时的可选来源文件名
	File      *File
	OCR       OCRProvider
	CreatedBy string
}

type KnowledgeResult struct {
	Document  *db.KnowledgeDocument `json:"document"`
	Created   bool                  `json:"created"`
	Chunks    int                   `json:"chunks"`
	Tables    int                   `json:"tables"`
	PageCount int                   `json:"page_count"`
	Format    string                `json:"format"`
	Warnings  []string              `json:"warnings,omitempty"`
}

// SectionPaths 计算每个段落的章节路径（祖先标题 "编号 标题" 以 " / " 连接，含自身）。
func SectionPaths(secs []Section) []string {
	type node struct {
		level int
		label string
	}
	var stack []node
	out := make([]string, len(secs))
	for i, s := range secs {
		label := strings.TrimSpace(strings.TrimSpace(s.Code) + " " + strings.TrimSpace(s.Title))
		if s.Level == 0 {
			out[i] = label
			continue
		}
		for len(stack) > 0 && stack[len(stack)-1].level >= s.Level {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, node{s.Level, label})
		parts := make([]string, len(stack))
		for k, n := range stack {
			parts[k] = n.label
		}
		out[i] = strings.Join(parts, " / ")
	}
	return out
}

// IngestKnowledge 解析、切分并写入一份知识库文档；同租户相同内容（文件为相同字节）重复上传幂等返回已有文档。
func IngestKnowledge(ctx context.Context, pg *db.DB, in KnowledgeInput, emb db.Embedder) (*KnowledgeResult, error) {
	in.Category = strings.TrimSpace(in.Category)
	if !db.ValidKnowledgeCategory(in.Category) {
		return nil, fmt.Errorf("%w: category must be one of %s", ErrInvalidInput, strings.Join(db.KnowledgeCategories, ", "))
	}
	parsed, sections, err := parsedInput(ctx, in.Text, in.File, in.OCR)
	if err != nil {
		return nil, err
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = parsed.Title
	}
	fileName := in.FileName
	if in.File != nil {
		fileName = in.File.Name
	}
	if title == "" {
		title = fileName
	}
	paths := SectionPaths(sections)
	chunks := make([]db.KnowledgeChunk, 0, len(sections))
	for i, s := range sections {
		tables, bbox := sectionJSON(s, parsed.Format)
		chunks = append(chunks, db.KnowledgeChunk{Seq: i, Level: s.Level, SectionCode: s.Code, SectionTitle: s.Title, SectionPath: paths[i],
			PageStart: s.PageStart, PageEnd: s.PageEnd, Content: s.Content, TokenEstimate: EstimateTokens(s.Content), IsMandatory: s.Mandatory,
			Tables: tables, BBoxMeta: bbox})
	}
	meta, _ := json.Marshal(parsed.Meta)
	doc := &db.KnowledgeDocument{TenantID: in.TenantID, Category: in.Category, Title: title, Tags: in.Tags, FileName: fileName,
		MimeType: parsed.MIME, PageCount: parsed.PageCount, RawText: parsed.Text, ParseMeta: meta, CreatedBy: in.CreatedBy}
	if in.Extra != nil {
		doc.Extra, _ = json.Marshal(in.Extra)
	}
	if in.File != nil {
		doc.ContentHash = db.ContentHash(string(in.File.Data))
	}
	id, created, err := pg.InsertKnowledgeDocument(ctx, doc, chunks, emb)
	if err != nil {
		return nil, err
	}
	saved, err := pg.GetKnowledgeDocument(ctx, in.TenantID, id, false)
	if err != nil {
		return nil, err
	}
	return &KnowledgeResult{Document: saved, Created: created, Chunks: saved.ChunkCount, Tables: saved.TableCount,
		PageCount: saved.PageCount, Format: parsed.Format, Warnings: parsed.Meta.Warnings}, nil
}

func mustJSON(v map[string]any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}
