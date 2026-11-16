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
	"github.com/autobid/autobid-engine/ingest"
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
// 严格最终稿（job.StrictRuntime）在执行时重验：文档引擎必须可用且与提交时固定的执行版本一致
// （否则 RUNTIME_UNAVAILABLE，不降级为未刷新目录的 DOCX，也不换用另一个引擎）；快照中的 PNG 须可读且哈希一致，不插入源码
// 占位框；产物回读须有刷新后的目录、全部图片与表格，章节页码引用（评分索引表 / 偏离表 / 附录）与标题实际所在页一致，
// 文档属性不带平台字样与无关作者（READBACK_FAILED）。
//
// D4：同一次引擎会话同时另存 DOCX 与导出 PDF——PDF 作业借 DOCX 读出页码引用的缓存结果，严格 DOCX 作业借 PDF 版面取得
// 标题实际所在页；另一种格式只用于核对，不作为产物。引擎另存的 DOCX 再经 SanitizeDOCX 清洗文档属性后才落盘。
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
	meta := DocMeta{Title: job.Doc.TaskName, Author: job.Doc.Bidder, At: job.Doc.CreatedAt}
	in := BuildInput{Doc: job.Doc, Bodies: bodies, Charts: job.Charts, ChartImages: images, Matrix: job.Matrix, Package: job.Package,
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
	var final, companion []byte // companion：同一次刷新的另一种格式，只用于核对页码
	if s.Office == nil {
		final = built.Data
		res.Warnings = append(res.Warnings, db.ExportWarning{Kind: db.WarnTOCNotRefreshed,
			Message: "目录页码未刷新：文档引擎（WPS / Word）不可用（" + s.OfficeReason + "），目录仅含占位文字，已设置打开时更新域"})
	} else {
		draft := filepath.Join(work, "draft.docx")
		if err := os.WriteFile(draft, built.Data, 0o644); err != nil {
			return nil, err
		}
		outDocx, outPDF := filepath.Join(work, "refreshed.docx"), filepath.Join(work, "out.pdf")
		switch {
		case job.Format == db.ExportPDF && built.PageRefs == 0:
			outDocx = ""
		case job.Format == db.ExportDOCX && !job.StrictRuntime:
			outPDF = ""
		}
		if _, err := s.Office.Refresh(ctx, draft, outDocx, outPDF); err != nil {
			if ctx.Err() == nil { // 取消 / 租约丢失不算引擎故障
				s.Health.Degrade(toolhealth.Office, err)
			}
			return nil, err
		}
		s.Health.Recover(toolhealth.Office)
		read := func(p string) ([]byte, error) {
			if p == "" {
				return nil, nil
			}
			return os.ReadFile(p)
		}
		docx, err := read(outDocx)
		if err != nil {
			return nil, err
		}
		if docx != nil {
			// 引擎另存时写入本机账号等属性：落盘前清洗，回读核对的是清洗后的文件。
			if docx, err = SanitizeDOCX(docx, meta); err != nil {
				return nil, permanent("清洗文档属性失败：%v", err)
			}
		}
		pdf, err := read(outPDF)
		if err != nil {
			return nil, err
		}
		final, companion = docx, pdf
		if job.Format == db.ExportPDF {
			final, companion = pdf, docx
		}
		res.TOCRefreshed, res.OfficeEngine, res.OfficeVersion = true, s.Office.Name(), s.Office.Version()
	}
	warns, check, rerr := readback(ctx, job, final, companion, tpl.Geometry, built, res.TOCRefreshed)
	if rerr != nil {
		return nil, rerr
	}
	res.Warnings = append(res.Warnings, warns...)
	res.SelfCheck = check
	rel := dirRel + "/" + safeFileName(name)
	sum, err := s.Store.WriteFile(rel, final)
	if err != nil {
		return nil, err
	}
	res.ArtifactPath, res.ArtifactHash, res.ArtifactSize = rel, sum, int64(len(final))
	return res, nil
}

// readback 用 ingest 解析器回读产物并返回提示与结构化自检结果（db.ExportSelfCheck，随成功结果写入作业）：
//   - PDF：目录页码、版式；DOCX（刷新过或严格）：目录域缓存结果、表格与图片数；
//   - companion 是同一次刷新导出的另一种格式：PDF 作业用它（DOCX）读出章节页码引用的缓存结果，DOCX 作业用它（PDF）取得
//     标题实际所在页与版式；没有 PDF 时页码引用只能与目录中同名条目比对；
//   - 文档属性：作者类字段只能是投标人或空，不带平台字样。
//
// 问题记为提示，不改变成败。严格最终稿另行要求目录结构、章节页码引用与文档属性问题为零，且图片与表格数量不少于快照，
// 否则返回 READBACK_FAILED（重试无意义）。
func readback(ctx context.Context, job *db.ExportJob, data, companion []byte, g Geometry, built *BuildResult, refreshed bool) ([]db.ExportWarning, *db.ExportSelfCheck, error) {
	check := &db.ExportSelfCheck{ScoreRows: built.ScoreRows, PageRefs: built.PageRefs}
	var issues []Issue
	images, tables := 0, -1
	fail := func(err error) ([]db.ExportWarning, *db.ExportSelfCheck, error) {
		if job.StrictRuntime {
			return nil, nil, permanentCode(db.JobErrReadbackFailed, "最终稿回读失败：%v", err)
		}
		return []db.ExportWarning{{Kind: db.WarnSelfCheck, Message: strings.ToUpper(job.Format) + " 自检回读失败：" + err.Error()}}, nil, nil
	}
	docx, pdf := data, companion
	if job.Format == db.ExportPDF {
		docx, pdf = companion, data
	}
	var pdfReport *PDFReport
	if pdf != nil {
		r, err := CheckPDF(pdf, g)
		if err != nil {
			return fail(err)
		}
		pdfReport = r
		check.Pages, check.TOCEntries, check.LayoutChecked = r.Pages, len(r.TOC), true
		for _, is := range r.Issues {
			switch is.Kind {
			case IssueOutOfBounds:
				check.OutOfBounds++
			case IssueBlankPage:
				check.BlankPages++
			case IssueFooterPage:
				check.FooterIssues++
			case IssueImageOverlap, IssueImageOverText:
				check.ImageIssues++
			}
		}
		issues = append(issues, r.Issues...)
		images = r.Images
	}
	var toc []ingest.TOCEntry
	if docx != nil && (refreshed || job.StrictRuntime) {
		r, err := CheckDOCX(ctx, docx)
		if err != nil {
			return fail(err)
		}
		toc = r.TOC
		if job.Format == db.ExportDOCX {
			issues = append(issues, r.Issues...)
			tables = len(r.Tables)
			if images, err = DOCXBodyImages(docx); err != nil {
				return fail(err)
			}
			if pdfReport == nil {
				check.TOCEntries = len(r.TOC)
			}
		}
	}
	// 章节页码引用：生成时写入了引用才核对；没有刷新过的 DOCX 可读时如实记为未核对。
	if built.PageRefs > 0 && refreshed {
		if docx == nil {
			issues = append(issues, Issue{Kind: IssuePageRefUnresolved, Detail: "没有可回读的 DOCX，章节页码引用未核对"})
		} else {
			checks, pis, err := CheckPageRefs(docx, built.Bookmarks, built.PageRefs, pdfReport, toc)
			if err != nil {
				return fail(err)
			}
			for _, c := range checks {
				if c.Listed > 0 && c.Actual == c.Listed {
					check.PageRefsVerified++
				}
			}
			check.PageRefIssues = len(pis)
			check.VerifiedAgainst = "toc"
			if pdfReport != nil {
				check.VerifiedAgainst = "pdf"
			}
			issues = append(issues, pis...)
		}
	}
	// 文档属性。
	var md *Metadata
	var mis []Issue
	var merr error
	if job.Format == db.ExportPDF {
		md, mis, merr = CheckPDFMetadata(data, job.Doc.Bidder)
	} else {
		md, mis, merr = CheckDOCXMetadata(data, job.Doc.Bidder)
	}
	if merr != nil {
		return fail(merr)
	}
	check.MetadataChecked, check.MetadataIssues, check.Author, check.LastModifiedBy = true, len(mis), md.Author, md.LastModifiedBy
	issues = append(issues, mis...)
	for _, is := range issues {
		if tocIssue(is.Kind) {
			check.TOCIssues++
		}
	}
	check.Images, check.Tables = images, max(tables, 0)

	if job.StrictRuntime {
		var bad []string
		for _, is := range issues {
			if strictIssue(is.Kind) {
				bad = append(bad, is.Detail)
			}
		}
		wantImages, wantTables := expectedContent(job)
		if images < wantImages {
			bad = append(bad, fmt.Sprintf("回读到 %d 张图片，快照有 %d 张渲染图", images, wantImages))
		}
		if tables >= 0 && tables < wantTables {
			bad = append(bad, fmt.Sprintf("回读到 %d 个表格，快照至少有 %d 个（设备选型表、偏离表与附表）", tables, wantTables))
		}
		if len(bad) > 0 {
			if len(bad) > 5 {
				bad = append(bad[:5], fmt.Sprintf("另有 %d 项", len(bad)-5))
			}
			return nil, nil, permanentCode(db.JobErrReadbackFailed, "最终稿回读未通过：%s", strings.Join(bad, "；"))
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
	return out, check, nil
}

// tocIssue 报告问题是否属于目录结构；版式类问题只记为提示。
func tocIssue(kind string) bool {
	switch kind {
	case IssueTOCMissing, IssueTOCPage, IssueHeadingMissing, IssueTOCPlaceholder, IssueTOCEntryNoPage, IssueTOCFieldMissing:
		return true
	}
	return false
}

// strictIssue 报告严格最终稿不容许的问题：目录结构、章节页码引用与文档属性。
func strictIssue(kind string) bool {
	switch kind {
	case IssuePageRefMissing, IssuePageRefUnresolved, IssuePageRefPage, IssueMetadata:
		return true
	}
	return tocIssue(kind)
}

// expectedContent 返回快照要求成稿至少包含的图片数（PNG 渲染图）与表格数（设备选型表、偏离表与 D4 附表）。
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
	for _, n := range []int{len(job.Package.ScoreIndex), len(job.Package.Commitments), len(job.Package.Evidence)} {
		if n > 0 {
			tables++
		}
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
