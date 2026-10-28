package ingest

// PDF 字体文本解码：Type0（CID）字体按 ToUnicode CMap / 预定义 UCS2 CMap 解码、/W /DW 取字宽；
// 简单字体按 ToUnicode 或 WinAnsi + /Differences 解码、/Widths 取字宽。

import (
	"strconv"
	"strings"
	"unicode/utf16"
)

type pdfFont struct {
	cid       bool
	codeBytes []int // 码空间各范围的字节长度（CID 字体），从长到短匹配
	ranges    []codeRange
	toUni     map[uint32]string
	identity2 bool // 预定义 UCS2/UTF16 CMap：码值即 Unicode
	simple    [256]rune
	widths    map[uint32]float64
	dw        float64
	hasToUni  bool
}

type codeRange struct {
	lo, hi uint32
	n      int
}

func (d *pdfDoc) loadFont(v any) *pdfFont {
	fd := d.dictOf(v)
	f := &pdfFont{toUni: map[uint32]string{}, widths: map[uint32]float64{}, dw: 1000}
	if fd == nil {
		f.initWinAnsi()
		f.dw = 500
		return f
	}
	if st, ok := d.resolve(fd["ToUnicode"]).(*pdfStream); ok {
		if data, err := d.decodeStream(st); err == nil {
			f.parseCMap(data)
			f.hasToUni = len(f.toUni) > 0
		}
	}
	if fd["Subtype"] == pdfName("Type0") {
		f.cid = true
		if enc, ok := d.resolve(fd["Encoding"]).(pdfName); ok {
			s := string(enc)
			if strings.Contains(s, "UCS2") || strings.Contains(s, "UTF16") {
				f.identity2 = true
			}
		}
		if len(f.ranges) == 0 {
			f.ranges = []codeRange{{0, 0xFFFF, 2}}
		}
		if arr, ok := d.resolve(fd["DescendantFonts"]).(pdfArray); ok && len(arr) > 0 {
			desc := d.dictOf(arr[0])
			if dw, ok := d.resolve(desc["DW"]).(int64); ok {
				f.dw = float64(dw)
			} else if dw, ok := d.resolve(desc["DW"]).(float64); ok {
				f.dw = dw
			}
			f.parseW(d, d.resolve(desc["W"]))
		}
		return f
	}
	f.initWinAnsi()
	f.dw = 500
	switch enc := d.resolve(fd["Encoding"]).(type) {
	case pdfDict:
		if diff, ok := d.resolve(enc["Differences"]).(pdfArray); ok {
			code := 0
			for _, x := range diff {
				switch t := d.resolve(x).(type) {
				case int64:
					code = int(t)
				case pdfName:
					if code >= 0 && code < 256 {
						if r := glyphRune(string(t)); r != 0 {
							f.simple[code] = r
						}
					}
					code++
				}
			}
		}
	}
	first := int(pdfNum(d.resolve(fd["FirstChar"])))
	if ws, ok := d.resolve(fd["Widths"]).(pdfArray); ok {
		for i, w := range ws {
			f.widths[uint32(first+i)] = pdfNum(d.resolve(w))
		}
	}
	return f
}

func (f *pdfFont) parseW(d *pdfDoc, v any) {
	arr, _ := v.(pdfArray)
	for i := 0; i < len(arr); {
		first := uint32(pdfNum(d.resolve(arr[i])))
		if i+1 >= len(arr) {
			return
		}
		if list, ok := d.resolve(arr[i+1]).(pdfArray); ok {
			for k, w := range list {
				f.widths[first+uint32(k)] = pdfNum(d.resolve(w))
			}
			i += 2
			continue
		}
		if i+2 >= len(arr) {
			return
		}
		last := uint32(pdfNum(d.resolve(arr[i+1])))
		w := pdfNum(d.resolve(arr[i+2]))
		for c := first; c <= last && c-first < 65536; c++ {
			f.widths[c] = w
		}
		i += 3
	}
}

// parseCMap 解析 ToUnicode CMap 的 codespacerange / bfchar / bfrange。
func (f *pdfFont) parseCMap(data []byte) {
	l := &pdfLexer{b: data}
	var stack []any
	for {
		v, err := l.next(false)
		if err != nil {
			break
		}
		kw, ok := v.(pdfKeyword)
		if !ok {
			stack = append(stack, v)
			continue
		}
		switch kw {
		case "endcodespacerange":
			for i := 0; i+1 < len(stack); i += 2 {
				lo, _ := stack[i].(pdfString)
				hi, _ := stack[i+1].(pdfString)
				if len(lo) > 0 && len(lo) == len(hi) {
					f.ranges = append(f.ranges, codeRange{beUint(lo), beUint(hi), len(lo)})
				}
			}
		case "endbfchar":
			for i := 0; i+1 < len(stack); i += 2 {
				src, _ := stack[i].(pdfString)
				switch dst := stack[i+1].(type) {
				case pdfString:
					f.toUni[beUint(src)] = utf16BE(dst)
				case pdfName:
					if r := glyphRune(string(dst)); r != 0 {
						f.toUni[beUint(src)] = string(r)
					}
				}
			}
		case "endbfrange":
			for i := 0; i+2 < len(stack); i += 3 {
				lo, _ := stack[i].(pdfString)
				hi, _ := stack[i+1].(pdfString)
				a, b := beUint(lo), beUint(hi)
				if b < a || b-a > 65535 {
					continue
				}
				switch dst := stack[i+2].(type) {
				case pdfString:
					base := append([]byte(nil), dst...)
					for c := a; c <= b; c++ {
						f.toUni[c] = utf16BE(base)
						incLast(base)
					}
				case pdfArray:
					for k, x := range dst {
						if s, ok := x.(pdfString); ok && a+uint32(k) <= b {
							f.toUni[a+uint32(k)] = utf16BE(s)
						}
					}
				}
			}
		}
		if strings.HasPrefix(string(kw), "begin") || strings.HasPrefix(string(kw), "end") {
			stack = stack[:0]
		}
	}
}

func beUint(b []byte) uint32 {
	var v uint32
	for _, c := range b {
		v = v<<8 | uint32(c)
	}
	return v
}

func incLast(b []byte) {
	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 {
			return
		}
	}
}

func utf16BE(b []byte) string {
	if len(b) == 1 {
		return string(rune(b[0]))
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
	}
	return string(utf16.Decode(u))
}

type pdfGlyph struct {
	code  uint32
	text  string
	width float64 // 1/1000 文字空间单位
	space bool    // 单字节码 32（适用字间距 Tw）
}

// decode 把显示字符串拆分为字形码并映射为 Unicode 文本与字宽。
func (f *pdfFont) decode(s []byte) []pdfGlyph {
	var out []pdfGlyph
	for i := 0; i < len(s); {
		n := 1
		if f.cid {
			n = 0
			for _, r := range f.ranges {
				if i+r.n <= len(s) {
					c := beUint(s[i : i+r.n])
					if c >= r.lo && c <= r.hi && r.n > n {
						n = r.n
					}
				}
			}
			if n == 0 {
				n = min(2, len(s)-i)
			}
		}
		code := beUint(s[i : i+n])
		i += n
		g := pdfGlyph{code: code, space: n == 1 && code == 32}
		switch {
		case f.hasToUni:
			g.text = f.toUni[code]
			if g.text == "" && !f.cid && code < 256 && f.simple[code] != 0 {
				g.text = string(f.simple[code])
			}
		case f.cid && f.identity2:
			g.text = string(rune(code))
		case !f.cid:
			if r := f.simple[code&0xFF]; r != 0 {
				g.text = string(r)
			}
		}
		if w, ok := f.widths[code]; ok {
			g.width = w
		} else {
			g.width = f.dw
		}
		out = append(out, g)
	}
	return out
}

// winAnsiHigh 是 cp1252 0x80–0x9F 区间的映射。
var winAnsiHigh = [32]rune{
	0x20AC, 0, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021, 0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0, 0x017D, 0,
	0, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014, 0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0, 0x017E, 0x0178,
}

func (f *pdfFont) initWinAnsi() {
	for c := 32; c < 256; c++ {
		switch {
		case c >= 0x80 && c < 0xA0:
			f.simple[c] = winAnsiHigh[c-0x80]
		case c == 0x7F:
		default:
			f.simple[c] = rune(c)
		}
	}
	f.simple['\t'], f.simple['\n'], f.simple['\r'] = ' ', ' ', ' '
}

var glyphNames = map[string]rune{
	"space": ' ', "exclam": '!', "quotedbl": '"', "numbersign": '#', "dollar": '$', "percent": '%', "ampersand": '&',
	"quotesingle": '\'', "parenleft": '(', "parenright": ')', "asterisk": '*', "plus": '+', "comma": ',', "hyphen": '-',
	"period": '.', "slash": '/', "zero": '0', "one": '1', "two": '2', "three": '3', "four": '4', "five": '5', "six": '6',
	"seven": '7', "eight": '8', "nine": '9', "colon": ':', "semicolon": ';', "less": '<', "equal": '=', "greater": '>',
	"question": '?', "at": '@', "bracketleft": '[', "backslash": '\\', "bracketright": ']', "asciicircum": '^',
	"underscore": '_', "grave": '`', "braceleft": '{', "bar": '|', "braceright": '}', "asciitilde": '~',
	"quoteright": '’', "quoteleft": '‘', "quotedblleft": '“', "quotedblright": '”', "endash": '–', "emdash": '—',
	"bullet": '•', "ellipsis": '…', "degree": '°', "plusminus": '±', "multiply": '×', "divide": '÷', "copyright": '©',
	"registered": '®', "trademark": '™', "Euro": '€', "fi": 'ﬁ', "fl": 'ﬂ', "minus": '−', "periodcentered": '·',
}

// glyphRune 把字形名映射为字符：常用名表、单字母名、uniXXXX / uXXXX[XX]。
func glyphRune(name string) rune {
	if r, ok := glyphNames[name]; ok {
		return r
	}
	if len(name) == 1 {
		return rune(name[0])
	}
	if strings.HasPrefix(name, "uni") && len(name) >= 7 {
		if v, err := strconv.ParseUint(name[3:7], 16, 32); err == nil {
			return rune(v)
		}
	}
	if strings.HasPrefix(name, "u") && len(name) >= 5 && len(name) <= 7 {
		if v, err := strconv.ParseUint(name[1:], 16, 32); err == nil {
			return rune(v)
		}
	}
	return 0
}
