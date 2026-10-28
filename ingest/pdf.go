package ingest

// PDF 文本与版面还原：
//  1. 内容流解释（BT/ET、Tf、Td/TD/Tm/T*、Tj/TJ/'/"、q/Q/cm、Form XObject 递归）得到每个字形的设备坐标与字宽；
//  2. 按基线聚类为版面行，行内按大于约 1.2 倍字号的水平间隙切成列段；
//  3. 连续的多列段行（列起点对齐）还原为表格：首列为空且行距宽松的行视为首列纵向合并，
//     行距紧凑的行视为上一行单元格的折行续写，跨越下一列起点且下一列为空的列段视为横向合并；
//  4. 页尾表格与下一页页首表格列数一致时合并为跨页表格，下一页重复的表头行只保留一次；
//  5. 去除页码行与跨页重复的页眉页脚；无文本层的页面取其图像交给 OCR。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

type pdfMat [6]float64

var pdfIdent = pdfMat{1, 0, 0, 1, 0, 0}

func (a pdfMat) mul(b pdfMat) pdfMat {
	return pdfMat{
		a[0]*b[0] + a[1]*b[2], a[0]*b[1] + a[1]*b[3],
		a[2]*b[0] + a[3]*b[2], a[2]*b[1] + a[3]*b[3],
		a[4]*b[0] + a[5]*b[2] + b[4], a[4]*b[1] + a[5]*b[3] + b[5],
	}
}

type pdfGState struct {
	ctm                      pdfMat
	font                     *pdfFont
	size, tc, tw, th, tl, ts float64
}

type glyphPos struct {
	x, y, w, size float64
	text          string
	seq           int
}

type pdfImage struct {
	data []byte
	mime string
	area float64
}

type pdfPageOut struct {
	num           int
	box           [4]float64
	glyphs        []glyphPos
	images        []pdfImage
	unsupportedIm int
}

type pdfInterp struct {
	d     *pdfDoc
	fonts map[int]*pdfFont
	seq   int
}

func parsePDF(ctx context.Context, data []byte, ocr OCRProvider) (p *Parsed, err error) {
	defer func() {
		if r := recover(); r != nil {
			p, err = nil, fmt.Errorf("%w: malformed PDF: %v", ErrUnsupportedFormat, r)
		}
	}()
	d, err := openPDF(data)
	if err != nil {
		return nil, err
	}
	in := &pdfInterp{d: d, fonts: map[int]*pdfFont{}}
	pages := d.pages()
	if len(pages) == 0 {
		return nil, fmt.Errorf("%w: PDF has no pages", ErrUnsupportedFormat)
	}
	p = &Parsed{Meta: ParseMeta{Parser: "pdf", Pages: len(pages)}}
	if info := d.dictOf(d.trailer["Info"]); info != nil {
		if t, ok := d.resolve(info["Title"]).(pdfString); ok {
			p.Title = strings.TrimSpace(pdfTextString(t))
		}
	}
	outs := make([]*pdfPageOut, len(pages))
	for i, pg := range pages {
		out := &pdfPageOut{num: i + 1, box: pg.box}
		in.runPage(pg, out)
		outs[i] = out
	}

	pageLines := make([][]layoutLine, len(outs))
	textPages := 0
	var ocrErr error
	for i, out := range outs {
		if countText(out.glyphs) >= 5 {
			pageLines[i] = buildLayoutLines(out)
			textPages++
			continue
		}
		if len(out.images) == 0 {
			if out.unsupportedIm > 0 {
				p.Meta.Warnings = append(p.Meta.Warnings, fmt.Sprintf("第 %d 页为扫描图像，但图像编码（CCITT/JBIG2 等）暂不支持送 OCR", out.num))
			}
			continue
		}
		if ocr == nil {
			ocrErr = ErrOCRUnavailable
			p.Meta.Warnings = append(p.Meta.Warnings, fmt.Sprintf("第 %d 页无文本层（疑似扫描件），未配置 OCR", out.num))
			continue
		}
		img := out.images[0]
		for _, im := range out.images[1:] {
			if im.area > img.area {
				img = im
			}
		}
		page, err := ocr.Recognize(ctx, img.data, img.mime)
		if err != nil {
			ocrErr = err
			p.Meta.Warnings = append(p.Meta.Warnings, fmt.Sprintf("第 %d 页 OCR 失败：%v", out.num, err))
			continue
		}
		p.Meta.OCR = ocr.Name()
		p.Meta.OCRPages = append(p.Meta.OCRPages, out.num)
		pageLines[i] = ocrLayoutLines(page, out.num)
	}
	if textPages == 0 && len(p.Meta.OCRPages) == 0 {
		if ocrErr != nil {
			return nil, ocrErr
		}
		return nil, fmt.Errorf("%w: PDF has no text layer", ErrNoText)
	}
	removeRunningLines(pageLines, outs)
	p.Lines = pdfBlocks(pageLines)
	return p, nil
}

func countText(gs []glyphPos) int {
	n := 0
	for _, g := range gs {
		if strings.TrimSpace(g.text) != "" {
			n++
		}
	}
	return n
}

// pdfTextString 解码 PDF 文本字符串（UTF-16BE 带 BOM 或 PDFDocEncoding 近似为 Latin-1）。
func pdfTextString(s pdfString) string {
	if len(s) >= 2 && s[0] == 0xFE && s[1] == 0xFF {
		return utf16BE(s[2:])
	}
	if utf8.Valid(s) {
		return string(s)
	}
	r := make([]rune, len(s))
	for i, c := range s {
		r[i] = rune(c)
	}
	return string(r)
}

type pdfPageRef struct {
	dict pdfDict
	res  pdfDict
	box  [4]float64
}

func (d *pdfDoc) pages() []pdfPageRef {
	root := d.dictOf(d.trailer["Root"])
	var out []pdfPageRef
	var walk func(node pdfDict, res pdfDict, box [4]float64, depth int)
	walk = func(node pdfDict, res pdfDict, box [4]float64, depth int) {
		if node == nil || depth > 64 {
			return
		}
		if r := d.dictOf(node["Resources"]); r != nil {
			res = r
		}
		if mb, ok := d.resolve(node["MediaBox"]).(pdfArray); ok && len(mb) == 4 {
			for i := range 4 {
				box[i] = pdfNum(d.resolve(mb[i]))
			}
		}
		if node["Type"] == pdfName("Page") || node["Kids"] == nil {
			out = append(out, pdfPageRef{dict: node, res: res, box: box})
			return
		}
		kids, _ := d.resolve(node["Kids"]).(pdfArray)
		for _, k := range kids {
			walk(d.dictOf(k), res, box, depth+1)
		}
	}
	walk(d.dictOf(root["Pages"]), nil, [4]float64{0, 0, 612, 792}, 0)
	return out
}

func (in *pdfInterp) runPage(pg pdfPageRef, out *pdfPageOut) {
	var content []byte
	switch c := in.d.resolve(pg.dict["Contents"]).(type) {
	case *pdfStream:
		content, _ = in.d.decodeStream(c)
	case pdfArray:
		for _, x := range c {
			if st, ok := in.d.resolve(x).(*pdfStream); ok {
				if b, err := in.d.decodeStream(st); err == nil {
					content = append(append(content, b...), '\n')
				}
			}
		}
	}
	in.run(content, pg.res, pdfGState{ctm: pdfIdent, th: 1}, out, 0)
}

func (in *pdfInterp) font(res pdfDict, name pdfName) *pdfFont {
	fonts := in.d.dictOf(res["Font"])
	if fonts == nil {
		return in.d.loadFont(nil)
	}
	v := fonts[name]
	if r, ok := v.(pdfRef); ok {
		if f, ok := in.fonts[r.num]; ok {
			return f
		}
		f := in.d.loadFont(v)
		in.fonts[r.num] = f
		return f
	}
	return in.d.loadFont(v)
}

func (in *pdfInterp) run(content []byte, res pdfDict, gs pdfGState, out *pdfPageOut, depth int) {
	if depth > 8 {
		return
	}
	l := &pdfLexer{b: content}
	var (
		stack    []any
		gsStack  []pdfGState
		tm, tlm  = pdfIdent, pdfIdent
		operands = func(n int) []any {
			if len(stack) < n {
				return nil
			}
			return stack[len(stack)-n:]
		}
	)
	num := func(v any) float64 { return pdfNum(v) }
	td := func(tx, ty float64) {
		tlm = pdfMat{1, 0, 0, 1, tx, ty}.mul(tlm)
		tm = tlm
	}
	show := func(s pdfString) {
		if gs.font == nil {
			gs.font = in.d.loadFont(nil)
		}
		for _, g := range gs.font.decode(s) {
			trm := pdfMat{gs.size * gs.th, 0, 0, gs.size, 0, gs.ts}.mul(tm).mul(gs.ctm)
			scaleX := math.Hypot(trm[0], trm[1])
			if g.text != "" {
				in.seq++
				out.glyphs = append(out.glyphs, glyphPos{x: trm[4], y: trm[5], w: g.width / 1000 * scaleX,
					size: math.Hypot(trm[2], trm[3]), text: g.text, seq: in.seq})
			}
			tx := g.width/1000*gs.size + gs.tc
			if g.space {
				tx += gs.tw
			}
			tm = pdfMat{1, 0, 0, 1, tx * gs.th, 0}.mul(tm)
		}
	}
	for {
		v, err := l.next(false)
		if err != nil {
			return
		}
		op, ok := v.(pdfKeyword)
		if !ok {
			stack = append(stack, v)
			continue
		}
		switch op {
		case "q":
			gsStack = append(gsStack, gs)
		case "Q":
			if n := len(gsStack); n > 0 {
				gs, gsStack = gsStack[n-1], gsStack[:n-1]
			}
		case "cm":
			if o := operands(6); o != nil {
				gs.ctm = pdfMat{num(o[0]), num(o[1]), num(o[2]), num(o[3]), num(o[4]), num(o[5])}.mul(gs.ctm)
			}
		case "BT":
			tm, tlm = pdfIdent, pdfIdent
		case "Tf":
			if o := operands(2); o != nil {
				if name, ok := o[0].(pdfName); ok {
					gs.font = in.font(res, name)
				}
				gs.size = num(o[1])
			}
		case "Td":
			if o := operands(2); o != nil {
				td(num(o[0]), num(o[1]))
			}
		case "TD":
			if o := operands(2); o != nil {
				gs.tl = -num(o[1])
				td(num(o[0]), num(o[1]))
			}
		case "Tm":
			if o := operands(6); o != nil {
				tlm = pdfMat{num(o[0]), num(o[1]), num(o[2]), num(o[3]), num(o[4]), num(o[5])}
				tm = tlm
			}
		case "T*":
			td(0, -gs.tl)
		case "Tc":
			if o := operands(1); o != nil {
				gs.tc = num(o[0])
			}
		case "Tw":
			if o := operands(1); o != nil {
				gs.tw = num(o[0])
			}
		case "Tz":
			if o := operands(1); o != nil {
				gs.th = num(o[0]) / 100
			}
		case "TL":
			if o := operands(1); o != nil {
				gs.tl = num(o[0])
			}
		case "Ts":
			if o := operands(1); o != nil {
				gs.ts = num(o[0])
			}
		case "Tj":
			if o := operands(1); o != nil {
				if s, ok := o[0].(pdfString); ok {
					show(s)
				}
			}
		case "'":
			td(0, -gs.tl)
			if o := operands(1); o != nil {
				if s, ok := o[0].(pdfString); ok {
					show(s)
				}
			}
		case "\"":
			if o := operands(3); o != nil {
				gs.tw, gs.tc = num(o[0]), num(o[1])
				td(0, -gs.tl)
				if s, ok := o[2].(pdfString); ok {
					show(s)
				}
			}
		case "TJ":
			if o := operands(1); o != nil {
				arr, _ := o[0].(pdfArray)
				for _, x := range arr {
					switch t := x.(type) {
					case pdfString:
						show(t)
					case int64, float64:
						tm = pdfMat{1, 0, 0, 1, -num(t) / 1000 * gs.size * gs.th, 0}.mul(tm)
					}
				}
			}
		case "Do":
			if o := operands(1); o != nil {
				if name, ok := o[0].(pdfName); ok {
					in.doXObject(res, name, gs, out, depth)
				}
			}
		case "BI":
			skipInlineImage(l)
		}
		stack = stack[:0]
	}
}

func skipInlineImage(l *pdfLexer) {
	i := bytes.Index(l.b[l.pos:], []byte("ID"))
	if i < 0 {
		l.pos = len(l.b)
		return
	}
	l.pos += i + 2
	for {
		j := bytes.Index(l.b[l.pos:], []byte("EI"))
		if j < 0 {
			l.pos = len(l.b)
			return
		}
		at := l.pos + j
		l.pos = at + 2
		if at > 0 && isPDFSpace(l.b[at-1]) && (l.pos >= len(l.b) || isPDFSpace(l.b[l.pos])) {
			return
		}
	}
}

func (in *pdfInterp) doXObject(res pdfDict, name pdfName, gs pdfGState, out *pdfPageOut, depth int) {
	xobjs := in.d.dictOf(res["XObject"])
	if xobjs == nil {
		return
	}
	st, ok := in.d.resolve(xobjs[name]).(*pdfStream)
	if !ok {
		return
	}
	switch st.dict["Subtype"] {
	case pdfName("Form"):
		m := pdfIdent
		if arr, ok := in.d.resolve(st.dict["Matrix"]).(pdfArray); ok && len(arr) == 6 {
			for i := range 6 {
				m[i] = pdfNum(in.d.resolve(arr[i]))
			}
		}
		fr := in.d.dictOf(st.dict["Resources"])
		if fr == nil {
			fr = res
		}
		data, err := in.d.decodeStream(st)
		if err != nil {
			return
		}
		sub := gs
		sub.ctm = m.mul(gs.ctm)
		in.run(data, fr, sub, out, depth+1)
	case pdfName("Image"):
		area := math.Abs(gs.ctm[0]*gs.ctm[3] - gs.ctm[1]*gs.ctm[2])
		data, filter, err := in.d.decodeStreamUntilImage(st)
		switch {
		case errors.Is(err, errImageFilter) && (filter == "DCTDecode" || filter == "DCT"):
			out.images = append(out.images, pdfImage{data: data, mime: "image/jpeg", area: area})
		case errors.Is(err, errImageFilter) && filter == "JPXDecode":
			out.images = append(out.images, pdfImage{data: data, mime: "image/jp2", area: area})
		case err == nil:
			if png := in.rawImagePNG(st, data); png != nil {
				out.images = append(out.images, pdfImage{data: png, mime: "image/png", area: area})
			} else {
				out.unsupportedIm++
			}
		default:
			out.unsupportedIm++
		}
	}
}

// rawImagePNG 把 8 bit DeviceGray / DeviceRGB 原始像素重新编码为 PNG 供 OCR。
func (in *pdfInterp) rawImagePNG(st *pdfStream, data []byte) []byte {
	w := int(pdfNum(in.d.resolve(st.dict["Width"])))
	h := int(pdfNum(in.d.resolve(st.dict["Height"])))
	bpc := int(pdfNum(in.d.resolve(st.dict["BitsPerComponent"])))
	cs, _ := in.d.resolve(st.dict["ColorSpace"]).(pdfName)
	if w <= 0 || h <= 0 || bpc != 8 {
		return nil
	}
	var img image.Image
	switch cs {
	case "DeviceGray":
		if len(data) < w*h {
			return nil
		}
		g := image.NewGray(image.Rect(0, 0, w, h))
		copy(g.Pix, data[:w*h])
		img = g
	case "DeviceRGB":
		if len(data) < w*h*3 {
			return nil
		}
		rgba := image.NewRGBA(image.Rect(0, 0, w, h))
		for i := 0; i < w*h; i++ {
			rgba.Set(i%w, i/w, color.RGBA{data[i*3], data[i*3+1], data[i*3+2], 255})
		}
		img = rgba
	default:
		return nil
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// 版面行与表格
// ---------------------------------------------------------------------------

type layoutSeg struct {
	x0, x1 float64
	text   string
}

type layoutLine struct {
	page   int
	y      float64 // 基线（PDF 坐标，越大越靠上）；OCR 行为 -top 以保持同一方向
	size   float64
	segs   []layoutSeg
	box    BBox
	ocr    bool
	height float64 // 页面高度
	margin bool    // 位于页眉/页脚带（页面顶部/底部 8%）
}

func (l layoutLine) text() string {
	parts := make([]string, 0, len(l.segs))
	for _, s := range l.segs {
		parts = append(parts, s.text)
	}
	return strings.Join(parts, " ")
}

// buildLayoutLines 按基线把字形聚为行，行内按水平间隙切分列段。
func buildLayoutLines(out *pdfPageOut) []layoutLine {
	gs := append([]glyphPos(nil), out.glyphs...)
	sort.SliceStable(gs, func(i, j int) bool { return gs[i].y > gs[j].y })
	var groups [][]glyphPos
	for _, g := range gs {
		if n := len(groups); n > 0 {
			ref := groups[n-1][0]
			if math.Abs(ref.y-g.y) <= max(ref.size*0.45, 1) {
				groups[n-1] = append(groups[n-1], g)
				continue
			}
		}
		groups = append(groups, []glyphPos{g})
	}
	height := out.box[3] - out.box[1]
	var lines []layoutLine
	for _, grp := range groups {
		sort.SliceStable(grp, func(i, j int) bool { return grp[i].x < grp[j].x })
		size := 0.0
		for _, g := range grp {
			size = max(size, g.size)
		}
		size = max(size, 1)
		colGap := max(size*1.2, 4)
		var (
			segs []layoutSeg
			cur  strings.Builder
			seg  layoutSeg
			prev *glyphPos
		)
		flush := func() {
			if t := strings.TrimSpace(cur.String()); t != "" {
				seg.text = t
				segs = append(segs, seg)
			}
			cur.Reset()
		}
		for i := range grp {
			g := &grp[i]
			if prev != nil {
				end := prev.x + prev.w
				// 伪粗体（同一字形偏移重绘）去重。
				if g.text == prev.text && g.x-prev.x < max(prev.w*0.3, 0.5) {
					continue
				}
				gap := g.x - end
				if gap > colGap {
					flush()
					seg = layoutSeg{x0: g.x}
				} else if gap > size*0.18 && g.text != " " && prev.text != " " {
					pr, _ := utf8.DecodeLastRuneInString(prev.text)
					nr, _ := utf8.DecodeRuneInString(g.text)
					if !isCJK(pr) && !isCJK(nr) {
						cur.WriteByte(' ')
					}
				}
			} else {
				seg = layoutSeg{x0: g.x}
			}
			cur.WriteString(g.text)
			seg.x1 = g.x + g.w
			prev = g
		}
		flush()
		if len(segs) == 0 {
			continue
		}
		segs = mergeHeadingSegs(segs)
		y := grp[0].y
		lines = append(lines, layoutLine{page: out.num, y: y, size: size, segs: segs, height: height,
			box: BBox{Page: out.num, X0: round2(segs[0].x0), Y0: round2(y - size*0.25), X1: round2(segs[len(segs)-1].x1), Y1: round2(y + size*0.85)}})
	}
	return lines
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

var reHeadingNo = regexp.MustCompile(`^(?:[★▲]?\d{1,3}(?:[.．]\d{1,3})*[.．]?|第[一二三四五六七八九十百零〇两\d]+[章节篇部]|[一二三四五六七八九十]{1,3}[、.．])$`)

// mergeHeadingSegs 把“编号 + 宽间距 + 标题”形式的两段行合并为一段，避免误判为表格行。
func mergeHeadingSegs(segs []layoutSeg) []layoutSeg {
	if len(segs) == 2 && reHeadingNo.MatchString(segs[0].text) {
		return []layoutSeg{{x0: segs[0].x0, x1: segs[1].x1, text: segs[0].text + " " + segs[1].text}}
	}
	return segs
}

func ocrLayoutLines(page *OCRPage, pageNo int) []layoutLine {
	var out []layoutLine
	for _, l := range page.Lines {
		t := strings.TrimSpace(l.Text)
		if t == "" {
			continue
		}
		h := max(l.BBox[3]-l.BBox[1], 1)
		out = append(out, layoutLine{page: pageNo, y: -l.BBox[1], size: h, ocr: true, height: page.Height,
			segs: []layoutSeg{{x0: l.BBox[0], x1: l.BBox[2], text: t}},
			box:  BBox{Page: pageNo, X0: l.BBox[0], Y0: l.BBox[1], X1: l.BBox[2], Y1: l.BBox[3], Conf: l.Confidence}})
	}
	return out
}

var reDigits = regexp.MustCompile(`\d+`)

// removeRunningLines 去除页码行，以及在页面顶部/底部 8% 区域内、跨至少两页且过半页面重复出现的页眉页脚；
// 保留下来的页眉/页脚带文字标记为 margin，跨页续表判断时跳过它们。
func removeRunningLines(pages [][]layoutLine, outs []*pdfPageOut) {
	edge := func(l layoutLine, out *pdfPageOut) bool {
		if l.ocr || l.height <= 0 {
			return false
		}
		top, bottom := out.box[3], out.box[1]
		margin := l.height * 0.08
		return l.y > top-margin || l.y < bottom+margin
	}
	counts := map[string]int{}
	textPages := 0
	for i, ls := range pages {
		if len(ls) > 0 {
			textPages++
		}
		seen := map[string]bool{}
		for _, l := range ls {
			if edge(l, outs[i]) {
				k := reDigits.ReplaceAllString(l.text(), "#")
				if !seen[k] {
					seen[k] = true
					counts[k]++
				}
			}
		}
	}
	for i, ls := range pages {
		kept := ls[:0]
		for _, l := range ls {
			t := strings.TrimSpace(l.text())
			if rePageMark.MatchString(t) {
				continue
			}
			if edge(l, outs[i]) && textPages >= 2 {
				if n := counts[reDigits.ReplaceAllString(l.text(), "#")]; n >= 2 && n*2 >= textPages {
					continue
				}
			}
			l.margin = edge(l, outs[i])
			kept = append(kept, l)
		}
		pages[i] = kept
	}
}

type pdfTable struct {
	t    *Table
	cols []float64
	box  []BBox
}

type pdfItem struct {
	line  *layoutLine
	table *pdfTable
}

// pdfBlocks 识别各页表格、合并跨页表格并输出切分器输入行。
func pdfBlocks(pages [][]layoutLine) []Line {
	items := make([][]pdfItem, len(pages))
	for i, ls := range pages {
		items[i] = detectTables(ls)
	}
	// 跨页续表：上一页正文以表格结束、下一页正文以列数一致的表格开始（页眉/页脚带文字不计）。
	marginal := func(it pdfItem) bool { return it.line != nil && it.line.margin }
	var open *pdfTable
	for i := range items {
		if open != nil {
			k := 0
			for k < len(items[i]) && marginal(items[i][k]) {
				k++
			}
			if k < len(items[i]) && items[i][k].table != nil && sameColumns(open, items[i][k].table) {
				next := items[i][k].table
				dropHeader := open.t.rowText(0) == next.t.rowText(0)
				open.t.appendRows(next.t, dropHeader)
				open.box = append(open.box, next.box...)
				items[i] = append(append([]pdfItem(nil), items[i][:k]...), items[i][k+1:]...)
				rest := false
				for _, it := range items[i] {
					rest = rest || !marginal(it)
				}
				if !rest {
					continue // 整页都是续表，继续向下一页延续
				}
			}
		}
		open = nil
		n := len(items[i])
		for n > 0 && marginal(items[i][n-1]) {
			n--
		}
		if n > 0 && items[i][n-1].table != nil {
			open = items[i][n-1].table
		}
	}
	var lines []Line
	block := 0
	for _, its := range items {
		for _, it := range its {
			if it.table != nil {
				t := it.table.t
				lines = append(lines, Line{Text: t.Markdown, Page: t.PageStart, PageEnd: t.PageEnd, Table: t, Boxes: it.table.box, Block: block})
			} else {
				l := it.line
				lines = append(lines, Line{Text: l.text(), Page: l.page, Boxes: []BBox{l.box}, Block: block})
			}
			block++
		}
	}
	return lines
}

func sameColumns(a, b *pdfTable) bool {
	if a.t.Cols != b.t.Cols || len(a.cols) != len(b.cols) {
		return false
	}
	for i := range a.cols {
		if math.Abs(a.cols[i]-b.cols[i]) > 12 {
			return false
		}
	}
	return true
}

func colIndex(cols []float64, x, tol float64) int {
	idx := -1
	for i, c := range cols {
		if c <= x+tol {
			idx = i
		}
	}
	return max(idx, 0)
}

func aligned(cols []float64, x, tol float64) (int, bool) {
	for i, c := range cols {
		if math.Abs(c-x) <= tol {
			return i, true
		}
	}
	return 0, false
}

// detectTables 把一页的版面行划分为正文行与表格。
func detectTables(lines []layoutLine) []pdfItem {
	var items []pdfItem
	for i := 0; i < len(lines); {
		l := lines[i]
		if len(l.segs) < 2 || l.ocr {
			items = append(items, pdfItem{line: &lines[i]})
			i++
			continue
		}
		tol := max(l.size*1.5, 6)
		cols := make([]float64, 0, len(l.segs))
		for _, s := range l.segs {
			cols = append(cols, s.x0)
		}
		j, multi := i+1, 1
		for j < len(lines) {
			ln := lines[j]
			if lines[j-1].y-ln.y > 3*max(ln.size, l.size) {
				break
			}
			hit := 0
			for _, s := range ln.segs {
				if _, ok := aligned(cols, s.x0, tol); ok {
					hit++
				}
			}
			if len(ln.segs) >= 2 && hit >= 1 && hit >= len(ln.segs)-1 {
				for _, s := range ln.segs {
					if _, ok := aligned(cols, s.x0, tol); !ok {
						cols = append(cols, s.x0)
					}
				}
				sort.Float64s(cols)
				multi++
				j++
				continue
			}
			if len(ln.segs) == 1 {
				if c, ok := aligned(cols, ln.segs[0].x0, tol); ok && c > 0 {
					j++
					continue
				}
			}
			break
		}
		if multi < 2 {
			items = append(items, pdfItem{line: &lines[i]})
			i++
			continue
		}
		items = append(items, pdfItem{table: buildPDFTable(lines[i:j], cols, tol)})
		i = j
	}
	return items
}

type pdfRow struct {
	page  int
	cells []string
	span  []int
	cont  []bool // 首列纵向合并续行
}

func joinCell(a, b string) string {
	if a == "" {
		return b
	}
	pr, _ := utf8.DecodeLastRuneInString(a)
	nr, _ := utf8.DecodeRuneInString(b)
	if isCJK(pr) || isCJK(nr) {
		return a + b
	}
	return a + " " + b
}

func buildPDFTable(lines []layoutLine, cols []float64, tol float64) *pdfTable {
	n := len(cols)
	var rows []pdfRow
	pt := &pdfTable{cols: cols}
	boxes := map[int]*BBox{}
	var order []int
	for k, l := range lines {
		if b, ok := boxes[l.page]; ok {
			b.X0, b.Y0, b.X1, b.Y1 = min(b.X0, l.box.X0), min(b.Y0, l.box.Y0), max(b.X1, l.box.X1), max(b.Y1, l.box.Y1)
		} else {
			cp := l.box
			boxes[l.page] = &cp
			order = append(order, l.page)
		}
		first := colIndex(cols, l.segs[0].x0, tol)
		if k > 0 && first > 0 && len(rows) > 0 && lines[k-1].y-l.y < 1.4*l.size {
			// 行距紧凑：上一行单元格折行续写。
			r := &rows[len(rows)-1]
			for _, s := range l.segs {
				c := colIndex(cols, s.x0, tol)
				r.cells[c] = joinCell(r.cells[c], s.text)
			}
			continue
		}
		r := pdfRow{page: l.page, cells: make([]string, n), span: make([]int, n), cont: make([]bool, n)}
		for i, s := range l.segs {
			c := colIndex(cols, s.x0, tol)
			r.cells[c] = joinCell(r.cells[c], s.text)
			span := 1
			nextX0 := math.Inf(1)
			if i+1 < len(l.segs) {
				nextX0 = l.segs[i+1].x0
			}
			for c+span < n && s.x1 > cols[c+span]+tol && cols[c+span] < nextX0-tol {
				span++
			}
			r.span[c] = max(r.span[c], span)
		}
		if k > 0 && first > 0 && len(rows) > 0 {
			r.cont[0] = true
		}
		rows = append(rows, r)
	}
	b := newTableBuilder()
	for _, r := range rows {
		ri := b.startRow(r.page)
		for c := 0; c < n; {
			if _, taken := b.occupied[[2]int{ri, c}]; taken {
				c++
				continue
			}
			if r.cont[c] {
				if _, ok := b.extendDown(ri, c); ok {
					c++
					continue
				}
			}
			span := max(r.span[c], 1)
			b.add(ri, c, span, r.cells[c])
			c += span
		}
	}
	pt.t = b.build(1, "pdf")
	for _, p := range order {
		pt.box = append(pt.box, *boxes[p])
	}
	return pt
}
