package filework

// S5a（AGENT_TOOL_SKILLS_PLAN 4.1 / 4.3）：Agent 招标解析提交。
//
// 只接受 source_kind=tender_file（任务内的留存原件或 ZIP 子文件）；知识库与企业证明材料来源在 S5b/S5c 分域设计前拒绝。
// Agent 提交强制 candidate_only：导入只生成候选，正式入库须人工采用；重新解析本来就是候选。提交与订阅当前 Intent
// 在同一事务内完成（db.SubmitTenderParse）。导入所需的招标编号等元数据优先取自本任务已有的正式招标文档，
// 缺失时返回 MetadataRequiredError（HUMAN_ACTION_REQUIRED），不臆造招标编号。

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/ingest"
)

// SourceKindTenderFile 是 S5a 唯一开放的来源类型。
const SourceKindTenderFile = "tender_file"

// 尚未开放的来源类型（S5b 知识库、S5c 企业证明材料）。
var futureSourceKinds = []string{"knowledge_file", "company_evidence"}

// ErrSourceKindDisabled 表示来源类型尚未开放（FEATURE_DISABLED）。
var ErrSourceKindDisabled = errors.New("filework: this source kind is not open for agent parsing")

// MetadataRequiredError 表示导入缺少只能由人补充的元数据（HUMAN_ACTION_REQUIRED）。
type MetadataRequiredError struct {
	Missing []string
	Reason  string
}

func (e *MetadataRequiredError) Error() string {
	return fmt.Sprintf("filework: import metadata required (%s): %s", strings.Join(e.Missing, ", "), e.Reason)
}

// AgentParseRequest 是 document_parse_submit 的规范化输入；请求指纹只按它计算（不含服务端推导的元数据），
// 因此同键重放不受之后文档变化影响。
type AgentParseRequest struct {
	SourceKind       string `json:"source_kind"`
	Operation        string `json:"operation"`
	FileID           int64  `json:"file_id,omitempty"`
	DocumentID       int64  `json:"document_id,omitempty"`
	ExpectedRevision *int   `json:"expected_parse_revision,omitempty"`
	ParserProfile    string `json:"parser_profile"`
	RequestKey       string `json:"-"`
	TenderNo         string `json:"tender_no,omitempty"`
	LotNo            string `json:"lot_no,omitempty"`
	Title            string `json:"title,omitempty"`
	DocRole          string `json:"doc_role,omitempty"`
	PublishedAt      string `json:"published_at,omitempty"`
}

func (r *AgentParseRequest) normalize() error {
	r.SourceKind, r.Operation = strings.TrimSpace(r.SourceKind), strings.TrimSpace(r.Operation)
	r.RequestKey = strings.TrimSpace(r.RequestKey)
	r.TenderNo, r.LotNo, r.Title = strings.TrimSpace(r.TenderNo), strings.TrimSpace(r.LotNo), strings.TrimSpace(r.Title)
	r.DocRole, r.PublishedAt = strings.TrimSpace(r.DocRole), strings.TrimSpace(r.PublishedAt)
	switch {
	case slices.Contains(futureSourceKinds, r.SourceKind):
		return fmt.Errorf("%w: %s（S5b / S5c 分域设计完成前不开放）", ErrSourceKindDisabled, r.SourceKind)
	case r.SourceKind != SourceKindTenderFile:
		return fmt.Errorf("%w: source_kind must be tender_file", ingest.ErrInvalidInput)
	}
	if err := db.ValidClientRequestKey(r.RequestKey); err != nil || len(r.RequestKey) > 128 || strings.HasPrefix(r.RequestKey, "_fixed:") {
		return fmt.Errorf("%w: request_key must be 1-128 characters and not start with sys: or _fixed:", ingest.ErrInvalidInput)
	}
	profile := r.ParserProfile
	if profile == "" {
		profile = db.ParserProfileAuto
	}
	if profile == db.ParserProfileEnhancedPDF {
		return fmt.Errorf("%w: enhanced_pdf（增强 PDF 解析后端尚未交付）", ErrProfileDisabled)
	}
	if _, err := NormalizeProfile(profile); err != nil {
		return err
	}
	r.ParserProfile = profile
	switch r.Operation {
	case "import":
		if r.DocumentID != 0 || r.ExpectedRevision != nil {
			return fmt.Errorf("%w: import must not set document_id or expected_parse_revision", ingest.ErrInvalidInput)
		}
		if r.FileID <= 0 {
			return fmt.Errorf("%w: import requires file_id", ingest.ErrInvalidInput)
		}
		if r.DocRole != "" && !db.ValidDocRole(r.DocRole) {
			return fmt.Errorf("%w: invalid doc_role %q", ingest.ErrInvalidInput, r.DocRole)
		}
	case "reparse":
		if r.DocumentID <= 0 || r.ExpectedRevision == nil || *r.ExpectedRevision < 0 {
			return fmt.Errorf("%w: reparse requires document_id and expected_parse_revision (the document's parse_revision integer)", ingest.ErrInvalidInput)
		}
		if r.TenderNo != "" || r.LotNo != "" || r.Title != "" || r.DocRole != "" || r.PublishedAt != "" {
			return fmt.Errorf("%w: reparse keeps the document's metadata; do not pass tender_no / lot_no / title / doc_role / published_at", ingest.ErrInvalidInput)
		}
	default:
		return fmt.Errorf("%w: operation must be import or reparse", ingest.ErrInvalidInput)
	}
	return nil
}

func parsePublished(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			t = t.UTC()
			return &t, nil
		}
	}
	return nil, fmt.Errorf("%w: published_at must be YYYY-MM-DD or RFC3339", ingest.ErrInvalidInput)
}

// importMetadata 用本任务已有的正式招标文档补全导入元数据：只能选用已有的招标编号 / 标段，不接受新造的编号。
func (p *Processor) importMetadata(ctx context.Context, taskID int64, r *AgentParseRequest) error {
	docs, err := p.DB.ListTenderDocuments(ctx, taskID)
	if err != nil {
		return err
	}
	var tenders []string
	lots := map[string][]string{}
	for _, d := range docs {
		if !slices.Contains(tenders, d.TenderNo) {
			tenders = append(tenders, d.TenderNo)
		}
		if d.LotNo != "" && !slices.Contains(lots[d.TenderNo], d.LotNo) {
			lots[d.TenderNo] = append(lots[d.TenderNo], d.LotNo)
		}
	}
	switch {
	case len(tenders) == 0:
		return &MetadataRequiredError{Missing: []string{"tender_no"}, Reason: "本任务还没有正式招标文档，首份文件的招标编号须由人在材料处理页填写并导入"}
	case r.TenderNo == "" && len(tenders) > 1:
		return &MetadataRequiredError{Missing: []string{"tender_no"}, Reason: "本任务有多个招标编号（" + strings.Join(tenders, "、") + "），须指定其中之一"}
	case r.TenderNo == "":
		r.TenderNo = tenders[0]
	case !slices.Contains(tenders, r.TenderNo):
		return &MetadataRequiredError{Missing: []string{"tender_no"}, Reason: "招标编号 " + r.TenderNo + " 不在本任务已有的正式招标文档中；新的招标编号须由人确认"}
	}
	if r.LotNo != "" && !slices.Contains(lots[r.TenderNo], r.LotNo) {
		return &MetadataRequiredError{Missing: []string{"lot_no"}, Reason: "标段 " + r.LotNo + " 不在该招标编号已有的文档中；新的标段须由人确认"}
	}
	return nil
}

// SubmitAgentParse 提交一次 Agent 招标解析（强制候选模式）并订阅宿主绑定的当前 Intent。
func (p *Processor) SubmitAgentParse(ctx context.Context, taskID int64, actor string, req AgentParseRequest) (*db.TenderParseSubmission, error) {
	if !db.IntentExecutionBound(ctx) {
		return nil, db.ErrNoIntentExecution
	}
	if err := req.normalize(); err != nil {
		return nil, err
	}
	if err := db.CheckJobKindScope(ctx, db.JobKindTenderParse); err != nil {
		return nil, err
	}
	fingerprint := db.ContentHash("agent-parse:" + string(marshal(req)))
	in := ProcessInput{RequestKey: req.RequestKey, CandidateOnly: true, ParserProfile: req.ParserProfile}
	var target map[string]any
	var expected *int64
	// A replay returns the original job (any state) before capability or metadata checks.
	if old, err := p.DB.FindFixedFileJob(ctx, db.DefaultTenant, taskID, req.RequestKey); err == nil {
		if old.Fingerprint != fingerprint {
			return nil, fmt.Errorf("%w: %w", db.ErrFileProcessingConflict, db.ErrIdempotencyKeyConflict)
		}
		target = map[string]any{"file_id": old.FileID, "operation": req.Operation}
		return p.DB.SubmitTenderParse(ctx, db.FixedFileJob{TenantID: db.DefaultTenant, TaskID: taskID, FileID: old.FileID, SourceHash: old.SourceHash,
			RequestKey: old.RequestKey, Fingerprint: old.Fingerprint, Input: old.Input, Executor: old.Executor, CreatedBy: actor}, target, nil)
	} else if !errors.Is(err, db.ErrNotFound) {
		return nil, err
	}
	switch req.Operation {
	case "reparse":
		doc, err := p.DB.GetTenderDocument(ctx, taskID, req.DocumentID)
		if err != nil {
			return nil, err
		}
		if doc.OriginalFileID == nil {
			return nil, fmt.Errorf("%w: 原件未留存，不能重新解析", db.ErrInvalidParseRevision)
		}
		if req.FileID > 0 && req.FileID != *doc.OriginalFileID {
			return nil, fmt.Errorf("%w: file_id is not the document's retained original", ingest.ErrInvalidInput)
		}
		if doc.ParseRevision != *req.ExpectedRevision {
			return nil, db.ErrParseRevisionConflict
		}
		in.DocumentID, in.ExpectedRevision, in.FileID = doc.ID, doc.ParseRevision, *doc.OriginalFileID
		in.TenderNo, in.LotNo, in.Title, in.DocRole, in.PublishedAt = doc.TenderNo, doc.LotNo, doc.Title, doc.DocRole, doc.PublishedAt
		rev := int64(doc.ParseRevision)
		expected = &rev
		target = map[string]any{"operation": "reparse", "document_id": doc.ID, "file_id": *doc.OriginalFileID, "parse_revision": doc.ParseRevision}
	default:
		if err := db.CheckFileScope(ctx, req.FileID); err != nil {
			return nil, err
		}
		if err := p.importMetadata(ctx, taskID, &req); err != nil {
			return nil, err
		}
		published, err := parsePublished(req.PublishedAt)
		if err != nil {
			return nil, err
		}
		in.FileID, in.TenderNo, in.LotNo, in.Title, in.DocRole, in.PublishedAt = req.FileID, req.TenderNo, req.LotNo, req.Title, req.DocRole, published
		target = map[string]any{"operation": "import", "file_id": req.FileID, "tender_no": req.TenderNo}
	}
	if err := db.CheckFileScope(ctx, in.FileID); err != nil {
		return nil, err
	}
	obj, err := p.checkJobFile(ctx, taskID, in)
	if err != nil {
		return nil, err
	}
	_, executor, err := p.ResolveRoute(ctx, obj.Format, in.ParserProfile)
	if err != nil {
		return nil, err
	}
	target["parser_profile"], target["executor"] = in.ParserProfile, executor
	return p.DB.SubmitTenderParse(ctx, db.FixedFileJob{TenantID: db.DefaultTenant, TaskID: taskID, FileID: obj.ID, SourceHash: obj.SHA256,
		RequestKey: req.RequestKey, Fingerprint: fingerprint, Input: marshal(in), Executor: executor, CreatedBy: actor}, target, expected)
}
