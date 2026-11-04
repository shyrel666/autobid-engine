package filework

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/ingest"
	"github.com/autobid/autobid-engine/toolhealth"
)

// Processor only runs trusted, fixed parsers. Candidates are produced and saved
// by the server; clients can select a record but cannot supply parsed content.
type Processor struct {
	DB    *db.DB
	Store *Store
	OCR   ingest.OCRProvider
	// Runtime is the local runtime registry; when set, an OCR capability that is unconfigured or failed its
	// self-test is not offered to new parse routes (S5a). Nil means probe the OCR provider directly.
	Runtime *toolhealth.Registry
}
type ProcessInput struct {
	Operation        string     `json:"operation,omitempty"`
	FileID           int64      `json:"file_id"`
	DocumentID       int64      `json:"document_id"`
	ExpectedRevision int        `json:"expected_revision"`
	RequestKey       string     `json:"request_key"`
	TenderNo         string     `json:"tender_no"`
	LotNo            string     `json:"lot_no"`
	Title            string     `json:"title"`
	DocRole          string     `json:"doc_role"`
	PublishedAt      *time.Time `json:"published_at,omitempty"`
	// CandidateOnly 只生成候选（S5a）：导入不写正式招标文档，等待人工采用。Agent 提交恒为 true，旧固定导入默认 false。
	CandidateOnly bool `json:"candidate_only,omitempty"`
	// ParserProfile 是解析路由 auto / native / ocr_only（空等同 auto）；与执行版本一起在首次提交时固定。
	ParserProfile string `json:"parser_profile,omitempty"`
}

func revisionInput(taskID, docID int64, rev int, p *ingest.PreparedTender, actor, version string) db.ParseRevisionInput {
	if version == "" {
		version = FixedParserVersion
	}
	return db.ParseRevisionInput{TenantID: db.DefaultTenant, TaskID: taskID, DocID: docID, ExpectedRevision: rev,
		OriginalFileID: p.Document.OriginalFileID, RawText: p.Document.RawText, Chunks: p.Chunks, ParseMeta: p.Document.ParseMeta,
		PageCount: p.Document.PageCount, ParserVersion: version, Actor: actor}
}
func marshal(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func (p *Processor) Process(ctx context.Context, taskID int64, in ProcessInput, actor string) (*db.FileProcessingRecord, error) {
	if strings.HasPrefix(in.RequestKey, "_fixed:") {
		return nil, ingest.ErrInvalidInput
	}
	return p.process(ctx, taskID, in, actor, nil)
}

// checkSourceFile allows retained originals, text snapshots and ZIP children only. Future script outputs must go
// through their own validation/review workflow.
func (p *Processor) checkSourceFile(ctx context.Context, taskID int64, obj *db.TaskFileObject) error {
	switch obj.Kind {
	case db.FileKindOriginal, db.FileKindTextSnapshot:
	case db.FileKindDerived:
		if obj.ParentFileID == nil {
			return db.ErrInvalidParseRevision
		}
		parent, _, err := p.DB.GetTaskFileObject(ctx, db.DefaultTenant, taskID, *obj.ParentFileID)
		if err != nil {
			return err
		}
		if parent.Format != ingest.FormatZIP || parent.Kind != db.FileKindOriginal {
			return db.ErrInvalidParseRevision
		}
	default:
		return db.ErrInvalidParseRevision
	}
	return nil
}

func (p *Processor) process(ctx context.Context, taskID int64, in ProcessInput, actor string, job *db.FixedFileJob) (*db.FileProcessingRecord, error) {
	in.RequestKey = strings.TrimSpace(in.RequestKey)
	if len(in.RequestKey) == 0 || len(in.RequestKey) > 128 {
		return nil, fmt.Errorf("%w: request_key is required (max 128)", ingest.ErrInvalidInput)
	}
	fingerprint := db.ContentHash(string(marshal(in)))
	if r, err := p.DB.FindFileProcessingRequest(ctx, db.DefaultTenant, taskID, in.RequestKey); err == nil {
		if r.Fingerprint != fingerprint {
			return nil, db.ErrFileProcessingConflict
		}
		return r, nil
	} else if !errors.Is(err, db.ErrNotFound) {
		return nil, err
	}
	operation := "import"
	var base *db.TenderDocument
	if in.DocumentID > 0 {
		operation = "reparse"
		var err error
		base, err = p.DB.GetTenderDocument(ctx, taskID, in.DocumentID)
		if err != nil {
			return nil, err
		}
		if base.OriginalFileID == nil {
			return nil, fmt.Errorf("%w: 原件未留存，请重新上传相同原件后再解析", db.ErrInvalidParseRevision)
		}
		if base.ParseRevision != in.ExpectedRevision {
			return nil, db.ErrParseRevisionConflict
		}
		if job != nil {
			sameDate := base.PublishedAt == nil && in.PublishedAt == nil || base.PublishedAt != nil && in.PublishedAt != nil && base.PublishedAt.Equal(*in.PublishedAt)
			if base.TenderNo != in.TenderNo || base.LotNo != in.LotNo || base.Title != in.Title || base.DocRole != in.DocRole || !sameDate || *base.OriginalFileID != in.FileID {
				return nil, db.ErrFileProcessingConflict
			}
		}
		in.FileID = *base.OriginalFileID
		in.TenderNo, in.LotNo, in.Title, in.DocRole, in.PublishedAt = base.TenderNo, base.LotNo, base.Title, base.DocRole, base.PublishedAt
	}
	data, obj, err := p.Store.Read(ctx, db.DefaultTenant, taskID, in.FileID)
	if err != nil {
		return nil, err
	}
	if job != nil && obj.SHA256 != job.SourceHash {
		return nil, db.ErrInvalidParseRevision
	}
	if err := p.checkSourceFile(ctx, taskID, obj); err != nil {
		return nil, err
	}
	// The route is fixed at first submission: a queued job keeps its executor; a synchronous request resolves it now.
	profile, err := NormalizeProfile(in.ParserProfile)
	if err != nil {
		return nil, err
	}
	executor := ""
	if job != nil {
		executor = job.Executor
	} else if profile, executor, err = p.ResolveRoute(ctx, obj.Format, in.ParserProfile); err != nil {
		return nil, err
	}
	ocr, pin := p.ocrFor(executor)
	rec := &db.FileProcessingRecord{FileID: obj.ID, Operation: operation, RequestKey: in.RequestKey, Fingerprint: fingerprint, SourceHash: obj.SHA256,
		ExpectedRevision: in.ExpectedRevision, CreatedBy: actor, Status: db.RecordReady, ParserProfile: profile,
		CandidateOnly: in.CandidateOnly || operation == "reparse"}
	if base != nil {
		rec.TargetDocumentID = &base.ID
		if base.ContentHash != obj.SHA256 {
			return nil, db.ErrInvalidParseRevision
		}
	}
	file := &ingest.File{Name: obj.OriginalName, MIME: obj.MediaType, Data: data, ObjectID: obj.ID, SHA256: obj.SHA256}
	prepared, err := ingest.PrepareTender(ctx, taskID, ingest.Input{TenderNo: in.TenderNo, LotNo: in.LotNo, Title: in.Title, File: file, OCR: ocr, Profile: profile,
		TenantID: db.DefaultTenant, CreatedBy: actor, DocRole: in.DocRole, PublishedAt: in.PublishedAt, OriginalFileID: &obj.ID})
	// A transient OCR outage or a changed OCR runtime discards this attempt: no candidate with fake gaps is saved.
	if pin != nil {
		if fatal := pin.err(); fatal != nil {
			return nil, fatal
		}
	}
	if err == nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		rec.Status = db.RecordFailed
		rec.Error = err.Error()
		rec.ParserVersion = parserVersion(executor, pin, 0)
		var failed *db.FileProcessingRecord
		err = p.DB.Transaction(ctx, func(tx *db.DB) error {
			if err := tx.LockFileProcessingTask(ctx, db.DefaultTenant, taskID); err != nil {
				return err
			}
			if job != nil {
				if err := tx.GuardFixedFileJob(ctx, job); err != nil {
					return err
				}
			}
			var saveErr error
			failed, saveErr = tx.InsertFileProcessingRecord(ctx, db.DefaultTenant, taskID, rec)
			if saveErr == nil && job != nil {
				saveErr = tx.FinishFixedFileJob(ctx, job, failed, "")
			}
			return saveErr
		})
		return failed, err
	}
	rec.ParserVersion = parserVersion(executor, pin, len(prepared.Summary.OCRPages))
	rec.Payload = marshal(prepared)
	var saved *db.FileProcessingRecord
	err = p.DB.Transaction(ctx, func(tx *db.DB) error {
		if err := tx.LockFileProcessingTask(ctx, db.DefaultTenant, taskID); err != nil {
			return err
		}
		if job != nil {
			if err := tx.GuardFixedFileJob(ctx, job); err != nil {
				return err
			}
		}
		// Lock document before file, matching AdoptTenderParseRevision's lock order.
		if base != nil {
			preview, err := tx.PreviewParseRevision(ctx, revisionInput(taskID, base.ID, in.ExpectedRevision, prepared, actor, rec.ParserVersion))
			if err != nil {
				return err
			}
			rec.Preview = marshal(preview)
		}
		if err := tx.LockTaskFile(ctx, db.DefaultTenant, taskID, obj.ID, obj.SHA256); err != nil {
			return err
		}
		var err error
		saved, err = tx.InsertFileProcessingRecord(ctx, db.DefaultTenant, taskID, rec)
		if err != nil {
			return err
		}
		// Reparse results and candidate-only imports wait for a person; they never touch formal documents here.
		if operation == "reparse" || saved.Status != db.RecordReady || saved.CandidateOnly {
			if job != nil {
				return tx.FinishFixedFileJob(ctx, job, saved, "")
			}
			return nil
		}
		result, err := ingest.PersistTender(ctx, tx, prepared)
		if err != nil {
			return err
		}
		if err = tx.CompleteFileProcessing(ctx, db.DefaultTenant, taskID, saved.ID, marshal(result), actor, ""); err != nil {
			return err
		}
		saved, err = tx.GetFileProcessingRecord(ctx, db.DefaultTenant, taskID, saved.ID, false)
		if err == nil && job != nil {
			err = tx.FinishFixedFileJob(ctx, job, saved, "")
		}
		return err
	})
	return saved, err
}

// Adopt applies a ready candidate after a person reviewed it. A reparse candidate goes through
// AdoptTenderParseRevision (document ID, chunk IDs and the original content hash stay, source_epoch advances on a
// substantive change). An import candidate (S5a) is persisted with PersistTender in the same transaction as the
// adoption record; expected must be 0. Both re-read and hash the retained original first, lock the task's file
// processing gate (which also invalidates older export snapshots) and are idempotent: adopting an adopted record
// returns it unchanged. Rejected and failed records cannot be adopted.
func (p *Processor) Adopt(ctx context.Context, taskID, id int64, expected int, actor, note string) (*db.FileProcessingRecord, error) {
	r, err := p.DB.GetFileProcessingRecord(ctx, db.DefaultTenant, taskID, id, false)
	if err != nil {
		return nil, err
	}
	switch {
	case r.Operation == "reparse" && (r.TargetDocumentID == nil || r.ExpectedRevision != expected):
		return nil, db.ErrFileProcessingConflict
	case r.Operation == "import" && expected != 0:
		return nil, fmt.Errorf("%w: 导入候选没有目标文档修订，expected_revision 必须为 0", db.ErrFileProcessingConflict)
	case r.Operation != "reparse" && r.Operation != "import":
		return nil, db.ErrFileProcessingConflict
	}
	if r.Status == db.RecordAdopted {
		return r, nil
	}
	if r.Status != db.RecordReady {
		return nil, fmt.Errorf("%w: 记录状态为 %s，不能采用", db.ErrFileProcessingConflict, r.Status)
	}
	// Re-read and hash the retained bytes, not just their metadata.
	_, obj, err := p.Store.Read(ctx, db.DefaultTenant, taskID, r.FileID)
	if err != nil {
		return nil, err
	}
	if obj.SHA256 != r.SourceHash {
		return nil, db.ErrInvalidParseRevision
	}
	var out *db.FileProcessingRecord
	err = p.DB.Transaction(ctx, func(tx *db.DB) error {
		if err := tx.LockFileProcessingTask(ctx, db.DefaultTenant, taskID); err != nil {
			return err
		}
		current, err := tx.GetFileProcessingRecord(ctx, db.DefaultTenant, taskID, id, true)
		if err != nil {
			return err
		}
		if current.Status == db.RecordAdopted {
			out = current
			return nil
		}
		if current.Status != db.RecordReady {
			return fmt.Errorf("%w: 记录状态为 %s，不能采用", db.ErrFileProcessingConflict, current.Status)
		}
		var candidate ingest.PreparedTender
		if err = json.Unmarshal(current.Payload, &candidate); err != nil {
			return err
		}
		if current.Operation == "import" {
			if strings.TrimSpace(candidate.Document.TenderNo) == "" || candidate.Document.TaskID != taskID {
				return fmt.Errorf("%w: 候选缺少招标编号或不属于本任务", db.ErrFileProcessingConflict)
			}
			if err := tx.LockTaskFile(ctx, db.DefaultTenant, taskID, current.FileID, current.SourceHash); err != nil {
				return err
			}
			result, err := ingest.PersistTender(ctx, tx, &candidate)
			if err != nil {
				return err
			}
			if err = tx.CompleteFileProcessing(ctx, db.DefaultTenant, taskID, id, marshal(result), actor, note); err != nil {
				return err
			}
			out, err = tx.GetFileProcessingRecord(ctx, db.DefaultTenant, taskID, id, false)
			return err
		}
		// Take the document lock before checking role/date and before the file lock.
		var lockedID int64
		if err = tx.QueryRowContext(ctx, `SELECT id FROM tender_documents WHERE tenant_id=$1 AND task_id=$2 AND id=$3 FOR UPDATE`, db.DefaultTenant, taskID, *r.TargetDocumentID).Scan(&lockedID); err != nil {
			return err
		}
		doc, err := tx.GetTenderDocument(ctx, taskID, *r.TargetDocumentID)
		if err != nil {
			return err
		}
		source := candidate.Document
		sameDate := doc.PublishedAt == nil && source.PublishedAt == nil || doc.PublishedAt != nil && source.PublishedAt != nil && doc.PublishedAt.Equal(*source.PublishedAt)
		if doc.TenderNo != source.TenderNo || doc.LotNo != source.LotNo || doc.Title != source.Title || doc.DocRole != source.DocRole || !sameDate {
			return fmt.Errorf("%w: 文档角色或元数据已变化，请重新解析", db.ErrFileProcessingConflict)
		}
		result, err := tx.AdoptTenderParseRevision(ctx, revisionInput(taskID, doc.ID, expected, &candidate, actor, current.ParserVersion))
		if err != nil {
			return err
		}
		if err = tx.CompleteFileProcessing(ctx, db.DefaultTenant, taskID, id, marshal(result), actor, note); err != nil {
			return err
		}
		out, err = tx.GetFileProcessingRecord(ctx, db.DefaultTenant, taskID, id, false)
		return err
	})
	return out, err
}

// Reject records a person's rejection of a ready candidate (S5a). See db.RejectFileProcessingRecord.
func (p *Processor) Reject(ctx context.Context, taskID, id int64, actor, reason string) (*db.FileProcessingRecord, error) {
	return p.DB.RejectFileProcessingRecord(ctx, db.DefaultTenant, taskID, id, actor, reason)
}
