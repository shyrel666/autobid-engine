package export

// 文档引擎：WPS 或 Word 打开生成的 DOCX，刷新全部目录直到页码稳定，再另存 DOCX / 导出 PDF，使成稿目录页码与
// 用户在 WPS / Word 中看到的排版一致。按 AUTOBID_OFFICE_ENGINE 探测（auto 时 WPS 优先、Word 备选），
// 每个候选先用内置小样本实际跑一遍刷新与回读自检，通过才启用。COM 自动化实现见 office_windows.go。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
)

var ErrOfficeUnavailable = errors.New("export: no document engine (WPS or Word) is available")

// 文档引擎标识。
const (
	EngineWPS  = "wps"
	EngineWord = "word"
)

// Office 是导出所用的文档引擎。
type Office interface {
	Name() string    // EngineWPS / EngineWord
	Version() string // 引擎自报的名称与版本
	// Refresh 打开 inDocx，更新全部目录直到稳定，然后另存 outDocx 与 / 或 outPDF（为空表示不生成）。
	Refresh(ctx context.Context, inDocx, outDocx, outPDF string) (*RefreshResult, error)
}

// RefreshResult 是一次刷新的结果。
type RefreshResult struct {
	Passes  int    `json:"passes"`
	Indexes int    `json:"indexes"`
	TOC     string `json:"toc"`
	Version string `json:"version"`
}

// selfTestTimeout 是启动自检单次刷新的最短上限。
const selfTestTimeout = 2 * time.Minute

// officeEngine 是平台实现额外提供的配置入口。
type officeEngine interface {
	Office
	setTimeout(time.Duration)
}

// OfficeOptions 选择引擎与单次刷新超时。
type OfficeOptions struct {
	Engine  string // auto（默认：WPS 优先，Word 备选）/ wps / word / off（不探测、不启动）
	Timeout time.Duration
}

func engineOrder(engine string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "", "auto":
		return []string{EngineWPS, EngineWord}, nil
	case EngineWPS:
		return []string{EngineWPS}, nil
	case EngineWord:
		return []string{EngineWord}, nil
	case "off":
		return nil, nil
	}
	return nil, fmt.Errorf("%w: unknown engine %q (want auto, wps, word or off)", ErrOfficeUnavailable, engine)
}

// DetectOffice 按顺序探测文档引擎，返回第一个通过自检的引擎；全部不可用时返回各候选的原因。
func DetectOffice(ctx context.Context, opt OfficeOptions) (Office, error) {
	order, err := engineOrder(opt.Engine)
	if err != nil {
		return nil, err
	}
	if len(order) == 0 {
		return nil, fmt.Errorf("%w: %w: 文档引擎已关闭，请启用 WPS 或 Word 后提交最终稿；当前仅可生成 DOCX 预览稿", ErrOfficeUnavailable, ErrNotInstalled)
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 3 * time.Minute
	}
	var reasons []string
	installed := false
	for _, name := range order {
		// 自检用独立上限：作业超时配置得很短时（如故障演练），不应让引擎在启动时被判为不可用。
		o, err := newOffice(name, max(opt.Timeout, selfTestTimeout))
		if err == nil {
			installed = true
			err = selfTestOffice(ctx, o)
		}
		if err == nil {
			o.setTimeout(opt.Timeout)
			return o, nil
		}
		reasons = append(reasons, name+": "+err.Error())
	}
	if !installed {
		// 没有任何候选安装：与“已安装但自检失败”区分（运行能力状态为 unconfigured）。
		return nil, fmt.Errorf("%w: %w: %s", ErrOfficeUnavailable, ErrNotInstalled, strings.Join(reasons, "; "))
	}
	return nil, fmt.Errorf("%w: %s", ErrOfficeUnavailable, strings.Join(reasons, "; "))
}

// selfTestOffice 用内置模板生成两章小样本，刷新后回读：DOCX 目录域要有带页码的条目，PDF 目录页码要与标题所在页一致。
func selfTestOffice(ctx context.Context, o Office) error {
	tpl, err := DefaultTemplate()
	if err != nil {
		return err
	}
	in := BuildInput{
		Doc: db.DocSnapshot{TaskName: "文档引擎自检", Sections: []db.SnapshotSection{
			{SectionID: 1, Code: "1", Title: "自检章节一", Level: 1, Seq: 0, RevNo: 1, RevID: 1},
			{SectionID: 2, Code: "2", Title: "自检章节二", Level: 1, Seq: 1, RevNo: 1, RevID: 2},
		}},
		Bodies: map[int64]string{1: "文档引擎自检正文。", 2: "文档引擎自检正文。"},
		Bidder: "自检", Date: "2026年01月01日",
	}
	built, err := Build(tpl, in)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "autobid-office-selftest-*")
	if err != nil {
		return err
	}
	defer removeAllRetry(dir)
	draft, outDocx, outPDF := filepath.Join(dir, "draft.docx"), filepath.Join(dir, "out.docx"), filepath.Join(dir, "out.pdf")
	if err := os.WriteFile(draft, built.Data, 0o644); err != nil {
		return err
	}
	if _, err := o.Refresh(ctx, draft, outDocx, outPDF); err != nil {
		return err
	}
	data, err := os.ReadFile(outDocx)
	if err != nil {
		return err
	}
	dr, err := CheckDOCX(ctx, data)
	if err != nil {
		return fmt.Errorf("self-test DOCX unreadable: %w", err)
	}
	if len(dr.TOC) < len(in.Doc.Sections) {
		return fmt.Errorf("self-test DOCX has %d TOC entries, want at least %d", len(dr.TOC), len(in.Doc.Sections))
	}
	for _, is := range dr.Issues {
		if is.Kind == IssueTOCPlaceholder || is.Kind == IssueTOCEntryNoPage || is.Kind == IssueTOCFieldMissing {
			return fmt.Errorf("self-test DOCX TOC was not refreshed: %s", is.Detail)
		}
	}
	if data, err = os.ReadFile(outPDF); err != nil {
		return err
	}
	pr, err := CheckPDF(data, tpl.Geometry)
	if err != nil {
		return fmt.Errorf("self-test PDF unreadable: %w", err)
	}
	if len(pr.TOC) < len(in.Doc.Sections) {
		return fmt.Errorf("self-test PDF has %d TOC entries, want at least %d", len(pr.TOC), len(in.Doc.Sections))
	}
	for _, is := range pr.Issues {
		switch is.Kind {
		case IssueTOCMissing, IssueTOCPage, IssueHeadingMissing:
			return fmt.Errorf("self-test PDF TOC check failed: %s", is.Detail)
		}
	}
	return nil
}
