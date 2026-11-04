package export

// 导出执行：套版模板登记与读取、生成 docx、经文档引擎（WPS / Word）刷新目录 / 转 PDF、产物落盘与回读自检。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/toolhealth"
)

// ---------------------------------------------------------------------------
// 模板
// ---------------------------------------------------------------------------

// SaveTemplate 校验并登记上传的 .docx 模板。
func (s *Service) SaveTemplate(ctx context.Context, tenant, actor, name, fileName string, data []byte) (*db.ExportTemplate, error) {
	tpl, err := LoadTemplate(name, data)
	if err != nil {
		return nil, err
	}
	meta := tpl.Meta()
	rel := s.Store.templateRel(tenant, tpl.Hash)
	if _, err := s.Store.WriteFile(rel, data); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(meta)
	if strings.TrimSpace(name) == "" {
		name = strings.TrimSuffix(fileName, filepath.Ext(fileName))
	}
	if strings.TrimSpace(name) == "" {
		name = "模板 " + tpl.Hash[:8]
	}
	return s.DB.CreateExportTemplate(ctx, tenant, actor, db.ExportTemplate{Name: name, FileName: fileName, Path: rel, SHA256: tpl.Hash,
		Size: int64(len(data)), Meta: raw})
}

func (s *Service) loadTemplate(ctx context.Context, tenant string, job *db.ExportJob) (*Template, error) {
	if job.TemplateID == nil {
		tpl, err := DefaultTemplate()
		if err == nil && job.TemplateHash != "" && tpl.Hash != job.TemplateHash {
			return nil, fmt.Errorf("built-in template changed since the job was created (hash %s, job expects %s)", tpl.Hash[:12], job.TemplateHash[:min(12, len(job.TemplateHash))])
		}
		return tpl, err
	}
	t, err := s.DB.GetExportTemplate(ctx, tenant, *job.TemplateID)
	if err != nil {
		return nil, err
	}
	data, err := s.Store.ReadFile(t.Path, job.TemplateHash)
	if err != nil {
		return nil, err
	}
	return LoadTemplate(t.Name, data)
}

// ---------------------------------------------------------------------------
// 导出执行
// ---------------------------------------------------------------------------

// permanentError 表示重试无意义的失败（如 PDF 导出时文档引擎不可用、快照数据损坏）；code 是作业错误码。
type permanentError struct {
	code string
	error
}

func permanent(format string, args ...any) error {
	return permanentError{code: db.JobErrExportFailed, error: fmt.Errorf(format, args...)}
}

func permanentCode(code, format string, args ...any) error {
	return permanentError{code: code, error: fmt.Errorf(format, args...)}
}

// OfficeLabel 是文档引擎的执行版本标签（如 "WPS 12.1.0.29161"），严格最终稿在首次提交时固定。
func OfficeLabel(o Office) string {
	if o == nil {
		return ""
	}
	if v := strings.TrimSpace(o.Version()); v != "" {
		return v
	}
	return o.Name()
}

// ArtifactName 是产物的下载文件名。
func ArtifactName(job *db.ExportJob) string {
	name := strings.Map(func(r rune) rune {
		if strings.ContainsRune(`\/:*?"<>|`, r) || r < 0x20 {
			return '_'
		}
		return r
	}, strings.TrimSpace(job.Doc.TaskName))
	if name == "" {
		name = "task" + strconv.FormatInt(job.TaskID, 10)
	}
	return fmt.Sprintf("%s_技术标_v%d_导出%d.%s", truncateRunes(name, 60), job.OutlineVersion, job.ID, job.Format)
}

// Execute 执行一次导出（调用方已持有租约）：生成 docx，经文档引擎（WPS / Word）刷新目录、另存 docx 或导出 PDF，
// 结果写到 <job>/t<token>/ 目录。返回产物与提示；不写数据库。
//
// 严格最终稿（job.StrictRuntime，策略 db.ExportPolicyFinalV1）在执行时重验：文档引擎必须可用且与提交时固定的执行版本一致
// （否则 RUNTIME_UNAVAILABLE，不降级为未刷新目录的 DOCX，也不换用另一个引擎）；快照中的 PNG 须可读且哈希一致，不插入源码
// 占位框；产物回读须有刷新后的目录、全部图片与表格（READBACK_FAILED）。
func (s *Service) Execute(ctx context.Context, tenant string, job *db.ExportJob) (*db.ExportResult, error) {
	reason := s.OfficeReason
	if reason == "" {
		reason = ErrOfficeUnavailable.Error()
	}
	switch {
	case job.StrictRuntime && s.Office == nil:
		return nil, permanentCode(db.JobErrRuntimeUnavailable, "严格最终稿要求文档引擎（WPS / Word）刷新目录，当前不可用：%s；不会降级为未刷新目录的预览稿", reason)
	case job.StrictRuntime && job.Executor != OfficeLabel(s.Office):
		return nil, permanentCode(db.JobErrRuntimeUnavailable, "作业提交时固定的文档引擎为 %q，当前为 %q；执行版本不随配置变化，请以新的请求键重新提交",
			job.Executor, OfficeLabel(s.Office))
	case job.Format == db.ExportPDF && s.Office == nil:
		return nil, permanentCode(db.JobErrRuntimeUnavailable, "文档引擎（WPS / Word）不可用，无法生成 PDF：%s", reason)
	}
	tpl, err := s.loadTemplate(ctx, tenant, job)
	if err != nil {
		return nil, permanent("模板不可用：%v", err)
	}
	revIDs := make([]int64, 0, len(job.Doc.Sections))
	for _, sec := range job.Doc.Sections {
		revIDs = append(revIDs, sec.RevID)
	}
	bodies, err := s.DB.RevisionContents(ctx, tenant, job.TaskID, revIDs)
	if err != nil {
		return nil, err
	}
	images := map[int64][]byte{}
	for _, c := range job.Charts {
		if c.RenderStatus == db.ChartRendered && c.RenderFormat == db.ChartFormatPNG && c.ArtifactPath != "" {
			png, err := s.Store.ReadFile(c.ArtifactPath, c.ArtifactHash)
			if err == nil {
				images[c.ChartID] = png
				continue
			}
			if job.StrictRuntime {
				// 只给原因分类：底层错误含受管目录或宿主路径，不写进作业错误。
				why := "内容哈希与登记不符或不可读"
				if errors.Is(err, fs.ErrNotExist) {
					why = "文件缺失"
				}
				return nil, permanent("图表 #%d《%s》的渲染图%s：最终稿不插入源码占位框，请重新渲染后以新的请求键导出", c.ChartID, c.Title, why)
			}
		} else if job.StrictRuntime && c.SourceFormat != db.ChartSourceJSON {
			return nil, permanent("图表 #%d《%s》在快照中未渲染（%s）：最终稿不插入源码占位框", c.ChartID, c.Title, c.RenderStatus)
		}
	}
	in := BuildInput{Doc: job.Doc, Bodies: bodies, Charts: job.Charts, ChartImages: images, Matrix: job.Matrix,
		Bidder: job.Doc.Bidder, Date: job.Doc.CreatedAt.In(time.Local).Format("2006年01月02日"), UpdateFieldsOnOpen: s.Office == nil}
	built, err := Build(tpl, in)
	if err != nil {
		return nil, permanent("生成 docx 失败：%v", err)
	}
	if job.StrictRuntime {
		for _, w := range built.Warnings {
			if w.ChartID > 0 {
				return nil, permanent("最终稿不允许图表降级：%s", w.Message)
			}
		}
	}
	dirRel := s.Store.jobRel(tenant, job.TaskID, job.ID) + "/t" + strconv.FormatInt(job.LeaseToken, 10)
	dir, err := s.Store.Abs(dirRel)
	if err != nil {
		return nil, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	name := ArtifactName(job)
	res := &db.ExportResult{ArtifactName: name}
	seen := map[string]bool{} // 门禁阶段已记录的图表提示不重复
	for _, w := range job.Warnings {
		seen[w.Kind+"/"+strconv.FormatInt(w.ChartID, 10)] = w.ChartID > 0
	}
	for _, w := range built.Warnings {
		if !seen[w.Kind+"/"+strconv.FormatInt(w.ChartID, 10)] {
			res.Warnings = append(res.Warnings, w)
		}
	}
	work := filepath.Join(dir, ".work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return nil, err
	}
	defer removeAllRetry(work)
	var final []byte
	if s.Office == nil {
		final = built.Data
		res.Warnings = append(res.Warnings, db.ExportWarning{Kind: db.WarnTOCNotRefreshed,
			Message: "目录页码未刷新：文档引擎（WPS / Word）不可用（" + s.OfficeReason + "），目录仅含占位文字，已设置打开时更新域"})
	} else {
		draft := filepath.Join(work, "draft.docx")
		if err := os.WriteFile(draft, built.Data, 0o644); err != nil {
			return nil, err
		}
		outDocx, outPDF := filepath.Join(work, "refreshed.docx"), ""
		if job.Format == db.ExportPDF {
			outDocx, outPDF = "", filepath.Join(work, "out.pdf")
		}
		if _, err := s.Office.Refresh(ctx, draft, outDocx, outPDF); err != nil {
			if ctx.Err() == nil { // 取消 / 租约丢失不算引擎故障
				s.Health.Degrade(toolhealth.Office, err)
			}
			return nil, err
		}
		s.Health.Recover(toolhealth.Office)
		out := outDocx
		if outPDF != "" {
			out = outPDF
		}
		if final, err = os.ReadFile(out); err != nil {
			return nil, err
		}
		res.TOCRefreshed, res.OfficeEngine, res.OfficeVersion = true, s.Office.Name(), s.Office.Version()
	}
	warns, rerr := readback(ctx, job, final, tpl.Geometry, res.TOCRefreshed)
	if rerr != nil {
		return nil, rerr
	}
	res.Warnings = append(res.Warnings, warns...)
	rel := dirRel + "/" + safeFileName(name)
	sum, err := s.Store.WriteFile(rel, final)
	if err != nil {
		return nil, err
	}
	res.ArtifactPath, res.ArtifactHash, res.ArtifactSize = rel, sum, int64(len(final))
	return res, nil
}

// readback 用 ingest 解析器回读产物：PDF 核对目录页码与版式，刷新过的 DOCX 核对目录域缓存结果，问题记为提示，不改变成败。
// 严格最终稿另行要求目录结构问题为零（目录存在、带页码、与标题实际页一致），且图片与表格数量不少于快照，否则返回
// READBACK_FAILED（重试无意义）。
func readback(ctx context.Context, job *db.ExportJob, data []byte, g Geometry, refreshed bool) ([]db.ExportWarning, error) {
	var issues []Issue
	var images, tables int
	var err error
	switch {
	case job.Format == db.ExportPDF:
		var r *PDFReport
		if r, err = CheckPDF(data, g); err == nil {
			issues, images, tables = r.Issues, r.Images, -1
		}
	case refreshed || job.StrictRuntime:
		var r *DOCXReport
		if r, err = CheckDOCX(ctx, data); err == nil {
			issues, tables = r.Issues, len(r.Tables)
			images, err = DOCXBodyImages(data)
		}
	}
	if err != nil {
		if job.StrictRuntime {
			return nil, permanentCode(db.JobErrReadbackFailed, "最终稿回读失败：%v", err)
		}
		return []db.ExportWarning{{Kind: db.WarnSelfCheck, Message: strings.ToUpper(job.Format) + " 自检回读失败：" + err.Error()}}, nil
	}
	if job.StrictRuntime {
		var bad []string
		for _, is := range issues {
			if tocIssue(is.Kind) {
				bad = append(bad, is.Detail)
			}
		}
		wantImages, wantTables := expectedContent(job)
		if images < wantImages {
			bad = append(bad, fmt.Sprintf("回读到 %d 张图片，快照有 %d 张渲染图", images, wantImages))
		}
		if tables >= 0 && tables < wantTables {
			bad = append(bad, fmt.Sprintf("回读到 %d 个表格，快照至少有 %d 个（设备选型表与偏离表）", tables, wantTables))
		}
		if len(bad) > 0 {
			if len(bad) > 5 {
				bad = append(bad[:5], fmt.Sprintf("另有 %d 项", len(bad)-5))
			}
			return nil, permanentCode(db.JobErrReadbackFailed, "最终稿回读未通过：%s", strings.Join(bad, "；"))
		}
	}
	var out []db.ExportWarning
	for i, is := range issues {
		if i == 20 {
			out = append(out, db.ExportWarning{Kind: db.WarnSelfCheck, Message: fmt.Sprintf("自检：另有 %d 项问题未列出", len(issues)-20)})
			break
		}
		msg := "自检：" + is.Detail
		if is.Page > 0 {
			msg = fmt.Sprintf("自检（第 %d 页）：%s", is.Page, is.Detail)
		}
		out = append(out, db.ExportWarning{Kind: db.WarnSelfCheck, Message: msg})
	}
	return out, nil
}

// tocIssue 报告问题是否属于目录结构（严格最终稿不容许）；版式类问题只记为提示。
func tocIssue(kind string) bool {
	switch kind {
	case IssueTOCMissing, IssueTOCPage, IssueHeadingMissing, IssueTOCPlaceholder, IssueTOCEntryNoPage, IssueTOCFieldMissing:
		return true
	}
	return false
}

// expectedContent 返回快照要求成稿至少包含的图片数（PNG 渲染图）与表格数（设备选型表 + 偏离表）。
func expectedContent(job *db.ExportJob) (images, tables int) {
	for _, c := range job.Charts {
		switch {
		case c.SourceFormat == db.ChartSourceJSON:
			tables++
		case c.RenderStatus == db.ChartRendered && c.RenderFormat == db.ChartFormatPNG:
			images++
		}
	}
	if len(job.Matrix) > 0 {
		tables++
	}
	return images, tables
}

// safeFileName 生成磁盘文件名（下载文件名单独保存，可含中文）。
func safeFileName(name string) string {
	ext := filepath.Ext(name)
	return "artifact" + ext
}

// cleanupOtherAttempts 删除同一任务其他令牌（被回收的旧执行）的目录。
func (s *Service) cleanupOtherAttempts(tenant string, job *db.ExportJob, keepToken int64) {
	dir, err := s.Store.Abs(s.Store.jobRel(tenant, job.TaskID, job.ID))
	if err != nil {
		return
	}
	entries, _ := os.ReadDir(dir)
	keep := "t" + strconv.FormatInt(keepToken, 10)
	for _, e := range entries {
		if e.Name() != keep {
			removeAllRetry(filepath.Join(dir, e.Name()))
		}
	}
}

// ArtifactPath 返回成功任务产物的绝对路径。
func (s *Service) ArtifactPath(job *db.ExportJob) (string, error) {
	if job.Status != db.ExportSucceeded || job.ArtifactPath == "" {
		return "", db.ErrExportNotReady
	}
	return s.Store.Abs(job.ArtifactPath)
}

func (s *Service) removeAttempt(tenant string, job *db.ExportJob) {
	if dir, err := s.Store.Abs(s.Store.jobRel(tenant, job.TaskID, job.ID) + "/t" + strconv.FormatInt(job.LeaseToken, 10)); err == nil {
		removeAllRetry(dir)
	}
}
