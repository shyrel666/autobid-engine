// Package compliance 是 Phase 6A 的确定性规则层：逐条正文实质响应核验（Verifier / RuleVerifier）与
// 全文关键承诺抽取（Extract）。本包不访问数据库，输入输出均为纯数据；db 包负责加锁读取修订、
// 写入 compliance_matrix / consistency_findings 并承担失效与并发门禁。
//
// 规则抽取与规则核验的召回率 / 准确率未经人工标注评测，结果只用于提示与门禁，不能替代人工复核。
package compliance

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// 量纲。
const (
	DimDuration = "duration" // 归一到天
	DimTime     = "time"     // 归一到分钟
	DimPercent  = "percent"
	DimCount    = "count" // 人 / 名
	DimItems    = "items" // 台 / 套 / 个 ...
)

// 比较方向。
const (
	CmpEQ = "="
	CmpGE = ">="
	CmpLE = "<="
	CmpGT = ">"
	CmpLT = "<"
)

// Quantity 是一个带单位与比较词的数值（Value 已按量纲归一）。
type Quantity struct {
	Raw   string  `json:"raw"`
	Cmp   string  `json:"cmp"`
	Value float64 `json:"value"`
	Dim   string  `json:"dim"`
	Unit  string  `json:"unit"`
	Start int     `json:"-"` // 在原文中的字节偏移（含比较词）
	End   int     `json:"-"`
}

const numPat = `(\d+(?:\.\d+)?|[零〇一二两三四五六七八九十百千万]+)`

const cmpPat = `(不少于|不低于|不小于|不短于|不得少于|不得低于|至少|最少|≥|>=|不超过|不大于|不多于|不长于|不高于|不得超过|最多|最长|≤|<=|小于|少于|低于|短于|<|大于|多于|高于|超过|长于|>)?\s*`

var quantityRe = regexp.MustCompile(cmpPat + numPat + `\s*(个日历天|日历天|个工作日|工作日|个自然日|自然日|天|日|个星期|星期|周|个月|月|年|个小时|小时|h\b|H\b|分钟|min\b|%|％|人|名|台|套|块|个|路|节点|组)`)

var cnDigit = map[rune]float64{'零': 0, '〇': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
var cnUnit = map[rune]float64{'十': 10, '百': 100, '千': 1000, '万': 10000}

// ParseNumber 解析阿拉伯数字或中文数字（十二、二十五、一百零五、三千、两万）。
func ParseNumber(s string) (float64, bool) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", ""))
	if s == "" {
		return 0, false
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return v, true
	}
	var total, section, digit float64
	seen := false
	for _, r := range s {
		if d, ok := cnDigit[r]; ok {
			digit, seen = d, true
			continue
		}
		u, ok := cnUnit[r]
		if !ok {
			return 0, false
		}
		seen = true
		if digit == 0 && u == 10 {
			digit = 1 // 「十二」
		}
		if u == 10000 {
			total += (section + digit) * u
			section = 0
		} else {
			section += digit * u
		}
		digit = 0
	}
	return total + section + digit, seen
}

func normCmp(c string) string {
	switch c {
	case "不少于", "不低于", "不小于", "不短于", "不得少于", "不得低于", "至少", "最少", "≥", ">=":
		return CmpGE
	case "不超过", "不大于", "不多于", "不长于", "不高于", "不得超过", "最多", "最长", "≤", "<=":
		return CmpLE
	case "小于", "少于", "低于", "短于", "<":
		return CmpLT
	case "大于", "多于", "高于", "超过", "长于", ">":
		return CmpGT
	}
	return CmpEQ
}

func unitDim(u string) (dim string, factor float64) {
	switch u {
	case "个日历天", "日历天", "个工作日", "工作日", "个自然日", "自然日", "天", "日":
		return DimDuration, 1
	case "个星期", "星期", "周":
		return DimDuration, 7
	case "个月", "月":
		return DimDuration, 30
	case "年":
		return DimDuration, 365
	case "个小时", "小时", "h", "H":
		return DimTime, 60
	case "分钟", "min":
		return DimTime, 1
	case "%", "％":
		return DimPercent, 1
	case "人", "名":
		return DimCount, 1
	}
	return DimItems, 1
}

// Quantities 抽取文本中的全部数值。「7×24 小时」这类服务时段表述不当作时限；
// 「以内 / 内」紧随其后的数值视为 ≤。
func Quantities(text string) []Quantity {
	var out []Quantity
	for _, m := range quantityRe.FindAllStringSubmatchIndex(text, -1) {
		raw := text[m[0]:m[1]]
		cmp := ""
		if m[2] >= 0 {
			cmp = text[m[2]:m[3]]
		}
		v, ok := ParseNumber(text[m[4]:m[5]])
		if !ok {
			continue
		}
		unit := text[m[6]:m[7]]
		if prev := lastRune(text[:m[4]]); prev == '×' || prev == 'x' || prev == 'X' || prev == '*' {
			continue
		}
		if next := firstRune(text[m[1]:]); next == '×' || next == '*' {
			continue
		}
		dim, f := unitDim(unit)
		q := Quantity{Raw: strings.TrimSpace(raw), Cmp: normCmp(cmp), Value: v * f, Dim: dim, Unit: unit, Start: m[0], End: m[1]}
		if q.Cmp == CmpEQ {
			rest := text[m[1]:]
			if strings.HasPrefix(rest, "以内") || strings.HasPrefix(rest, "内") {
				q.Cmp = CmpLE
			} else if strings.HasPrefix(rest, "以上") {
				q.Cmp = CmpGE
			}
		}
		out = append(out, q)
	}
	return out
}

func lastRune(s string) rune {
	s = strings.TrimRight(s, " ")
	if s == "" {
		return 0
	}
	r := []rune(s)
	return r[len(r)-1]
}

func firstRune(s string) rune {
	s = strings.TrimLeft(s, " ")
	for _, r := range s {
		return r
	}
	return 0
}

// Satisfies 判断承诺值 got 是否满足要求约束 want；better 表示严格优于门槛（正偏离）。
// 对时长 / 时限类「≤」门槛，越小越好；对数量 / 百分比 / 质保等「≥」门槛，越大越好。
func Satisfies(want, got Quantity) (ok, better bool) {
	const eps = 1e-9
	switch want.Cmp {
	case CmpGE:
		return got.Value+eps >= want.Value, got.Value > want.Value+eps
	case CmpGT:
		return got.Value > want.Value+eps, got.Value > want.Value+eps
	case CmpLE:
		return got.Value <= want.Value+eps, got.Value+eps < want.Value
	case CmpLT:
		return got.Value+eps < want.Value, got.Value+eps < want.Value
	}
	return Close(want.Value, got.Value), false
}

// Close 判断两个归一值是否视为相同（3% 相对容差，覆盖「1 年 = 12 个月 = 365 天」「6 个月 = 180 天」）。
func Close(a, b float64) bool {
	if a == b {
		return true
	}
	m := math.Max(math.Abs(a), math.Abs(b))
	return m > 0 && math.Abs(a-b)/m <= 0.03
}

// FormatValue 以人类可读方式输出归一值。
func FormatValue(dim string, v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	switch dim {
	case DimDuration:
		return s + " 天"
	case DimTime:
		return s + " 分钟"
	case DimPercent:
		return s + "%"
	case DimCount:
		return s + " 人"
	}
	return s
}
