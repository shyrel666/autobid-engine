package ingest

// PDF 版面读取（供导出版式验收使用）：逐页返回页面框、未去除页眉页脚的原始版面行（文字外接框）与图片外接框。
// 与 Parse 共用内容流解释器与行聚类，不做表格识别与页眉页脚去重。

import (
	"fmt"
	"math"
)

// PDFLine 是一行版面文字。Box 的 Y 为 PDF 坐标（越大越靠上）。
type PDFLine struct {
	Text     string   `json:"text"`
	Size     float64  `json:"size"`
	Baseline float64  `json:"baseline"`
	Box      BBox     `json:"box"`
	Segments []string `json:"segments"`
}

// PDFPageLayout 是一页的版面。
type PDFPageLayout struct {
	Number int        `json:"number"`
	Box    [4]float64 `json:"box"` // MediaBox x0, y0, x1, y1
	Lines  []PDFLine  `json:"lines"`
	Images []BBox     `json:"images"`
}

func (p PDFPageLayout) Width() float64  { return p.Box[2] - p.Box[0] }
func (p PDFPageLayout) Height() float64 { return p.Box[3] - p.Box[1] }

// ReadPDFLayout 读取每页的版面行与图片位置。
func ReadPDFLayout(data []byte) (pages []PDFPageLayout, err error) {
	defer func() {
		if r := recover(); r != nil {
			pages, err = nil, fmt.Errorf("%w: malformed PDF: %v", ErrUnsupportedFormat, r)
		}
	}()
	d, err := openPDF(data)
	if err != nil {
		return nil, err
	}
	in := &pdfInterp{d: d, fonts: map[int]*pdfFont{}}
	refs := d.pages()
	if len(refs) == 0 {
		return nil, fmt.Errorf("%w: PDF has no pages", ErrUnsupportedFormat)
	}
	for i, pg := range refs {
		out := &pdfPageOut{num: i + 1, box: pg.box}
		in.runPage(pg, out)
		lay := PDFPageLayout{Number: i + 1, Box: pg.box, Lines: []PDFLine{}, Images: []BBox{}}
		for _, l := range buildLayoutLines(out) {
			segs := make([]string, len(l.segs))
			for k, s := range l.segs {
				segs[k] = s.text
			}
			lay.Lines = append(lay.Lines, PDFLine{Text: l.text(), Size: l.size, Baseline: l.y, Box: l.box, Segments: segs})
		}
		for _, im := range out.images {
			lay.Images = append(lay.Images, BBox{Page: i + 1, X0: round2(im.box[0]), Y0: round2(im.box[1]), X1: round2(im.box[2]), Y1: round2(im.box[3])})
		}
		pages = append(pages, lay)
	}
	return pages, nil
}

// imageBox 返回单位正方形经 CTM 变换后的外接矩形。
func imageBox(m pdfMat) [4]float64 {
	x0, y0, x1, y1 := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, c := range [][2]float64{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
		x := m[0]*c[0] + m[2]*c[1] + m[4]
		y := m[1]*c[0] + m[3]*c[1] + m[5]
		x0, y0, x1, y1 = math.Min(x0, x), math.Min(y0, y), math.Max(x1, x), math.Max(y1, y)
	}
	return [4]float64{x0, y0, x1, y1}
}
