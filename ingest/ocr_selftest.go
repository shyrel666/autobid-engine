package ingest

// OCR 功能自检：用内置的固定中文样本图片（“招标文件自检 / 编号 2026-1103”）实际识别一次，
// 检查识别出中文与数字、坐标落在图片范围内。只检测到程序或服务地址存在不算可用。

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

//go:embed ocr_selftest.png
var ocrSelfTestPNG []byte

// OCRSelfTest 识别内置样本；UnavailableOCR 返回 ErrOCRUnavailable。
func OCRSelfTest(ctx context.Context, p OCRProvider) error {
	if p == nil {
		return ErrOCRUnavailable
	}
	if _, ok := p.(UnavailableOCR); ok {
		return ErrOCRUnavailable
	}
	page, err := p.Recognize(ctx, ocrSelfTestPNG, "image/png")
	if err != nil {
		return fmt.Errorf("self-test recognize: %w", err)
	}
	if page == nil || len(page.Lines) == 0 {
		return errors.New("self-test: OCR returned no text lines")
	}
	var all strings.Builder
	for _, l := range page.Lines {
		for _, r := range l.Text {
			if !unicode.IsSpace(r) {
				all.WriteRune(r)
			}
		}
		if page.Width > 0 && page.Height > 0 && (l.BBox[2] <= l.BBox[0] || l.BBox[3] <= l.BBox[1] ||
			l.BBox[0] < -1 || l.BBox[1] < -1 || l.BBox[2] > page.Width+1 || l.BBox[3] > page.Height+1) {
			return fmt.Errorf("self-test: line %q has bbox %v outside the %gx%g page", l.Text, l.BBox, page.Width, page.Height)
		}
	}
	text := all.String()
	if !strings.Contains(text, "招标") || !strings.Contains(text, "2026") {
		return fmt.Errorf("self-test: expected 招标 and 2026 in the recognized text, got %q", truncateRunes(text, 60))
	}
	return nil
}
