package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func newProcessingTask(t *testing.T) (*DB, *Task) {
	t.Helper()
	d := openTestDB(t)
	task, err := d.CreateTask(context.Background(), CreateTaskInput{Name: uniq("file-processing"), Goal: "复杂文件处理"})
	if err != nil {
		t.Fatal(err)
	}
	return d, task
}

func processingDoc(t *testing.T, d *DB, taskID int64, contents ...string) (int64, string) {
	t.Helper()
	ctx := context.Background()
	var raw strings.Builder
	chunks := make([]DocumentChunk, 0, len(contents))
	for i, content := range contents {
		raw.WriteString(content)
		raw.WriteString("\n")
		chunks = append(chunks, DocumentChunk{Seq: i, Level: 2, SectionCode: fmt.Sprintf("1.%d", i+1), SectionTitle: fmt.Sprintf("条款%d", i+1),
			PageStart: 1, PageEnd: 1, Content: content, TokenEstimate: 10, Tables: []byte("[]"), BBoxMeta: []byte("{}")})
	}
	doc := &TenderDocument{TenantID: DefaultTenant, TaskID: taskID, TenderNo: uniq("TENDER"), Title: "测试招标文件",
		RawText: raw.String(), FileName: "tender.docx", MimeType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		PageCount: 1, ContentHash: ContentHash("original-file-bytes"), CreatedBy: "tester"}
	id, created, err := d.InsertTenderDocument(ctx, doc, chunks)
	if err != nil || !created {
		t.Fatalf("insert doc: %d/%v %v", id, created, err)
	}
	return id, raw.String()
}

func revisionChunks(contents ...string) []DocumentChunk {
	return revisionChunksAt(0, contents...)
}

func revisionChunksAt(start int, contents ...string) []DocumentChunk {
	out := make([]DocumentChunk, 0, len(contents))
	for i, content := range contents {
		seq := start + i
		out = append(out, DocumentChunk{Seq: seq, Level: 2, SectionCode: fmt.Sprintf("1.%d", seq+1), SectionTitle: fmt.Sprintf("条款%d", seq+1),
			PageStart: 1, PageEnd: 1, Content: content, TokenEstimate: 10, Tables: []byte("[]"), BBoxMeta: []byte("{}")})
	}
	return out
}

func mustExtractAndConfirm(t *testing.T, d *DB, taskID int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := d.ExtractRequirements(ctx, DefaultTenant, taskID, "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ConfirmRequirements(ctx, DefaultTenant, taskID, "tester", 0); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateIdempotent(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var checksum string
	if err := d.QueryRowContext(ctx, `SELECT checksum FROM schema_version WHERE id=1`).Scan(&checksum); err != nil || len(checksum) != 64 {
		t.Fatalf("schema_version = %q %v", checksum, err)
	}
}

func TestRetrievalInvalidationOnlyContentColumns(t *testing.T) {
	d, task := newProcessingTask(t)
	ctx := context.Background()
	docID, _ := processingDoc(t, d, task.ID, "第一章 资格要求", "投标人须具备有效的营业执照。")
	chunks, err := d.TaskChunks(ctx, task.ID, docID, 10)
	if err != nil || len(chunks) != 2 {
		t.Fatalf("chunks = %d %v", len(chunks), err)
	}
	var runID int64
	if err := d.QueryRowContext(ctx, `INSERT INTO retrieval_runs(tenant_id, task_id, request_key, request_hash, request, kind,
		status, max_calls, max_tokens, deadline) VALUES ($1,$2,$3,'h','{}'::jsonb,'search','completed',1,1,now()+interval '1 hour')
		RETURNING id`, DefaultTenant, task.ID, uniq("retrieval")).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	var evidenceID int64
	if err := d.QueryRowContext(ctx, `INSERT INTO retrieval_evidence(tenant_id, run_id, source_domain, source_id, doc_id,
		content_hash, snapshot, status) VALUES ($1,$2,'tender',$3,$4,'h','{}'::jsonb,'unverified') RETURNING id`,
		DefaultTenant, runID, chunks[0].ID, docID).Scan(&evidenceID); err != nil {
		t.Fatal(err)
	}
	status := func() string {
		var s string
		if err := d.QueryRowContext(ctx, `SELECT status FROM retrieval_evidence WHERE id=$1`, evidenceID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	var fileID int64
	if err := d.QueryRowContext(ctx, `INSERT INTO task_file_objects(tenant_id, task_id, kind, sha256, byte_size, relative_path, original_name)
		VALUES ($1,$2,'original',$3,0,$4,'tender.docx') RETURNING id`, DefaultTenant, task.ID, strings.Repeat("a", 64),
		"default/objects/"+uniq("f")).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE tender_documents SET original_file_id = $2, parse_revision = parse_revision + 1,
		page_count = NULL, parse_meta = '{}'::jsonb WHERE id=$1`, docID, fileID); err != nil {
		t.Fatal(err)
	}
	if status() != "unverified" {
		t.Fatalf("metadata-only document update invalidated evidence: %s", status())
	}
	if _, err := d.ExecContext(ctx, `UPDATE document_chunks SET token_estimate = token_estimate + 1,
		bbox_meta = '{"x":1}'::jsonb WHERE id=$1`, chunks[0].ID); err != nil {
		t.Fatal(err)
	}
	if status() != "unverified" {
		t.Fatalf("metadata-only chunk update invalidated evidence: %s", status())
	}
	// UPDATE OF 列触发器在同值写入时也会触发；实际值不变的 SET 不得使证据失效。
	if _, err := d.ExecContext(ctx, `UPDATE document_chunks SET content = content WHERE id=$1`, chunks[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE tender_documents SET raw_text = raw_text WHERE id=$1`, docID); err != nil {
		t.Fatal(err)
	}
	if status() != "unverified" {
		t.Fatalf("same-value content update invalidated evidence: %s", status())
	}
	if _, err := d.ExecContext(ctx, `UPDATE document_chunks SET content = content || '（补丁）' WHERE id=$1`, chunks[0].ID); err != nil {
		t.Fatal(err)
	}
	if status() != "stale" {
		t.Fatalf("content update did not invalidate evidence: %s", status())
	}
	if _, err := d.ExecContext(ctx, `UPDATE retrieval_evidence SET status='unverified' WHERE id=$1`, evidenceID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE tender_documents SET raw_text = raw_text || ' ' WHERE id=$1`, docID); err != nil {
		t.Fatal(err)
	}
	if status() != "stale" {
		t.Fatalf("raw_text update did not invalidate evidence: %s", status())
	}
}

func TestAdoptTenderParseRevisionOriginalHash(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(fmt.Sprintf("already_linked=%t", linked), func(t *testing.T) {
			d, task := newProcessingTask(t)
			ctx := context.Background()
			docID, raw := processingDoc(t, d, task.ID, "投标人须具备有效的营业执照。")
			mustExtractAndConfirm(t, d, task.ID)
			register := func(contents string) int64 {
				t.Helper()
				hash := ContentHash(contents)
				obj, _, err := d.InsertTaskFileObject(ctx, &TaskFileObject{
					TenantID: DefaultTenant, TaskID: task.ID, Kind: FileKindOriginal,
					SHA256: hash, ByteSize: int64(len(contents)), RelativePath: "default/objects/" + hash,
					OriginalName: "tender.docx", CreatedBy: "tester",
				})
				if err != nil {
					t.Fatal(err)
				}
				return obj.ID
			}
			originalID := register("original-file-bytes")
			otherID := register("different-file-bytes")
			if linked {
				if _, err := d.ExecContext(ctx, `UPDATE tender_documents SET original_file_id=$2 WHERE id=$1`, docID, originalID); err != nil {
					t.Fatal(err)
				}
			}
			// 比较整个文档、分块和清单，保证错误关联在任何快照/业务写入之前被拒绝。
			state := func() string {
				t.Helper()
				var snapshot string
				if err := d.QueryRowContext(ctx, `SELECT jsonb_build_object(
					'document', to_jsonb(d),
					'chunks', (SELECT jsonb_agg(to_jsonb(c) ORDER BY c.seq) FROM document_chunks c WHERE c.doc_id=d.id),
					'manifest', (SELECT to_jsonb(m) FROM requirement_manifests m WHERE m.tenant_id=d.tenant_id AND m.task_id=d.task_id),
					'snapshots', (SELECT count(*) FROM tender_parse_revisions r WHERE r.document_id=d.id))::text
					FROM tender_documents d WHERE d.id=$1`, docID).Scan(&snapshot); err != nil {
					t.Fatal(err)
				}
				return snapshot
			}
			before := state()
			in := ParseRevisionInput{TenantID: DefaultTenant, TaskID: task.ID, DocID: docID,
				OriginalFileID: &otherID, ExpectedRevision: 0, ParserVersion: "test", PageCount: 1,
				RawText: "投标人须提供营业执照复印件。\n", Chunks: revisionChunks("投标人须提供营业执照复印件。"), Actor: "tester"}
			res, err := d.AdoptTenderParseRevision(ctx, in)
			if !errors.Is(err, ErrInvalidParseRevision) || res != nil {
				t.Fatalf("mismatched original must be rejected: result=%+v err=%v", res, err)
			}
			if after := state(); after != before {
				t.Fatal("rejected original changed document, chunks, manifest or revision snapshots")
			}
			// 匹配的是原始文件字节哈希，不是解析正文的哈希；仍允许同原件重新解析。
			in.OriginalFileID = &originalID
			res, err = d.AdoptTenderParseRevision(ctx, in)
			if err != nil || res == nil || res.RevisionNo != 1 || !res.SourceBumped {
				t.Fatalf("matching original rejected: result=%+v err=%v", res, err)
			}
			doc, err := d.GetTenderDocument(ctx, task.ID, docID)
			if err != nil || doc.OriginalFileID == nil || *doc.OriginalFileID != originalID || doc.ContentHash != ContentHash("original-file-bytes") {
				t.Fatalf("matching original not preserved: document=%+v err=%v", doc, err)
			}
			var snapshotRaw string
			if err := d.QueryRowContext(ctx, `SELECT raw_text FROM tender_parse_revisions WHERE document_id=$1 AND revision_no=0`, docID).Scan(&snapshotRaw); err != nil || snapshotRaw != raw {
				t.Fatalf("previous document snapshot = %q, err=%v", snapshotRaw, err)
			}
		})
	}
}

func TestAdoptTenderParseRevisionDiffAndGating(t *testing.T) {
	d, task := newProcessingTask(t)
	ctx := context.Background()
	docID, raw := processingDoc(t, d, task.ID,
		"投标人须具备有效的营业执照。", "服务团队不少于 10 人。", "质保期不少于 3 年。")
	mustExtractAndConfirm(t, d, task.ID)
	before, err := d.RequirementManifest(ctx, DefaultTenant, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.SourceEpoch == 0 || before.ExtractedSourceEpoch != before.SourceEpoch || before.Status != ManifestConfirmed {
		t.Fatalf("initial manifest = %+v", before)
	}
	// 1) 无实质变化：只写快照、parse_revision 递增，清单/来源 epoch 不变。
	res, err := d.AdoptTenderParseRevision(ctx, ParseRevisionInput{TenantID: DefaultTenant, TaskID: task.ID, DocID: docID,
		ExpectedRevision: 0, ParserVersion: "test", RawText: raw, PageCount: 1,
		Chunks: revisionChunks("投标人须具备有效的营业执照。", "服务团队不少于 10 人。", "质保期不少于 3 年。"), Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Inserted+res.Updated+res.Deleted != 0 || res.Unchanged != 3 || res.SourceBumped || res.SnapshotRevision != 0 || res.RevisionNo != 1 {
		t.Fatalf("no-change adoption = %+v", res)
	}
	var contentHash string
	if err := d.QueryRowContext(ctx, `SELECT content_hash FROM tender_documents WHERE id=$1`, docID).Scan(&contentHash); err != nil ||
		contentHash != ContentHash("original-file-bytes") {
		t.Fatalf("content_hash changed by revision: %q %v", contentHash, err)
	}
	after, err := d.RequirementManifest(ctx, DefaultTenant, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Epoch != before.Epoch || after.SourceEpoch != before.SourceEpoch || after.Status != before.Status {
		t.Fatalf("no-change adoption changed manifest: %+v -> %+v", before, after)
	}
	var snapshotRaw, snapshotChunks string
	if err := d.QueryRowContext(ctx, `SELECT raw_text, chunks_snapshot::text FROM tender_parse_revisions
		WHERE tenant_id=$1 AND document_id=$2 AND revision_no=0`, DefaultTenant, docID).Scan(&snapshotRaw, &snapshotChunks); err != nil {
		t.Fatal(err)
	}
	if snapshotRaw != raw || !strings.Contains(snapshotChunks, "有效的营业执照") {
		t.Fatalf("snapshot content = %q / %s", snapshotRaw, snapshotChunks)
	}
	if _, err := d.ExecContext(ctx, `UPDATE tender_parse_revisions SET raw_text='mutated' WHERE document_id=$1 AND revision_no=0`, docID); err == nil {
		t.Fatal("immutable snapshot was updated")
	}

	// 2) 原未关联要求的分块变化：仍须推进 source_epoch 并使确认/导出受阻。
	res, err = d.AdoptTenderParseRevision(ctx, ParseRevisionInput{TenantID: DefaultTenant, TaskID: task.ID, DocID: docID,
		ExpectedRevision: 1, ParserVersion: "test", RawText: raw + "服务团队须提供近三年类似项目业绩合同关键页及验收报告。\n", PageCount: 1,
		Chunks: revisionChunks("投标人须具备有效的营业执照。", "服务团队不少于 10 人。",
			"质保期不少于 3 年。", "服务团队须提供近三年类似项目业绩合同关键页及验收报告。"), Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Inserted != 1 || !res.SourceBumped {
		t.Fatalf("insert-only adoption = %+v", res)
	}
	if _, err := d.ConfirmRequirements(ctx, DefaultTenant, task.ID, "tester", 0); !errors.Is(err, ErrRequirementReview) ||
		!strings.Contains(err.Error(), "重新提取") {
		t.Fatalf("confirm after source change = %v", err)
	}
	ready, err := d.ExportReadiness(ctx, DefaultTenant, task.ID)
	if err != nil || ready.Counts[BlockSourcePending] != 1 {
		t.Fatalf("export readiness after source change = %+v %v", ready, err)
	}
	// 重新提取后才解除待提取状态；新增项须人工复核后确认。
	if _, err := d.ExtractRequirements(ctx, DefaultTenant, task.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	m, _ := d.RequirementManifest(ctx, DefaultTenant, task.ID)
	if m.SourceEpoch != m.ExtractedSourceEpoch {
		t.Fatalf("extraction did not consume source epoch: %+v", m)
	}
	if _, err := d.ExecContext(ctx, `UPDATE requirement_items SET review_status='confirmed' WHERE tenant_id=$1 AND task_id=$2 AND review_status='needs_revision'`, DefaultTenant, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ConfirmRequirements(ctx, DefaultTenant, task.ID, "tester", 0); err != nil {
		t.Fatalf("confirm after re-extraction: %v", err)
	}

	// 3) 关联要求的分块变化：要求 needs_revision，重新提取后手动复核才能确认。
	res, err = d.AdoptTenderParseRevision(ctx, ParseRevisionInput{TenantID: DefaultTenant, TaskID: task.ID, DocID: docID,
		ExpectedRevision: 2, ParserVersion: "test", RawText: raw + "投标人须具备有效的营业执照，并提供复印件。\n", PageCount: 1,
		Chunks: revisionChunks("投标人须具备有效的营业执照，并提供复印件。", "服务团队不少于 10 人。",
			"质保期不少于 3 年。", "服务团队须提供近三年类似项目业绩合同关键页及验收报告。"), Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated != 1 || !res.SourceBumped {
		t.Fatalf("content adoption = %+v", res)
	}
	items, err := d.ListRequirements(ctx, DefaultTenant, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	linked := 0
	for _, it := range items.Items {
		if it.SourceChunkID != nil && *it.SourceChunkID == chunkIDBySeq(t, d, docID, 0) {
			linked++
			if it.ReviewStatus != ReviewNeedsRevision {
				t.Fatalf("linked requirement not needs_revision: %+v", it)
			}
		}
	}
	if linked == 0 {
		t.Fatal("no requirement linked to changed chunk")
	}
	if _, err := d.ConfirmRequirements(ctx, DefaultTenant, task.ID, "tester", 0); !errors.Is(err, ErrRequirementReview) {
		t.Fatalf("confirm with changed requirement = %v", err)
	}

	// 4) 删除分块：要求变为 needs_revision 且 source_chunk_id 为 NULL。
	if _, err := d.ExtractRequirements(ctx, DefaultTenant, task.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE requirement_items SET review_status='confirmed' WHERE tenant_id=$1 AND task_id=$2 AND review_status='needs_revision'`, DefaultTenant, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ConfirmRequirements(ctx, DefaultTenant, task.ID, "tester", 0); err != nil {
		t.Fatal(err)
	}
	res, err = d.AdoptTenderParseRevision(ctx, ParseRevisionInput{TenantID: DefaultTenant, TaskID: task.ID, DocID: docID,
		ExpectedRevision: 3, ParserVersion: "test", RawText: "服务团队不少于 10 人。\n质保期不少于 3 年。\n服务团队须提供近三年类似项目业绩合同关键页及验收报告。\n", PageCount: 1,
		Chunks: revisionChunksAt(1, "服务团队不少于 10 人。", "质保期不少于 3 年。", "服务团队须提供近三年类似项目业绩合同关键页及验收报告。"), Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 || res.Updated != 0 || !res.SourceBumped {
		t.Fatalf("delete adoption = %+v", res)
	}
	items, err = d.ListRequirements(ctx, DefaultTenant, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items.Items {
		if it.Code == "RQ-"+fmt.Sprintf("D%d-0000", docID) {
			if it.SourceChunkID != nil || it.ReviewStatus != ReviewNeedsRevision {
				t.Fatalf("deleted chunk requirement = %+v", it)
			}
			break
		}
	}
	ready, err = d.ExportReadiness(ctx, DefaultTenant, task.ID)
	if err != nil || ready.Counts[BlockSourcePending] != 1 {
		t.Fatalf("export readiness after delete = %+v %v", ready, err)
	}
}

func TestAdoptUnlinkedChunkChangeBumpsSource(t *testing.T) {
	d, task := newProcessingTask(t)
	ctx := context.Background()
	docID, raw := processingDoc(t, d, task.ID, "投标人须具备有效的营业执照。", "质保期不少于 3 年。")
	mustExtractAndConfirm(t, d, task.ID)
	before, _ := d.RequirementManifest(ctx, DefaultTenant, task.ID)
	res, err := d.AdoptTenderParseRevision(ctx, ParseRevisionInput{TenantID: DefaultTenant, TaskID: task.ID, DocID: docID,
		ExpectedRevision: 0, ParserVersion: "test", RawText: raw + "质保期不少于 5 年。\n", PageCount: 1,
		Chunks: revisionChunks("投标人须具备有效的营业执照。", "质保期不少于 5 年。"), Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated != 1 || !res.SourceBumped {
		t.Fatalf("unlinked change = %+v", res)
	}
	after, _ := d.RequirementManifest(ctx, DefaultTenant, task.ID)
	if after.SourceEpoch <= before.SourceEpoch || after.Status != ManifestStale {
		t.Fatalf("unlinked change did not bump source/manifest: %+v -> %+v", before, after)
	}
	if _, err := d.ConfirmRequirements(ctx, DefaultTenant, task.ID, "tester", 0); !errors.Is(err, ErrRequirementReview) {
		t.Fatalf("confirm after unlinked change = %v", err)
	}
	ready, err := d.ExportReadiness(ctx, DefaultTenant, task.ID)
	if err != nil || ready.Counts[BlockSourcePending] != 1 {
		t.Fatalf("export readiness = %+v %v", ready, err)
	}
}

func chunkIDBySeq(t *testing.T, d *DB, docID int64, seq int) int64 {
	t.Helper()
	var id int64
	if err := d.QueryRowContext(context.Background(), `SELECT id FROM document_chunks WHERE doc_id=$1 AND seq=$2`, docID, seq).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAdoptConcurrentExtractionKeepsPending(t *testing.T) {
	d, task := newProcessingTask(t)
	ctx := context.Background()
	docID, raw := processingDoc(t, d, task.ID, "投标人须具备有效的营业执照。")
	mustExtractAndConfirm(t, d, task.ID)

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var captured int64
	if err := tx.QueryRowContext(ctx, `SELECT source_epoch FROM requirement_manifests WHERE tenant_id=$1 AND task_id=$2 FOR UPDATE`,
		DefaultTenant, task.ID).Scan(&captured); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := d.AdoptTenderParseRevision(ctx, ParseRevisionInput{TenantID: DefaultTenant, TaskID: task.ID, DocID: docID,
			ExpectedRevision: 0, ParserVersion: "test", RawText: raw + "投标人须提供驻场人员名单。\n", PageCount: 1,
			Chunks: revisionChunks("投标人须具备有效的营业执照。", "投标人须提供驻场人员名单。"), Actor: "tester"})
		done <- err
	}()
	time.Sleep(150 * time.Millisecond)
	// 模拟提取在变更提交前读取了旧 epoch，并在提交时只写入旧值。
	if _, err := tx.ExecContext(ctx, `UPDATE requirement_manifests SET extracted_source_epoch=$3 WHERE tenant_id=$1 AND task_id=$2`,
		DefaultTenant, task.ID, captured); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	m, err := d.RequirementManifest(ctx, DefaultTenant, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.SourceEpoch <= m.ExtractedSourceEpoch {
		t.Fatalf("concurrent adoption was cleared by stale extraction: %+v", m)
	}
	var snapshotRaw string
	if err := d.QueryRowContext(ctx, `SELECT raw_text FROM tender_parse_revisions WHERE document_id=$1 ORDER BY revision_no DESC LIMIT 1`, docID).Scan(&snapshotRaw); err != nil {
		t.Fatal(err)
	}
	if snapshotRaw != raw {
		t.Fatalf("snapshot raw = %q", snapshotRaw)
	}
}
