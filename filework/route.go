package filework

// S5a（AGENT_TOOL_SKILLS_PLAN 4.1 / 4.3）：解析路由与执行版本。
//
// parser_profile 取 auto / native / ocr_only（enhanced_pdf 增强后端尚未交付，返回 ErrProfileDisabled）。
// 新作业在首次提交时把路由固定为 executor：固定解析器版本，路由含 OCR 时再加当时 OCR 服务回报的执行版本；
// 回收、重试与暂停恢复都按原 executor 执行，不随配置变化。执行时 OCR 服务版本与锁定的不一致，
// 以 RUNTIME_UNAVAILABLE 失败（按新请求重新提交）；OCR 服务暂时不可达则在原期限与尝试预算内重试。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/ingest"
	"github.com/autobid/autobid-engine/toolhealth"
)

// FixedParserVersion 是内置固定解析器（DOCX / PDF / 表格 / 文本）的版本标签。
const FixedParserVersion = "fixed-parser-v1"

// ArchiveExecutor 是 ZIP 清单展开的执行版本。
const ArchiveExecutor = "zip-manifest-v1"

const executorOCRSep = "+ocr:"

var (
	// ErrProfileDisabled 表示解析配置在本服务器上没有交付（如 enhanced_pdf），映射为 FEATURE_DISABLED。
	ErrProfileDisabled = errors.New("filework: parser profile is not available on this server")
	// ErrOCRRequired 表示所选路由需要 OCR，但本机 OCR 不可用，映射为 RUNTIME_UNAVAILABLE（不插入作业）。
	ErrOCRRequired = errors.New("filework: OCR is required but the local OCR runtime is unavailable")
	// ErrRuntimeChanged 表示执行时 OCR 服务版本与首次提交时锁定的不一致，作业以 RUNTIME_UNAVAILABLE 失败。
	ErrRuntimeChanged = errors.New("filework: the OCR runtime differs from the version pinned at submission")
	// ErrRuntimeTransient 表示 OCR 服务暂时不可达；作业回到队列，在原期限与尝试预算内重试。
	ErrRuntimeTransient = errors.New("filework: the OCR runtime is temporarily unavailable")
)

// NormalizeProfile 校验解析配置；空值等同 auto。
func NormalizeProfile(p string) (string, error) {
	switch strings.TrimSpace(p) {
	case "", db.ParserProfileAuto:
		return db.ParserProfileAuto, nil
	case db.ParserProfileNative:
		return db.ParserProfileNative, nil
	case db.ParserProfileOCROnly:
		return db.ParserProfileOCROnly, nil
	case db.ParserProfileEnhancedPDF:
		return "", fmt.Errorf("%w: enhanced_pdf（增强 PDF 解析后端尚未交付）", ErrProfileDisabled)
	}
	return "", fmt.Errorf("%w: parser_profile must be auto, native or ocr_only", ingest.ErrInvalidInput)
}

// ocrCapable 报告格式是否可能经过 OCR（PDF 扫描页、图片）。
func ocrCapable(format string) bool {
	return format == ingest.FormatPDF || format == ingest.FormatImage
}

// OCRRuntime 探测当前 OCR 执行版本：运行能力注册表判定为未配置 / 自检失败时视为不可用（检查中仍直接探测服务）。
func (p *Processor) OCRRuntime(ctx context.Context) (string, error) {
	if p.Runtime != nil {
		switch st := p.Runtime.Get(toolhealth.OCR); st.State {
		case toolhealth.Unconfigured, toolhealth.SelfTestFailed:
			reason := st.Reason
			if reason == "" {
				reason = string(st.State)
			}
			return "", fmt.Errorf("%w: %w: %s", ErrOCRRequired, ingest.ErrOCRUnavailable, reason)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v, err := ingest.OCRVersion(ctx, p.OCR)
	if err != nil {
		if errors.Is(err, ingest.ErrOCRUnavailable) {
			return "", fmt.Errorf("%w: %w", ErrOCRRequired, err)
		}
		return "", fmt.Errorf("%w: %w: %v", ErrOCRRequired, ingest.ErrOCRUnavailable, err)
	}
	return v, nil
}

// ResolveRoute 在首次提交时固定解析路由（数据库事务外调用；外部探测不进长事务）。
// 返回规范化后的配置与 executor；ocr_only / 图片在 OCR 不可用时返回 ErrOCRRequired。
func (p *Processor) ResolveRoute(ctx context.Context, format, profile string) (string, string, error) {
	profile, err := NormalizeProfile(profile)
	if err != nil {
		return "", "", err
	}
	switch {
	case format == ingest.FormatZIP:
		return "", "", fmt.Errorf("%w: ZIP 招标包按清单展开，不按单篇文档解析", ingest.ErrUnsupportedFormat)
	case profile == db.ParserProfileOCROnly && !ocrCapable(format):
		return "", "", fmt.Errorf("%w: ocr_only 只适用于 PDF 与图片，当前文件格式为 %s", ingest.ErrInvalidInput, format)
	case profile == db.ParserProfileNative && format == ingest.FormatImage:
		return "", "", fmt.Errorf("%w: 图片文件只能经 OCR 识别，不能使用 native", ingest.ErrInvalidInput)
	}
	if profile == db.ParserProfileNative || !ocrCapable(format) {
		return profile, FixedParserVersion, nil
	}
	v, err := p.OCRRuntime(ctx)
	if err != nil {
		// auto 的 PDF 没有 OCR 时仍可解析文本层，扫描页记为缺口；ocr_only 与图片必须有 OCR。
		if profile == db.ParserProfileAuto && format == ingest.FormatPDF {
			return profile, FixedParserVersion, nil
		}
		return "", "", err
	}
	return profile, FixedParserVersion + executorOCRSep + v, nil
}

// executorOCR 返回 executor 锁定的 OCR 版本；路由不含 OCR 时 ok=false。
func executorOCR(executor string) (string, bool) {
	_, v, ok := strings.Cut(executor, executorOCRSep)
	return v, ok && v != ""
}

// pinnedOCR 只允许首次提交时锁定的 OCR 执行版本参与解析；服务暂时不可达或版本变化时记录致命错误并让后续页面立即失败，
// 由处理器丢弃本次候选（不把暂时故障固化为“缺页”）。
type pinnedOCR struct {
	inner ingest.OCRProvider
	want  string
	mu    sync.Mutex
	fatal error
	ok    bool // 已核对版本
	used  bool // 至少一页经 OCR
}

func (o *pinnedOCR) Name() string { return o.inner.Name() }

func (o *pinnedOCR) fail(err error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fatal == nil {
		o.fatal = err
	}
	return o.fatal
}

func (o *pinnedOCR) err() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.fatal
}

func (o *pinnedOCR) Recognize(ctx context.Context, image []byte, mime string) (*ingest.OCRPage, error) {
	if err := o.err(); err != nil {
		return nil, err
	}
	o.mu.Lock()
	checked := o.ok
	o.mu.Unlock()
	if !checked {
		v, err := ingest.OCRVersion(ctx, o.inner)
		switch {
		case errors.Is(err, ingest.ErrOCRTransient):
			return nil, o.fail(fmt.Errorf("%w: %v", ErrRuntimeTransient, err))
		case err != nil:
			return nil, o.fail(fmt.Errorf("%w: %v", ErrRuntimeChanged, err))
		case v != o.want:
			return nil, o.fail(fmt.Errorf("%w: pinned %q, now %q", ErrRuntimeChanged, o.want, v))
		}
		o.mu.Lock()
		o.ok = true
		o.mu.Unlock()
	}
	page, err := o.inner.Recognize(ctx, image, mime)
	switch {
	case errors.Is(err, ingest.ErrOCRTransient):
		return nil, o.fail(fmt.Errorf("%w: %v", ErrRuntimeTransient, err))
	case err != nil:
		return nil, err
	case page.Version != "" && page.Version != o.want:
		return nil, o.fail(fmt.Errorf("%w: pinned %q, recognized by %q", ErrRuntimeChanged, o.want, page.Version))
	}
	o.mu.Lock()
	o.used = true
	o.mu.Unlock()
	return page, nil
}

// ocrFor 按 executor 选择本次解析可用的 OCR：旧作业（executor 为空）沿用当前 OCR；路由不含 OCR 时不提供 OCR。
func (p *Processor) ocrFor(executor string) (ingest.OCRProvider, *pinnedOCR) {
	if executor == "" {
		return p.OCR, nil
	}
	want, ok := executorOCR(executor)
	if !ok || p.OCR == nil {
		return nil, nil
	}
	if _, unavailable := p.OCR.(ingest.UnavailableOCR); unavailable {
		pin := &pinnedOCR{inner: p.OCR, want: want}
		pin.fatal = fmt.Errorf("%w: OCR is no longer configured", ErrRuntimeChanged)
		return pin, pin
	}
	pin := &pinnedOCR{inner: p.OCR, want: want}
	return pin, pin
}

// parserVersion 是记录在候选上的实际执行版本：用到 OCR 时为完整 executor，否则为固定解析器版本。
func parserVersion(executor string, pin *pinnedOCR, ocrPages int) string {
	if pin != nil {
		pin.mu.Lock()
		used := pin.used
		pin.mu.Unlock()
		if used {
			return executor
		}
		return FixedParserVersion
	}
	if executor != "" {
		return executor
	}
	if ocrPages > 0 {
		return FixedParserVersion + executorOCRSep + "unpinned"
	}
	return FixedParserVersion
}
