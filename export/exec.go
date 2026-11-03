package export

// 导出执行：套版模板登记与读取、生成 docx、经文档引擎（WPS / Word）刷新目录 / 转 PDF、产物落盘与回读自检。

import (
	"context"
	"encoding/json"
	"fmt"
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

// permanentError 表示重试无意义的失败（如 PDF 导出时文档引擎不可用、快照数据损坏）。
type permanentError struct{ error }

func permanent(format string, args ...any) error { return permanentError{fmt.Errorf(format, args...)} }

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
func (s *Service) Execute(ctx context.Context, tenant string, job *db.ExportJob) (*db.ExportResult, error) {
	if job.Format == db.ExportPDF && s.Office == nil {
		reason := s.OfficeReason
		if reason == "" {
			reason = ErrOfficeUnavailable.Error()
		}
		return nil, permanent("文档引擎（WPS / Word）不可用，无法生成 PDF：%s", reason)
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
			if png, err := s.Store.ReadFile(c.ArtifactPath, c.ArtifactHash); err == nil {
				images[c.ChartID] = png
			}
		}
	}
	in := BuildInput{Doc: job.Doc, Bodies: bodies, Charts: job.Charts, ChartImages: images, Matrix: job.Matrix,
		Bidder: job.Doc.Bidder, Date: job.Doc.CreatedAt.In(time.Local).Format("2006年01月02日"), UpdateFieldsOnOpen: s.Office == nil}
	built, err := Build(tpl, in)
	if err != nil {
		return nil, permanent("生成 docx 失败：%v", err)
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
		res.TOCRefreshed = true
	}
	res.Warnings = append(res.Warnings, selfCheck(ctx, job.Format, final, tpl.Geometry, res.TOCRefreshed)...)
	rel := dirRel + "/" + safeFileName(name)
	sum, err := s.Store.WriteFile(rel, final)
	if err != nil {
		return nil, err
	}
	res.ArtifactPath, res.ArtifactHash, res.ArtifactSize = rel, sum, int64(len(final))
	return res, nil
}

// selfCheck 用 ingest 解析器回读产物：PDF 核对目录页码与版式，刷新过的 DOCX 核对目录域缓存结果。问题记为提示，不改变成败。
func selfCheck(ctx context.Context, format string, data []byte, g Geometry, refreshed bool) []db.ExportWarning {
	var issues []Issue
	switch {
	case format == db.ExportPDF:
		r, err := CheckPDF(data, g)
		if err != nil {
			return []db.ExportWarning{{Kind: db.WarnSelfCheck, Message: "PDF 自检回读失败：" + err.Error()}}
		}
		issues = r.Issues
	case refreshed:
		r, err := CheckDOCX(ctx, data)
		if err != nil {
			return []db.ExportWarning{{Kind: db.WarnSelfCheck, Message: "DOCX 自检回读失败：" + err.Error()}}
		}
		issues = r.Issues
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
	return out
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
