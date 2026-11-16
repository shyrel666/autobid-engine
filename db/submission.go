package db

// D4（成稿与提交）：提交检查清单（白手套清单），按导出请求组持久化。
//
// 一份标书能不能交出去，除了导出门禁，还有两类检查：
//   - 系统可查项（auto）：由请求组与各格式作业的不可变结果判定——是否全部为严格最终稿、成稿内容是否仍是当前批准的修订、
//     目录页码、评分索引页码、版式、页码、文档属性、模板、封面投标人。不落库，每次读取时从 export_jobs.self_check 等重算；
//   - 人工确认项（manual）：系统看不到的事——封面核对、签字盖章 / 电子签章、证明材料原件附入、在目标交易平台的投标文件
//     制作工具中导入并通过检查、加密上传与回执、技术标不含报价。确认人、时间、说明与凭证编号落库（submission_checks），
//     每次确认、改判与撤回另记一条不可修改的事件（submission_check_events）。
// 状态为 attention 的系统项（如使用内置模板、版式自检有提示）不能自动放行，须由人写明理由后确认知悉。
//
// 人工确认只能针对最终稿、且成稿内容仍是当前批准修订的请求组：预览稿不能当作提交文件，导出之后正文又变了的成稿也不能。
// 确认记录绑定当时的产物哈希（artifact_hashes）；作业终态之后产物哈希不可修改（schema 触发器），所以确认始终对应同一份文件。
// 清单只判定与留痕，不代表已在交易平台提交；确认人是服务端核验过的登录账号（R2）。
//
// 锁序：请求组行（FOR UPDATE，与同一请求组的并发确认串行化）→ submission_checks。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SubmissionChecklistVersion 是清单条目集合的版本；增删条目或改变判定规则时升级。
const SubmissionChecklistVersion = "white-glove/v1"

// 清单条目类型与状态。
const (
	CheckAuto   = "auto"
	CheckManual = "manual"

	CheckPass          = "pass"           // 系统项：通过
	CheckFail          = "fail"           // 系统项：不通过（须修复后重新导出，不能人工放行）
	CheckAttention     = "attention"      // 系统项：需要人确认知悉
	CheckPending       = "pending"        // 人工项：尚未确认
	CheckConfirmed     = "confirmed"      // 人工项 / attention 项：已确认
	CheckFailed        = "failed"         // 人工项：检查未通过
	CheckNotApplicable = "not_applicable" // 人工项：不适用（须写明原因）
)

var (
	ErrSubmissionNotFinal     = errors.New("submission: the export request is not a current final draft")
	ErrSubmissionCheckInvalid = errors.New("submission: invalid checklist confirmation")
)

// SubmissionItem 是清单中的一项。
type SubmissionItem struct {
	Key    string `json:"key"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Help   string `json:"help,omitempty"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Confirmable：可以写入人工确认（人工项，或状态为 attention 的系统项）。
	Confirmable bool       `json:"confirmable"`
	Note        string     `json:"note,omitempty"`
	Reference   string     `json:"reference,omitempty"`
	ConfirmedBy string     `json:"confirmed_by,omitempty"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	Version     int        `json:"version,omitempty"`
}

// SubmissionChecklist 是一个导出请求组的提交检查清单。
type SubmissionChecklist struct {
	TaskID        int64  `json:"task_id"`
	RequestID     int64  `json:"export_request_id"`
	Version       string `json:"checklist_version"`
	BusinessState string `json:"business_state"`
	// Final：请求组全部格式都是严格最终稿。Current：成稿锁定的修订仍是批准大纲下各章节当前批准的修订。
	Final          bool              `json:"final"`
	Current        bool              `json:"current"`
	StaleReason    string            `json:"stale_reason,omitempty"`
	ArtifactHashes map[string]string `json:"artifact_hashes"`
	Items          []SubmissionItem  `json:"items"`
	// Ready：是当前最终稿，系统项无不通过，attention 项均已确认知悉，人工项均已确认或标为不适用。
	Ready bool `json:"ready"`
	Open  int  `json:"open"` // 阻止 Ready 的条目数
}

// SubmissionCheckInput 是一次人工确认。Status 为空表示撤回已有确认；ExpectedVersion > 0 时须与现有记录版本一致。
type SubmissionCheckInput struct {
	Status          string `json:"status"`
	Note            string `json:"note"`
	Reference       string `json:"reference"`
	ExpectedVersion int    `json:"expected_version"`
}

type manualCheck struct{ key, label, help string }

// manualChecks 是 white-glove/v1 的人工确认项（顺序即展示顺序）。
var manualChecks = []manualCheck{
	{"cover_check", "封面与基本信息核对", "封面的项目名称、招标编号、分包号、投标人名称与日期同招标文件及营业执照一致。"},
	{"price_isolation", "技术标不含报价", "招标文件要求技术标不得出现投标报价时，通读正文、表格与附录确认没有价格信息。"},
	{"evidence_attached", "证明材料原件已附入", "按《证明材料索引》的顺序附入原件扫描件，逐件核对与索引的名称、编号、页码一致且清晰可辨。"},
	{"signature_seal", "签字盖章 / 电子签章", "按招标文件要求完成法定代表人或授权代表签字、单位公章或 CA 电子签章，授权委托书在有效期内。"},
	{"platform_import", "交易平台制作工具导入检查", "在目标交易平台的投标文件制作工具中导入成稿，逐项通过工具的格式与完整性检查。凭证栏填写平台名称与工具版本。"},
	{"platform_upload", "加密上传与回执", "在投标截止时间之前完成加密上传并取得回执。凭证栏填写回执编号或上传时间。"},
}

func (d *DB) loadSubmissionRequest(ctx context.Context, tenant string, taskID, requestID int64, lock bool) (*ExportRequest, error) {
	q := `SELECT ` + exportRequestCols + ` FROM export_requests WHERE tenant_id = $1 AND task_id = $2 AND id = $3`
	if lock {
		q += ` FOR UPDATE`
	}
	req, err := scanExportRequest(d.QueryRowContext(ctx, q, tenant, taskID, requestID))
	if err != nil {
		return nil, err
	}
	if req.Jobs, err = listRequestJobsTx(ctx, d, tenant, taskID, requestID); err != nil {
		return nil, err
	}
	req.summarize()
	return req, nil
}

// snapshotCurrent 判断成稿锁定的修订是否仍是批准大纲下各章节当前批准的修订。
func (d *DB) snapshotCurrent(ctx context.Context, tenant string, req *ExportRequest) (bool, string, error) {
	if len(req.Jobs) == 0 {
		return false, "请求组没有导出作业", nil
	}
	var status string
	err := d.QueryRowContext(ctx, `SELECT status FROM proposal_outlines WHERE tenant_id = $1 AND task_id = $2 AND id = $3`,
		tenant, req.TaskID, req.OutlineID).Scan(&status)
	if err != nil {
		return false, "", err
	}
	if status != OutlineApproved {
		return false, fmt.Sprintf("成稿依据的大纲 v%d 已不是批准状态（%s）", req.OutlineVersion, status), nil
	}
	rows, err := d.QueryContext(ctx, `SELECT id, section_code, approved_rev_no FROM proposal_sections
		WHERE tenant_id = $1 AND outline_id = $2 ORDER BY seq`, tenant, req.OutlineID)
	if err != nil {
		return false, "", err
	}
	defer rows.Close()
	snap := req.Jobs[0].RevSnapshot
	n := 0
	for rows.Next() {
		var id int64
		var code string
		var approved sql.NullInt32
		if err := rows.Scan(&id, &code, &approved); err != nil {
			return false, "", err
		}
		n++
		locked, ok := snap[strconv.FormatInt(id, 10)]
		switch {
		case !ok:
			return false, fmt.Sprintf("章节 %s 不在成稿快照中", code), rows.Err()
		case !approved.Valid:
			return false, fmt.Sprintf("章节 %s 导出后被修改或撤销批准，成稿锁定的是 rev %d", code, locked), rows.Err()
		case int(approved.Int32) != locked:
			return false, fmt.Sprintf("章节 %s 当前批准的是 rev %d，成稿锁定的是 rev %d", code, approved.Int32, locked), rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return false, "", err
	}
	if n != len(snap) {
		return false, "批准大纲的章节集合与成稿快照不一致", nil
	}
	return true, "", nil
}

// autoChecks 由请求组与作业的不可变结果判定系统可查项。
func autoChecks(req *ExportRequest, final, current bool, staleReason string) []SubmissionItem {
	item := func(key, label, help, status, detail string) SubmissionItem {
		return SubmissionItem{Key: key, Kind: CheckAuto, Label: label, Help: help, Status: status, Detail: detail, Confirmable: status == CheckAttention}
	}
	var out []SubmissionItem
	// 1. 最终稿。
	if final {
		engines := map[string]bool{}
		for _, j := range req.Jobs {
			engines[strings.TrimSpace(j.OfficeEngine+" "+j.OfficeVersion)] = true
		}
		var names []string
		for e := range engines {
			names = append(names, e)
		}
		out = append(out, item("final_draft", "严格最终稿", "全部格式均经文档引擎刷新目录并通过回读。", CheckPass,
			fmt.Sprintf("%d 种格式均为最终稿（%s）", len(req.Jobs), strings.Join(names, "、"))))
	} else {
		detail := "预览稿不能作为提交文件，请以最终稿重新导出"
		if req.StrictRuntime {
			detail = fmt.Sprintf("请求组状态为 %s，尚未全部成功", req.Status)
		}
		out = append(out, item("final_draft", "严格最终稿", "全部格式均经文档引擎刷新目录并通过回读。", CheckFail, detail))
	}
	// 2. 内容仍是当前批准修订。
	if current {
		out = append(out, item("current_content", "成稿内容为当前批准修订", "导出之后章节没有再被修改、撤销批准或换用新大纲。", CheckPass, ""))
	} else {
		out = append(out, item("current_content", "成稿内容为当前批准修订", "导出之后章节没有再被修改、撤销批准或换用新大纲。", CheckFail,
			staleReason+"；请重新导出"))
	}
	if len(req.Jobs) == 0 {
		return out
	}
	first := req.Jobs[0]
	// 3. 封面投标人。
	if strings.TrimSpace(first.Doc.Bidder) != "" {
		out = append(out, item("bidder_name", "封面投标人名称", "封面与文档属性使用提交导出时填写的投标人名称。", CheckPass, first.Doc.Bidder))
	} else {
		out = append(out, item("bidder_name", "封面投标人名称", "封面与文档属性使用提交导出时填写的投标人名称。", CheckFail,
			"导出时没有填写投标人名称，封面为空；请填写后重新导出"))
	}
	// 4~8. 各格式作业的回读自检：任一格式不满足即不通过。
	type agg struct {
		status string
		detail []string
	}
	worse := func(a *agg, status, detail string) {
		rank := map[string]int{"": 0, CheckPass: 1, CheckAttention: 2, CheckFail: 3}
		if rank[status] > rank[a.status] {
			a.status = status
		}
		if detail != "" {
			a.detail = append(a.detail, detail)
		}
	}
	var toc, index, layout, pages, meta agg
	for _, j := range req.Jobs {
		f := strings.ToUpper(j.Format)
		sc := j.SelfCheck
		if j.Status != ExportSucceeded || sc == nil {
			why := f + " 没有回读自检记录"
			if j.Status != ExportSucceeded {
				why = f + " 未成功（" + j.Status + "）"
			}
			for _, a := range []*agg{&toc, &index, &layout, &pages, &meta} {
				worse(a, CheckFail, why)
			}
			continue
		}
		if sc.TOCIssues == 0 && sc.TOCEntries > 0 {
			worse(&toc, CheckPass, fmt.Sprintf("%s 目录 %d 条", f, sc.TOCEntries))
		} else {
			worse(&toc, CheckFail, fmt.Sprintf("%s 目录问题 %d 项", f, sc.TOCIssues))
		}
		switch {
		case len(j.Package.ScoreIndex) == 0:
			worse(&index, CheckAttention, "没有评分项映射到章节，成稿没有评分索引表")
		case sc.PageRefs > 0 && sc.PageRefIssues == 0 && sc.PageRefsVerified >= sc.PageRefs && sc.VerifiedAgainst == "pdf":
			worse(&index, CheckPass, fmt.Sprintf("%s %d 个评分项、%d 处页码与标题所在页一致", f, sc.ScoreRows, sc.PageRefsVerified))
		default:
			worse(&index, CheckFail, fmt.Sprintf("%s 页码引用 %d 处，核对一致 %d 处，问题 %d 项", f, sc.PageRefs, sc.PageRefsVerified, sc.PageRefIssues))
		}
		switch {
		case !sc.LayoutChecked:
			worse(&layout, CheckFail, f+" 没有版面可核对")
			worse(&pages, CheckFail, f+" 没有版面可核对")
		default:
			if n := sc.OutOfBounds + sc.ImageIssues; n > 0 {
				worse(&layout, CheckAttention, fmt.Sprintf("%s 版式提示 %d 项（越出版芯 %d、图片重叠或压字 %d），见该作业的自检提示", f, n, sc.OutOfBounds, sc.ImageIssues))
			} else {
				worse(&layout, CheckPass, fmt.Sprintf("%s 共 %d 页", f, sc.Pages))
			}
			if n := sc.FooterIssues + sc.BlankPages; n > 0 {
				worse(&pages, CheckAttention, fmt.Sprintf("%s 页码不连续 %d 处、空白页 %d 页", f, sc.FooterIssues, sc.BlankPages))
			} else {
				worse(&pages, CheckPass, "")
			}
		}
		if sc.MetadataChecked && sc.MetadataIssues == 0 {
			worse(&meta, CheckPass, fmt.Sprintf("%s 作者 %q", f, sc.Author))
		} else {
			worse(&meta, CheckFail, fmt.Sprintf("%s 文档属性问题 %d 项", f, sc.MetadataIssues))
		}
	}
	join := func(a agg) string { return strings.Join(a.detail, "；") }
	out = append(out,
		item("toc_pages", "目录页码", "目录每个条目的页码与标题实际所在页一致。", toc.status, join(toc)),
		item("score_index", "评分索引页码", "《评分索引表》、偏离表与附录中的章节页码与标题实际所在页一致（以同一次刷新的 PDF 版面为准）。", index.status, join(index)),
		item("layout", "版式", "正文与图片不越出版芯，图片不重叠、不压字。", layout.status, join(layout)),
		item("page_numbers", "页码与空白页", "页脚页码与物理页序一致，没有只含页眉页脚的空白页。", pages.status, join(pages)),
		item("metadata", "文档属性", "作者等属性只含投标人名称，不带系统字样与本机账号。", meta.status, join(meta)))
	// 9. 模板。
	if req.TemplateID != nil {
		out = append(out, item("own_template", "企业自有模板", "使用本企业上传的版式模板。", CheckPass, req.TemplateKey))
	} else {
		out = append(out, item("own_template", "企业自有模板", "使用本企业上传的版式模板。", CheckAttention,
			"使用的是系统内置模板：版式与其他使用本系统的投标人相同，存在被认定为文件异常一致的风险。建议上传本企业模板后重新导出，或写明理由确认接受"))
	}
	return out
}

func (d *DB) submissionChecklist(ctx context.Context, tenant string, taskID, requestID int64, lock bool) (*SubmissionChecklist, *ExportRequest, error) {
	req, err := d.loadSubmissionRequest(ctx, tenant, taskID, requestID, lock)
	if err != nil {
		return nil, nil, err
	}
	cl := &SubmissionChecklist{TaskID: taskID, RequestID: requestID, Version: SubmissionChecklistVersion, BusinessState: req.BusinessState,
		Final: req.BusinessState == ExportFinalReady, ArtifactHashes: map[string]string{}, Items: []SubmissionItem{}}
	for _, j := range req.Jobs {
		if j.Status == ExportSucceeded {
			cl.ArtifactHashes[j.Format] = j.ArtifactHash
		}
	}
	if cl.Current, cl.StaleReason, err = d.snapshotCurrent(ctx, tenant, req); err != nil {
		return nil, nil, err
	}
	cl.Items = autoChecks(req, cl.Final, cl.Current, cl.StaleReason)
	for _, m := range manualChecks {
		if m.key == "evidence_attached" && (len(req.Jobs) == 0 || len(req.Jobs[0].Package.Evidence) == 0) {
			continue // 成稿没有引用企业证据
		}
		cl.Items = append(cl.Items, SubmissionItem{Key: m.key, Kind: CheckManual, Label: m.label, Help: m.help, Status: CheckPending, Confirmable: true})
	}
	rows, err := d.QueryContext(ctx, `SELECT item_key, status, note, reference, confirmed_by, confirmed_at, version FROM submission_checks
		WHERE tenant_id = $1 AND task_id = $2 AND export_request_id = $3`, tenant, taskID, requestID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	type rec struct {
		status, note, ref, by string
		at                    time.Time
		version               int
	}
	recs := map[string]rec{}
	for rows.Next() {
		var k string
		var r rec
		if err := rows.Scan(&k, &r.status, &r.note, &r.ref, &r.by, &r.at, &r.version); err != nil {
			return nil, nil, err
		}
		recs[k] = r
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for i := range cl.Items {
		it := &cl.Items[i]
		r, ok := recs[it.Key]
		if !ok || !it.Confirmable {
			continue // 系统项恢复为通过 / 不通过后，旧的知悉记录不再影响判定
		}
		at := r.at
		it.Status, it.Note, it.Reference, it.ConfirmedBy, it.ConfirmedAt, it.Version = r.status, r.note, r.ref, r.by, &at, r.version
	}
	for _, it := range cl.Items {
		switch it.Status {
		case CheckPass, CheckConfirmed, CheckNotApplicable:
		default:
			cl.Open++
		}
	}
	cl.Ready = cl.Final && cl.Current && cl.Open == 0
	return cl, req, nil
}

// SubmissionChecklist 返回导出请求组的提交检查清单（只读）。
func (d *DB) SubmissionChecklist(ctx context.Context, tenant string, taskID, requestID int64) (*SubmissionChecklist, error) {
	cl, _, err := d.submissionChecklist(ctx, tenantOr(tenant), taskID, requestID, false)
	return cl, err
}

// SetSubmissionCheck 写入、改判或撤回一项人工确认，返回更新后的清单。只接受人工项与状态为 attention 的系统项；
// 请求组必须是当前最终稿（否则 ErrSubmissionNotFinal）。宿主绑定 Intent 的执行不能确认：这是人的职责。
func (d *DB) SetSubmissionCheck(ctx context.Context, tenant string, taskID, requestID int64, key string, in SubmissionCheckInput, actor string) (*SubmissionChecklist, error) {
	tenant = tenantOr(tenant)
	if _, bound := intentExecutionFrom(ctx); bound {
		return nil, fmt.Errorf("%w: checklist confirmations are a human action", ErrSubmissionCheckInvalid)
	}
	key, actor = strings.TrimSpace(key), strings.TrimSpace(actor)
	in.Status, in.Note, in.Reference = strings.TrimSpace(in.Status), strings.TrimSpace(in.Note), strings.TrimSpace(in.Reference)
	switch {
	case actor == "":
		return nil, fmt.Errorf("%w: the operator name is required", ErrSubmissionCheckInvalid)
	case len([]rune(in.Note)) > 2000 || len([]rune(in.Reference)) > 500:
		return nil, fmt.Errorf("%w: note or reference is too long", ErrSubmissionCheckInvalid)
	}
	switch in.Status {
	case "", CheckConfirmed:
	case CheckFailed, CheckNotApplicable:
		if in.Note == "" {
			return nil, fmt.Errorf("%w: a note is required for %s", ErrSubmissionCheckInvalid, in.Status)
		}
	default:
		return nil, fmt.Errorf("%w: unknown status %q", ErrSubmissionCheckInvalid, in.Status)
	}
	var out *SubmissionChecklist
	err := d.Transaction(ctx, func(tx *DB) error {
		cl, _, err := tx.submissionChecklist(ctx, tenant, taskID, requestID, true)
		if err != nil {
			return err
		}
		var item *SubmissionItem
		for i := range cl.Items {
			if cl.Items[i].Key == key {
				item = &cl.Items[i]
			}
		}
		switch {
		case item == nil:
			return fmt.Errorf("%w: unknown item %q", ErrSubmissionCheckInvalid, key)
		case !item.Confirmable:
			return fmt.Errorf("%w: item %q is decided by the system (%s) and cannot be confirmed by hand", ErrSubmissionCheckInvalid, key, item.Status)
		case item.Kind == CheckAuto && in.Status != "" && (in.Status != CheckConfirmed || in.Note == ""):
			return fmt.Errorf("%w: an attention item can only be acknowledged (confirmed) with a note", ErrSubmissionCheckInvalid)
		case in.ExpectedVersion > 0 && in.ExpectedVersion != item.Version:
			return fmt.Errorf("%w: item %q is at version %d", ErrRevisionConflict, key, item.Version)
		}
		hashes, _ := json.Marshal(cl.ArtifactHashes)
		if in.Status == "" { // 撤回：不要求仍是当前最终稿
			res, err := tx.ExecContext(ctx, `DELETE FROM submission_checks WHERE tenant_id = $1 AND task_id = $2 AND export_request_id = $3 AND item_key = $4`,
				tenant, taskID, requestID, key)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return fmt.Errorf("%w: item %q has no confirmation to withdraw", ErrSubmissionCheckInvalid, key)
			}
		} else {
			if !cl.Final || !cl.Current {
				why := "the request is not a final draft"
				if cl.Final {
					why = cl.StaleReason
				}
				return fmt.Errorf("%w: %s", ErrSubmissionNotFinal, why)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO submission_checks(tenant_id, task_id, export_request_id, item_key, checklist_version, status,
					note, reference, artifact_hashes, confirmed_by, created_by, updated_by)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$10,$10)
				ON CONFLICT (tenant_id, task_id, export_request_id, item_key) DO UPDATE SET status = EXCLUDED.status, note = EXCLUDED.note,
					reference = EXCLUDED.reference, artifact_hashes = EXCLUDED.artifact_hashes, confirmed_by = EXCLUDED.confirmed_by,
					confirmed_at = now(), version = submission_checks.version + 1, updated_by = EXCLUDED.updated_by`,
				tenant, taskID, requestID, key, SubmissionChecklistVersion, in.Status, in.Note, in.Reference, string(hashes), actor); err != nil {
				return err
			}
		}
		action := in.Status
		if action == "" {
			action = "withdrawn"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO submission_check_events(tenant_id, task_id, export_request_id, item_key, action, note, reference,
				artifact_hashes, created_by, updated_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$9)`,
			tenant, taskID, requestID, key, action, in.Note, in.Reference, string(hashes), actor); err != nil {
			return err
		}
		out, _, err = tx.submissionChecklist(ctx, tenant, taskID, requestID, false)
		return err
	})
	return out, err
}

// SubmissionCheckEvent 是清单确认的一条审计记录。
type SubmissionCheckEvent struct {
	ID        int64     `json:"id"`
	ItemKey   string    `json:"item_key"`
	Action    string    `json:"action"`
	Note      string    `json:"note"`
	Reference string    `json:"reference"`
	Actor     string    `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
}

// ListSubmissionCheckEvents 返回请求组的清单确认审计记录（新者在前）。
func (d *DB) ListSubmissionCheckEvents(ctx context.Context, tenant string, taskID, requestID int64, limit int) ([]*SubmissionCheckEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.QueryContext(ctx, `SELECT id, item_key, action, note, reference, created_by, created_at FROM submission_check_events
		WHERE tenant_id = $1 AND task_id = $2 AND export_request_id = $3 ORDER BY id DESC LIMIT $4`, tenantOr(tenant), taskID, requestID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SubmissionCheckEvent{}
	for rows.Next() {
		var e SubmissionCheckEvent
		if err := rows.Scan(&e.ID, &e.ItemKey, &e.Action, &e.Note, &e.Reference, &e.Actor, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}
