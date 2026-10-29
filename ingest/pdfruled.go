package ingest

// 有框线表格还原：Word / WPS 导出的招标 PDF 用细线（描边线段或细长填充矩形）绘制单元格边框。
// 按框线连通分量划出表格区域，以垂直线 x / 水平线 y 聚类得到网格，缺少内部边线的相邻网格单元合并为一个单元格，
// 再按字形中心落入单元格还原文字。与按文字列对齐推断（detectTables）相比，能正确还原多行单元格与纵向合并，
// 不会把单元格内的折行拆成多行。没有竖线的“三线表”与无框线表格仍走文字对齐推断。

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	ruleJoinTol = 2.0 // 水平线与垂直线相交判定容差（pt）
	ruleGridTol = 2.5 // 网格线聚类容差（pt），双线边框聚为一条
)

// ruleSeg 是合并后的框线：水平线 pos=y、a0..a1 为 x 范围；垂直线 pos=x、a0..a1 为 y 范围。
type ruleSeg struct{ a0, a1, pos float64 }

// mergeRuleSegs 合并共线且相接 / 重叠的线段（逐格绘制的边框会被拆成很多短线段）。
func mergeRuleSegs(segs []ruleSeg) []ruleSeg {
	sort.Slice(segs, func(i, j int) bool {
		if math.Abs(segs[i].pos-segs[j].pos) > 0.01 {
			return segs[i].pos < segs[j].pos
		}
		return segs[i].a0 < segs[j].a0
	})
	var out []ruleSeg
	for _, s := range segs {
		merged := false
		for k := len(out) - 1; k >= 0 && s.pos-out[k].pos <= 1.2; k-- {
			o := &out[k]
			if s.a0 <= o.a1+1.5 && s.a1 >= o.a0-1.5 {
				o.a0, o.a1 = min(o.a0, s.a0), max(o.a1, s.a1)
				merged = true
				break
			}
		}
		if !merged {
			out = append(out, s)
		}
	}
	return out
}

// clusterPos 把相距不超过 tol 的坐标聚为一条网格线（取平均），返回升序结果。
func clusterPos(vals []float64, tol float64) []float64 {
	sort.Float64s(vals)
	var (
		out  []float64
		sum  float64
		n    int
		last float64
	)
	for _, v := range vals {
		if n > 0 && v-last > tol {
			out = append(out, sum/float64(n))
			sum, n = 0, 0
		}
		sum += v
		n++
		last = v
	}
	if n > 0 {
		out = append(out, sum/float64(n))
	}
	return out
}

type unionFind []int

func newUnionFind(n int) unionFind {
	u := make(unionFind, n)
	for i := range u {
		u[i] = i
	}
	return u
}

func (u unionFind) find(i int) int {
	for u[i] != i {
		u[i] = u[u[i]]
		i = u[i]
	}
	return i
}

func (u unionFind) union(a, b int) {
	if ra, rb := u.find(a), u.find(b); ra != rb {
		u[max(ra, rb)] = min(ra, rb)
	}
}

// ruledTables 还原一页上的有框线表格，返回按自上而下排序的表格与被表格占用的字形标记。
func ruledTables(out *pdfPageOut) ([]*pdfTable, []bool) {
	used := make([]bool, len(out.glyphs))
	var hs, vs []ruleSeg
	for _, r := range out.rules {
		if r.y0 == r.y1 {
			hs = append(hs, ruleSeg{r.x0, r.x1, r.y0})
		} else {
			vs = append(vs, ruleSeg{r.y0, r.y1, r.x0})
		}
	}
	if len(hs) < 2 || len(vs) < 2 {
		return nil, used
	}
	hs, vs = mergeRuleSegs(mergeRuleSegs(hs)), mergeRuleSegs(mergeRuleSegs(vs))
	uf := newUnionFind(len(hs) + len(vs))
	for i, h := range hs {
		for j, v := range vs {
			if v.pos >= h.a0-ruleJoinTol && v.pos <= h.a1+ruleJoinTol && h.pos >= v.a0-ruleJoinTol && h.pos <= v.a1+ruleJoinTol {
				uf.union(i, len(hs)+j)
			}
		}
	}
	type group struct{ h, v []ruleSeg }
	groups := map[int]*group{}
	var roots []int
	for i := range hs {
		r := uf.find(i)
		if groups[r] == nil {
			groups[r] = &group{}
			roots = append(roots, r)
		}
		groups[r].h = append(groups[r].h, hs[i])
	}
	for j := range vs {
		r := uf.find(len(hs) + j)
		if groups[r] == nil {
			groups[r] = &group{}
			roots = append(roots, r)
		}
		groups[r].v = append(groups[r].v, vs[j])
	}
	sort.Ints(roots)
	var tables []*pdfTable
	for _, r := range roots {
		if g := groups[r]; len(g.h) >= 2 && len(g.v) >= 2 {
			if t := buildRuledTable(out, g.h, g.v, used); t != nil {
				tables = append(tables, t)
			}
		}
	}
	sort.SliceStable(tables, func(i, j int) bool { return tables[i].top > tables[j].top })
	return tables, used
}

type ruledCell struct {
	r0, r1, c0, c1 int
	glyphs         []int
	idx            int // tableBuilder 中的单元格下标，-1 表示尚未放置
}

// buildRuledTable 按一组相连框线构造表格；网格不足两个单元格、单元格全部连通（外框）或没有文字时返回 nil。
func buildRuledTable(out *pdfPageOut, hs, vs []ruleSeg, used []bool) *pdfTable {
	xv := make([]float64, 0, len(vs))
	for _, v := range vs {
		xv = append(xv, v.pos)
	}
	yv := make([]float64, 0, len(hs))
	for _, h := range hs {
		yv = append(yv, h.pos)
	}
	xs := clusterPos(xv, ruleGridTol)
	ys := clusterPos(yv, ruleGridTol)
	for i, j := 0, len(ys)-1; i < j; i, j = i+1, j-1 { // 自上而下
		ys[i], ys[j] = ys[j], ys[i]
	}
	R, C := len(ys)-1, len(xs)-1
	if R < 1 || C < 2 || xs[C]-xs[0] < 40 || ys[0]-ys[R] < 10 {
		return nil
	}
	near := func(a, b float64) bool { return math.Abs(a-b) <= ruleGridTol }
	hasV := func(xi, r int) bool {
		mid := (ys[r] + ys[r+1]) / 2
		for _, v := range vs {
			if near(v.pos, xs[xi]) && v.a0-1 <= mid && mid <= v.a1+1 {
				return true
			}
		}
		return false
	}
	hasH := func(yi, c int) bool {
		mid := (xs[c] + xs[c+1]) / 2
		for _, h := range hs {
			if near(h.pos, ys[yi]) && h.a0-1 <= mid && mid <= h.a1+1 {
				return true
			}
		}
		return false
	}
	uf := newUnionFind(R * C)
	for r := 0; r < R; r++ {
		for c := 0; c < C; c++ {
			if c+1 < C && !hasV(c+1, r) {
				uf.union(r*C+c, r*C+c+1)
			}
			if r+1 < R && !hasH(r+1, c) {
				uf.union(r*C+c, (r+1)*C+c)
			}
		}
	}
	cells := map[int]*ruledCell{}
	for r := 0; r < R; r++ {
		for c := 0; c < C; c++ {
			root := uf.find(r*C + c)
			if cl, ok := cells[root]; ok {
				cl.r0, cl.r1, cl.c0, cl.c1 = min(cl.r0, r), max(cl.r1, r), min(cl.c0, c), max(cl.c1, c)
			} else {
				cells[root] = &ruledCell{r0: r, r1: r, c0: c, c1: c, idx: -1}
			}
		}
	}
	if len(cells) < 2 {
		return nil
	}
	var assigned []int
	for gi, g := range out.glyphs {
		if used[gi] {
			continue
		}
		cx, cy := g.x+g.w/2, g.y+g.size*0.3
		if cx < xs[0]-1 || cx > xs[C]+1 || cy > ys[0]+1 || cy < ys[R]-1 {
			continue
		}
		c := min(sort.Search(C, func(i int) bool { return xs[i+1] > cx }), C-1)
		r := min(sort.Search(R, func(i int) bool { return ys[i+1] < cy }), R-1)
		cl := cells[uf.find(r*C+c)]
		cl.glyphs = append(cl.glyphs, gi)
		assigned = append(assigned, gi)
	}
	if len(assigned) == 0 {
		return nil
	}
	for _, gi := range assigned {
		used[gi] = true
	}
	b := newTableBuilder()
	for r := 0; r < R; r++ {
		ri := b.startRow(out.num)
		for c := 0; c < C; {
			if _, taken := b.occupied[[2]int{ri, c}]; taken {
				c++
				continue
			}
			cl := cells[uf.find(r*C+c)]
			span := max(cl.c1-c+1, 1)
			if cl.idx >= 0 {
				prev := b.cells[cl.idx]
				if prev.Col == c && prev.Row+prev.RowSpan == ri {
					if _, ok := b.extendDown(ri, c); ok {
						c += prev.ColSpan
						continue
					}
				}
				b.add(ri, c, span, "") // 不规则合并区域：只在首次出现处放置文字
				c += span
				continue
			}
			cl.idx = b.add(ri, c, span, cellText(out, cl.glyphs))
			c += span
		}
	}
	pt := &pdfTable{t: b.build(1, "pdf"), cols: append([]float64(nil), xs[:C]...), top: ys[0],
		box: []BBox{{Page: out.num, X0: round2(xs[0]), Y0: round2(ys[R]), X1: round2(xs[C]), Y1: round2(ys[0])}}}
	return pt
}

// reCellBreak 匹配单元格内应另起一行的行首：列表编号、圈号、注、★ 等（「1.0」这类小数不算编号）。
var reCellBreak = regexp.MustCompile(`^(?:\d{1,2}(?:[、)）]|[.．](?:\D|$))|[（(]\d{1,2}[)）]|[①-⑳]|[一二三四五六七八九十]{1,3}[、.．]|注[:：]|[★▲※])`)

// cellText 把单元格内字形按行还原：句末标点或下一行以编号开头时换行，否则视为折行直接续接。
func cellText(out *pdfPageOut, idx []int) string {
	if len(idx) == 0 {
		return ""
	}
	tmp := &pdfPageOut{num: out.num, box: out.box, glyphs: make([]glyphPos, 0, len(idx))}
	for _, i := range idx {
		tmp.glyphs = append(tmp.glyphs, out.glyphs[i])
	}
	var (
		b    strings.Builder
		prev string
	)
	for _, l := range buildLayoutLines(tmp) {
		t := strings.TrimSpace(l.text())
		if t == "" {
			continue
		}
		if prev != "" {
			pr, _ := utf8.DecodeLastRuneInString(prev)
			nr, _ := utf8.DecodeRuneInString(t)
			switch {
			case reCellBreak.MatchString(t) || strings.ContainsRune("。；：:;！？!?", pr):
				b.WriteByte('\n')
			case !isCJK(pr) && !isCJK(nr):
				b.WriteByte(' ')
			}
		}
		b.WriteString(t)
		prev = t
	}
	return b.String()
}

// withoutGlyphs 返回去掉已被表格占用字形的页面副本（框线与图片不再需要）。
func withoutGlyphs(out *pdfPageOut, used []bool) *pdfPageOut {
	cp := &pdfPageOut{num: out.num, box: out.box, glyphs: make([]glyphPos, 0, len(out.glyphs))}
	for i, g := range out.glyphs {
		if !used[i] {
			cp.glyphs = append(cp.glyphs, g)
		}
	}
	return cp
}
