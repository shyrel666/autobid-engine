package export

import (
	"errors"
	"slices"
	"testing"

	"github.com/autobid/autobid-engine/ingest"
)

func TestEngineOrder(t *testing.T) {
	for in, want := range map[string][]string{
		"": {EngineWPS, EngineWord}, "auto": {EngineWPS, EngineWord}, " WPS ": {EngineWPS}, "word": {EngineWord},
	} {
		got, err := engineOrder(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("engineOrder(%q) = %v %v, want %v", in, got, err, want)
		}
	}
	if _, err := engineOrder("libre"); !errors.Is(err, ErrOfficeUnavailable) {
		t.Fatalf("unknown engine accepted: %v", err)
	}
}

// 行末全角收尾标点压缩为半宽后，字形前进框越过右边距最多半个字宽，不算越界；其他字符越界照常检出。
func TestCheckLayoutAllowsLineEndPunctuation(t *testing.T) {
	tpl, _ := DefaultTemplate()
	box := [4]float64{0, 0, 595.3, 841.9}
	right := textArea(ingest.PDFPageLayout{Box: box}, tpl.Geometry).x1
	line := func(text string, x1 float64) ingest.PDFLine {
		return ingest.PDFLine{Text: text, Size: 12, Segments: []string{text}, Box: ingest.BBox{Page: 1, X0: 71, Y0: 600, X1: x1, Y1: 612}}
	}
	pages := []ingest.PDFPageLayout{{Number: 1, Box: box, Lines: []ingest.PDFLine{
		line("保证系统的可扩展性与安全性。", right+6), // 半个字宽：允许
		line("单台网关并发接入不少于", right+3),    // 普通字符：越界
		line("覆盖物理环境、通信网络、", right+7.6), // 超出半个字宽 + 容差：越界
	}}}
	var out []string
	for _, is := range checkLayout(pages, tpl.Geometry).Issues {
		if is.Kind == IssueOutOfBounds {
			out = append(out, is.Detail)
		}
	}
	if len(out) != 2 {
		t.Fatalf("out_of_bounds = %q, want the plain-character line and the over-wide punctuation line", out)
	}
}
