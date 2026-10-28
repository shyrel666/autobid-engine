package ingest

// 最小 PDF 对象层：词法/对象解析、xref 表与 xref 流（含 /Prev 链）、对象流（ObjStm）、
// FlateDecode（含 PNG 预测器）/ ASCIIHex / ASCII85 解码；xref 损坏时回退为全文扫描 "n g obj" 重建索引。
// 不支持加密 PDF（返回 ErrUnsupportedFormat）。

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"encoding/ascii85"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
)

type (
	pdfName    string
	pdfKeyword string
	pdfString  []byte
	pdfArray   []any
	pdfDict    map[pdfName]any
	pdfRef     struct{ num, gen int }
	pdfStream  struct {
		dict pdfDict
		raw  []byte
	}
)

var errPDFSyntax = errors.New("pdf syntax error")

type pdfLexer struct {
	b   []byte
	pos int
}

func isPDFSpace(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t' || c == '\f' || c == 0
}

func isPDFDelim(c byte) bool {
	return c == '(' || c == ')' || c == '<' || c == '>' || c == '[' || c == ']' || c == '{' || c == '}' || c == '/' || c == '%'
}

func (l *pdfLexer) skipSpace() {
	for l.pos < len(l.b) {
		c := l.b[l.pos]
		if isPDFSpace(c) {
			l.pos++
			continue
		}
		if c == '%' {
			for l.pos < len(l.b) && l.b[l.pos] != '\n' && l.b[l.pos] != '\r' {
				l.pos++
			}
			continue
		}
		return
	}
}

func (l *pdfLexer) eof() bool { l.skipSpace(); return l.pos >= len(l.b) }

func (l *pdfLexer) regular() []byte {
	start := l.pos
	for l.pos < len(l.b) && !isPDFSpace(l.b[l.pos]) && !isPDFDelim(l.b[l.pos]) {
		l.pos++
	}
	return l.b[start:l.pos]
}

// next 读取一个对象或关键字；refs 为 true 时识别 "n g R" 间接引用。
func (l *pdfLexer) next(refs bool) (any, error) {
	l.skipSpace()
	if l.pos >= len(l.b) {
		return nil, io.EOF
	}
	c := l.b[l.pos]
	switch {
	case c == '/':
		l.pos++
		return pdfName(decodeName(l.regular())), nil
	case c == '(':
		return l.literalString()
	case c == '<':
		if l.pos+1 < len(l.b) && l.b[l.pos+1] == '<' {
			l.pos += 2
			return l.dict(refs)
		}
		return l.hexString()
	case c == '>':
		if l.pos+1 < len(l.b) && l.b[l.pos+1] == '>' {
			l.pos += 2
			return pdfKeyword(">>"), nil
		}
		l.pos++
		return nil, errPDFSyntax
	case c == '[':
		l.pos++
		var arr pdfArray
		for {
			v, err := l.next(refs)
			if err != nil {
				return arr, err
			}
			if k, ok := v.(pdfKeyword); ok && k == "]" {
				return arr, nil
			}
			arr = append(arr, v)
		}
	case c == ']':
		l.pos++
		return pdfKeyword("]"), nil
	case c == '{' || c == '}':
		l.pos++
		return pdfKeyword(string(c)), nil
	}
	tok := l.regular()
	if len(tok) == 0 {
		l.pos++
		return nil, errPDFSyntax
	}
	switch string(tok) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	}
	if isNumber(tok) {
		if bytes.ContainsAny(tok, ".") {
			f, _ := strconv.ParseFloat(string(tok), 64)
			return f, nil
		}
		n, err := strconv.ParseInt(string(tok), 10, 64)
		if err != nil {
			f, _ := strconv.ParseFloat(string(tok), 64)
			return f, nil
		}
		if refs && n >= 0 {
			save := l.pos
			if g, ok := l.peekInt(); ok {
				l.skipSpace()
				if l.pos < len(l.b) && l.b[l.pos] == 'R' && (l.pos+1 == len(l.b) || isPDFSpace(l.b[l.pos+1]) || isPDFDelim(l.b[l.pos+1])) {
					l.pos++
					return pdfRef{int(n), g}, nil
				}
			}
			l.pos = save
		}
		return n, nil
	}
	return pdfKeyword(tok), nil
}

func (l *pdfLexer) peekInt() (int, bool) {
	l.skipSpace()
	start := l.pos
	for l.pos < len(l.b) && l.b[l.pos] >= '0' && l.b[l.pos] <= '9' {
		l.pos++
	}
	if l.pos == start || (l.pos < len(l.b) && !isPDFSpace(l.b[l.pos]) && !isPDFDelim(l.b[l.pos])) {
		return 0, false
	}
	n, err := strconv.Atoi(string(l.b[start:l.pos]))
	return n, err == nil
}

func isNumber(tok []byte) bool {
	digits := 0
	for i, c := range tok {
		switch {
		case c >= '0' && c <= '9':
			digits++
		case (c == '+' || c == '-') && i == 0:
		case c == '.':
		default:
			return false
		}
	}
	return digits > 0
}

func decodeName(b []byte) string {
	if !bytes.ContainsRune(b, '#') {
		return string(b)
	}
	var out []byte
	for i := 0; i < len(b); i++ {
		if b[i] == '#' && i+2 < len(b) {
			if v, err := strconv.ParseUint(string(b[i+1:i+3]), 16, 8); err == nil {
				out = append(out, byte(v))
				i += 2
				continue
			}
		}
		out = append(out, b[i])
	}
	return string(out)
}

func (l *pdfLexer) literalString() (any, error) {
	l.pos++ // (
	var out []byte
	depth := 1
	for l.pos < len(l.b) {
		c := l.b[l.pos]
		l.pos++
		switch c {
		case '(':
			depth++
			out = append(out, c)
		case ')':
			depth--
			if depth == 0 {
				return pdfString(out), nil
			}
			out = append(out, c)
		case '\\':
			if l.pos >= len(l.b) {
				return pdfString(out), nil
			}
			e := l.b[l.pos]
			l.pos++
			switch e {
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case '\r':
				if l.pos < len(l.b) && l.b[l.pos] == '\n' {
					l.pos++
				}
			case '\n':
			default:
				if e >= '0' && e <= '7' {
					v := int(e - '0')
					for k := 0; k < 2 && l.pos < len(l.b) && l.b[l.pos] >= '0' && l.b[l.pos] <= '7'; k++ {
						v = v*8 + int(l.b[l.pos]-'0')
						l.pos++
					}
					out = append(out, byte(v))
				} else {
					out = append(out, e)
				}
			}
		default:
			out = append(out, c)
		}
	}
	return pdfString(out), nil
}

func (l *pdfLexer) hexString() (any, error) {
	l.pos++ // <
	var digits []byte
	for l.pos < len(l.b) && l.b[l.pos] != '>' {
		c := l.b[l.pos]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			digits = append(digits, c)
		}
		l.pos++
	}
	l.pos++
	if len(digits)%2 == 1 {
		digits = append(digits, '0')
	}
	out := make([]byte, len(digits)/2)
	_, _ = hex.Decode(out, digits)
	return pdfString(out), nil
}

func (l *pdfLexer) dict(refs bool) (any, error) {
	d := pdfDict{}
	for {
		k, err := l.next(refs)
		if err != nil {
			return d, err
		}
		if kw, ok := k.(pdfKeyword); ok && kw == ">>" {
			return d, nil
		}
		name, ok := k.(pdfName)
		if !ok {
			continue
		}
		v, err := l.next(refs)
		if err != nil {
			return d, err
		}
		if kw, ok := v.(pdfKeyword); ok && kw == ">>" {
			d[name] = nil
			return d, nil
		}
		d[name] = v
	}
}

// ---------------------------------------------------------------------------
// 文档与交叉引用
// ---------------------------------------------------------------------------

type xrefEntry struct {
	offset   int64
	inStream bool
	stream   int
	index    int
}

type pdfDoc struct {
	data    []byte
	xref    map[int]xrefEntry
	trailer pdfDict
	cache   map[int]any
	objStm  map[int]map[int]any
	loading map[int]bool
}

var reObjHeader = regexp.MustCompile(`(\d+)\s+(\d+)\s+obj\b`)

func openPDF(data []byte) (*pdfDoc, error) {
	if !bytes.HasPrefix(bytes.TrimLeft(data[:min(len(data), 1024)], "\x00\r\n\t "), []byte("%PDF")) && !bytes.Contains(data[:min(len(data), 1024)], []byte("%PDF")) {
		return nil, fmt.Errorf("%w: not a PDF file", ErrUnsupportedFormat)
	}
	d := &pdfDoc{data: data, xref: map[int]xrefEntry{}, cache: map[int]any{}, objStm: map[int]map[int]any{}, loading: map[int]bool{}}
	if err := d.loadXref(); err != nil || d.trailer == nil || d.trailer["Root"] == nil {
		d.rebuildXref()
	}
	if d.trailer["Encrypt"] != nil {
		return nil, fmt.Errorf("%w: encrypted PDF", ErrUnsupportedFormat)
	}
	if _, ok := d.resolve(d.trailer["Root"]).(pdfDict); !ok {
		return nil, fmt.Errorf("%w: PDF catalog not found", ErrUnsupportedFormat)
	}
	return d, nil
}

func (d *pdfDoc) loadXref() error {
	i := bytes.LastIndex(d.data, []byte("startxref"))
	if i < 0 {
		return errPDFSyntax
	}
	l := &pdfLexer{b: d.data, pos: i + len("startxref")}
	v, err := l.next(false)
	if err != nil {
		return err
	}
	off, ok := v.(int64)
	if !ok {
		return errPDFSyntax
	}
	seen := map[int64]bool{}
	for off > 0 && off < int64(len(d.data)) && !seen[off] {
		seen[off] = true
		trailer, err := d.readXrefAt(off)
		if err != nil {
			return err
		}
		if d.trailer == nil {
			d.trailer = trailer
		}
		if stm, ok := trailer["XRefStm"].(int64); ok && !seen[stm] {
			seen[stm] = true
			if _, err := d.readXrefAt(stm); err != nil {
				return err
			}
		}
		prev, ok := trailer["Prev"].(int64)
		if !ok {
			break
		}
		off = prev
	}
	return nil
}

// readXrefAt 读取 off 处的 xref 表或 xref 流；已有条目（更新版本）不被旧条目覆盖。
func (d *pdfDoc) readXrefAt(off int64) (pdfDict, error) {
	l := &pdfLexer{b: d.data, pos: int(off)}
	l.skipSpace()
	if bytes.HasPrefix(d.data[l.pos:], []byte("xref")) {
		l.pos += 4
		for {
			v, err := l.next(false)
			if err != nil {
				return nil, err
			}
			if kw, ok := v.(pdfKeyword); ok && kw == "trailer" {
				t, err := l.next(true)
				if err != nil {
					return nil, err
				}
				td, _ := t.(pdfDict)
				return td, nil
			}
			start, ok1 := v.(int64)
			cv, err := l.next(false)
			count, ok2 := cv.(int64)
			if err != nil || !ok1 || !ok2 {
				return nil, errPDFSyntax
			}
			for k := int64(0); k < count; k++ {
				ov, _ := l.next(false)
				_, _ = l.next(false)
				kind, _ := l.next(false)
				o, _ := ov.(int64)
				num := int(start + k)
				if kw, _ := kind.(pdfKeyword); kw == "n" {
					if _, exists := d.xref[num]; !exists {
						d.xref[num] = xrefEntry{offset: o}
					}
				} else if _, exists := d.xref[num]; !exists {
					d.xref[num] = xrefEntry{offset: -1}
				}
			}
		}
	}
	obj, _, err := d.parseIndirectAt(off)
	if err != nil {
		return nil, err
	}
	st, ok := obj.(*pdfStream)
	if !ok || st.dict["Type"] != pdfName("XRef") {
		return nil, errPDFSyntax
	}
	data, err := d.decodeStream(st)
	if err != nil {
		return nil, err
	}
	w := toInts(d.resolve(st.dict["W"]))
	if len(w) != 3 {
		return nil, errPDFSyntax
	}
	index := toInts(d.resolve(st.dict["Index"]))
	if len(index) == 0 {
		size, _ := d.resolve(st.dict["Size"]).(int64)
		index = []int{0, int(size)}
	}
	rowLen := w[0] + w[1] + w[2]
	field := func(row []byte, from, n int, def int64) int64 {
		if n == 0 {
			return def
		}
		var v int64
		for _, c := range row[from : from+n] {
			v = v<<8 | int64(c)
		}
		return v
	}
	pos := 0
	for i := 0; i+1 < len(index); i += 2 {
		for k := 0; k < index[i+1]; k++ {
			if pos+rowLen > len(data) {
				break
			}
			row := data[pos : pos+rowLen]
			pos += rowLen
			num := index[i] + k
			if _, exists := d.xref[num]; exists {
				continue
			}
			switch field(row, 0, w[0], 1) {
			case 1:
				d.xref[num] = xrefEntry{offset: field(row, w[0], w[1], 0)}
			case 2:
				d.xref[num] = xrefEntry{inStream: true, stream: int(field(row, w[0], w[1], 0)), index: int(field(row, w[0]+w[1], w[2], 0))}
			default:
				d.xref[num] = xrefEntry{offset: -1}
			}
		}
	}
	return st.dict, nil
}

// rebuildXref 扫描全文的 "n g obj" 重建索引（后出现的覆盖先出现的），并从对象流与 trailer 中补全。
func (d *pdfDoc) rebuildXref() {
	d.xref = map[int]xrefEntry{}
	d.cache = map[int]any{}
	for _, m := range reObjHeader.FindAllSubmatchIndex(d.data, -1) {
		if m[0] > 0 && !isPDFSpace(d.data[m[0]-1]) && !isPDFDelim(d.data[m[0]-1]) {
			continue
		}
		num, _ := strconv.Atoi(string(d.data[m[2]:m[3]]))
		d.xref[num] = xrefEntry{offset: int64(m[0])}
	}
	if d.trailer == nil {
		d.trailer = pdfDict{}
	}
	if i := bytes.LastIndex(d.data, []byte("trailer")); i >= 0 {
		l := &pdfLexer{b: d.data, pos: i + len("trailer")}
		if t, err := l.next(true); err == nil {
			if td, ok := t.(pdfDict); ok {
				for k, v := range td {
					d.trailer[k] = v
				}
			}
		}
	}
	for num := range d.xref {
		st, ok := d.getObject(num).(*pdfStream)
		if !ok {
			continue
		}
		switch st.dict["Type"] {
		case pdfName("ObjStm"):
			for inner := range d.loadObjStm(num) {
				if _, exists := d.xref[inner]; !exists {
					d.xref[inner] = xrefEntry{inStream: true, stream: num}
				}
			}
		case pdfName("XRef"):
			if d.trailer["Root"] == nil {
				d.trailer["Root"] = st.dict["Root"]
			}
		}
	}
	if d.trailer["Root"] == nil {
		for num := range d.xref {
			if dict, ok := d.getObject(num).(pdfDict); ok && dict["Type"] == pdfName("Catalog") {
				d.trailer["Root"] = pdfRef{num: num}
				break
			}
		}
	}
}

// parseIndirectAt 解析 off 处的 "n g obj ... endobj"。
func (d *pdfDoc) parseIndirectAt(off int64) (any, int, error) {
	if off < 0 || off >= int64(len(d.data)) {
		return nil, 0, errPDFSyntax
	}
	l := &pdfLexer{b: d.data, pos: int(off)}
	nv, err := l.next(false)
	if err != nil {
		return nil, 0, err
	}
	num, ok := nv.(int64)
	if !ok {
		return nil, 0, errPDFSyntax
	}
	_, _ = l.next(false)
	if kw, _ := l.next(false); kw != pdfKeyword("obj") {
		return nil, 0, errPDFSyntax
	}
	obj, err := l.next(true)
	if err != nil {
		return nil, 0, err
	}
	dict, isDict := obj.(pdfDict)
	if !isDict {
		return obj, int(num), nil
	}
	save := l.pos
	if kw, _ := l.next(false); kw != pdfKeyword("stream") {
		l.pos = save
		return dict, int(num), nil
	}
	// stream 关键字后紧跟 CRLF 或 LF。
	if l.pos < len(d.data) && d.data[l.pos] == '\r' {
		l.pos++
	}
	if l.pos < len(d.data) && d.data[l.pos] == '\n' {
		l.pos++
	}
	start := l.pos
	end := -1
	if n, ok := d.resolveLength(dict["Length"]); ok && n >= 0 && start+n <= len(d.data) {
		tail := bytes.TrimLeft(d.data[start+n:min(start+n+32, len(d.data))], "\r\n \t")
		if bytes.HasPrefix(tail, []byte("endstream")) {
			end = start + n
		}
	}
	if end < 0 {
		i := bytes.Index(d.data[start:], []byte("endstream"))
		if i < 0 {
			return nil, 0, errPDFSyntax
		}
		end = start + i
		for end > start && (d.data[end-1] == '\n' || d.data[end-1] == '\r') {
			end--
		}
	}
	return &pdfStream{dict: dict, raw: d.data[start:end]}, int(num), nil
}

func (d *pdfDoc) resolveLength(v any) (int, bool) {
	if r, ok := v.(pdfRef); ok {
		if d.loading[r.num] {
			return 0, false
		}
		v = d.getObject(r.num)
	}
	n, ok := v.(int64)
	return int(n), ok
}

func (d *pdfDoc) getObject(num int) any {
	if v, ok := d.cache[num]; ok {
		return v
	}
	e, ok := d.xref[num]
	if !ok || d.loading[num] {
		return nil
	}
	d.loading[num] = true
	defer delete(d.loading, num)
	var obj any
	if e.inStream {
		obj = d.loadObjStm(e.stream)[num]
	} else if e.offset >= 0 {
		v, got, err := d.parseIndirectAt(e.offset)
		if err == nil && got == num {
			obj = v
		}
	}
	d.cache[num] = obj
	return obj
}

func (d *pdfDoc) loadObjStm(num int) map[int]any {
	if m, ok := d.objStm[num]; ok {
		return m
	}
	m := map[int]any{}
	d.objStm[num] = m
	st, ok := d.getObject(num).(*pdfStream)
	if !ok {
		return m
	}
	data, err := d.decodeStream(st)
	if err != nil {
		return m
	}
	n, _ := d.resolve(st.dict["N"]).(int64)
	first, _ := d.resolve(st.dict["First"]).(int64)
	l := &pdfLexer{b: data}
	type pair struct{ num, off int }
	pairs := make([]pair, 0, n)
	for i := int64(0); i < n; i++ {
		a, _ := l.next(false)
		b, _ := l.next(false)
		an, _ := a.(int64)
		bn, _ := b.(int64)
		pairs = append(pairs, pair{int(an), int(bn)})
	}
	for _, p := range pairs {
		pos := int(first) + p.off
		if pos < 0 || pos >= len(data) {
			continue
		}
		ol := &pdfLexer{b: data, pos: pos}
		if v, err := ol.next(true); err == nil {
			m[p.num] = v
		}
	}
	return m
}

func (d *pdfDoc) resolve(v any) any {
	for range 32 {
		r, ok := v.(pdfRef)
		if !ok {
			return v
		}
		v = d.getObject(r.num)
	}
	return nil
}

func (d *pdfDoc) dictOf(v any) pdfDict {
	switch t := d.resolve(v).(type) {
	case pdfDict:
		return t
	case *pdfStream:
		return t.dict
	}
	return nil
}

func toInts(v any) []int {
	arr, _ := v.(pdfArray)
	out := make([]int, 0, len(arr))
	for _, x := range arr {
		out = append(out, int(pdfNum(x)))
	}
	return out
}

func pdfNum(v any) float64 {
	switch t := v.(type) {
	case int64:
		return float64(t)
	case float64:
		return t
	}
	return 0
}

// ---------------------------------------------------------------------------
// 流解码
// ---------------------------------------------------------------------------

// errImageFilter 表示流使用图像压缩（DCT/JPX/CCITT/JBIG2），原样返回已解码到该滤镜之前的数据。
var errImageFilter = errors.New("pdf image filter")

func (d *pdfDoc) decodeStream(st *pdfStream) ([]byte, error) {
	data, _, err := d.decodeStreamUntilImage(st)
	return data, err
}

// decodeStreamUntilImage 依次应用滤镜；遇到图像滤镜时停止并返回该滤镜名（数据即为该格式的原始字节）。
func (d *pdfDoc) decodeStreamUntilImage(st *pdfStream) ([]byte, string, error) {
	data := st.raw
	var filters []pdfName
	switch f := d.resolve(st.dict["Filter"]).(type) {
	case pdfName:
		filters = []pdfName{f}
	case pdfArray:
		for _, x := range f {
			if n, ok := d.resolve(x).(pdfName); ok {
				filters = append(filters, n)
			}
		}
	}
	var params []pdfDict
	switch p := d.resolve(st.dict["DecodeParms"]).(type) {
	case pdfDict:
		params = []pdfDict{p}
	case pdfArray:
		for _, x := range p {
			params = append(params, d.dictOf(x))
		}
	}
	for i, f := range filters {
		var parm pdfDict
		if i < len(params) {
			parm = params[i]
		}
		var err error
		switch f {
		case "FlateDecode", "Fl":
			data, err = inflate(data)
			if err == nil {
				data, err = d.applyPredictor(data, parm)
			}
		case "ASCIIHexDecode", "AHx":
			data, err = asciiHex(data)
		case "ASCII85Decode", "A85":
			data, err = ascii85Decode(data)
		case "DCTDecode", "DCT", "JPXDecode", "CCITTFaxDecode", "CCF", "JBIG2Decode":
			return data, string(f), errImageFilter
		default:
			return nil, string(f), fmt.Errorf("unsupported PDF filter %s", f)
		}
		if err != nil {
			return nil, string(f), err
		}
	}
	return data, "", nil
}

func inflate(data []byte) ([]byte, error) {
	if r, err := zlib.NewReader(bytes.NewReader(data)); err == nil {
		out, err := io.ReadAll(r)
		if err == nil || len(out) > 0 {
			return out, nil
		}
	}
	out, err := io.ReadAll(flate.NewReader(bytes.NewReader(data)))
	if err != nil && len(out) == 0 {
		return nil, err
	}
	return out, nil
}

func (d *pdfDoc) applyPredictor(data []byte, parm pdfDict) ([]byte, error) {
	if parm == nil {
		return data, nil
	}
	pred := int(pdfNum(d.resolve(parm["Predictor"])))
	if pred < 10 {
		return data, nil
	}
	cols := int(pdfNum(d.resolve(parm["Columns"])))
	colors := int(pdfNum(d.resolve(parm["Colors"])))
	bpc := int(pdfNum(d.resolve(parm["BitsPerComponent"])))
	cols, colors, bpc = max(cols, 1), max(colors, 1), max(bpc, 8)
	bpp := max(colors*bpc/8, 1)
	rowLen := (cols*colors*bpc + 7) / 8
	var out []byte
	prev := make([]byte, rowLen)
	for pos := 0; pos+1+rowLen <= len(data); pos += 1 + rowLen {
		ft := data[pos]
		row := append([]byte(nil), data[pos+1:pos+1+rowLen]...)
		for i := range row {
			var left, up, upLeft byte
			if i >= bpp {
				left, upLeft = row[i-bpp], prev[i-bpp]
			}
			up = prev[i]
			switch ft {
			case 1:
				row[i] += left
			case 2:
				row[i] += up
			case 3:
				row[i] += byte((int(left) + int(up)) / 2)
			case 4:
				row[i] += paeth(left, up, upLeft)
			}
		}
		out = append(out, row...)
		prev = row
	}
	return out, nil
}

func paeth(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	pa, pb, pc := abs(p-int(a)), abs(p-int(b)), abs(p-int(c))
	switch {
	case pa <= pb && pa <= pc:
		return a
	case pb <= pc:
		return b
	}
	return c
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func asciiHex(data []byte) ([]byte, error) {
	var digits []byte
	for _, c := range data {
		if c == '>' {
			break
		}
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			digits = append(digits, c)
		}
	}
	if len(digits)%2 == 1 {
		digits = append(digits, '0')
	}
	out := make([]byte, len(digits)/2)
	_, err := hex.Decode(out, digits)
	return out, err
}

func ascii85Decode(data []byte) ([]byte, error) {
	data = bytes.TrimSpace(data)
	data = bytes.TrimPrefix(data, []byte("<~"))
	if i := bytes.Index(data, []byte("~>")); i >= 0 {
		data = data[:i]
	}
	out := make([]byte, len(data)*4/5+4)
	n, _, err := ascii85.Decode(out, data, true)
	return out[:n], err
}
