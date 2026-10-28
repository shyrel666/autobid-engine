package compliance

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// 一致性核验的关键承诺指标。
const (
	MetricDuration     = "total_duration"
	MetricWarranty     = "warranty"
	MetricSLAResponse  = "sla_response"
	MetricSLAOnsite    = "sla_onsite"
	MetricSLARepair    = "sla_repair"
	MetricAvailability = "sla_availability"
	MetricTeamSize     = "team_size"
	MetricModel        = "model"    // model:<产品>
	MetricQuantity     = "quantity" // quantity:<型号>
)

type metricDef struct {
	key, label string
	dim        string
	kw         *regexp.Regexp
}

var metricDefs = []metricDef{
	{MetricDuration, "总工期", DimDuration, regexp.MustCompile(`总工期|建设工期|施工工期|工期|建设周期|实施周期|交付周期|项目周期`)},
	{MetricWarranty, "质保期", DimDuration, regexp.MustCompile(`免费质保期|质保期|质量保证期|保修期|免费保修|质保`)},
	{MetricSLAResponse, "故障响应时限", DimTime, regexp.MustCompile(`响应时间|响应时限|响应`)},
	{MetricSLAOnsite, "到场时限", DimTime, regexp.MustCompile(`到达现场|抵达现场|赶到现场|到场`)},
	{MetricSLARepair, "修复时限", DimTime, regexp.MustCompile(`故障修复|修复时间|排除故障|故障恢复|恢复时间|修复`)},
	{MetricAvailability, "系统可用性", DimPercent, regexp.MustCompile(`可用性|可用率|在线率`)},
	{MetricTeamSize, "项目团队人数", DimCount, regexp.MustCompile(`项目团队|团队成员|实施团队|服务团队|项目组|投入人员|实施人员|驻场人员|团队`)},
}

// Mention 是正文中的一处关键承诺。
type Mention struct {
	Metric    string   `json:"metric"`
	Label     string   `json:"label"`
	SectionID int64    `json:"section_id"`
	RevNo     int      `json:"rev_no"`
	Seq       int      `json:"-"`
	Value     float64  `json:"value"`
	Dim       string   `json:"dim"`
	Text      string   `json:"text"` // 值的展示（原文数值或型号）
	Excerpt   string   `json:"excerpt"`
	Quantity  *float64 `json:"quantity,omitempty"`
}

// SectionText 是一致性核验的输入：一个章节的当前有效修订正文。
type SectionText struct {
	SectionID int64
	Code      string
	Title     string
	Seq       int
	RevNo     int
	ContentMD string
}

var sentenceRe = regexp.MustCompile(`[^。；;！!？?\n]+`)

// 引述招标方要求的句子不是我方承诺，跳过（如「招标文件要求工期不超过 120 天」）。
var quotedReqRe = regexp.MustCompile(`招标文件|招标要求|采购需求|采购文件|招标人要求|采购人要求|根据要求|要求为|要求：|要求:`)

// 型号：<产品名><采用 / 选用 / 型号为 / （型号：>MODEL，后随可选数量。
var modelRe = regexp.MustCompile(`([\p{Han}A-Za-z]{2,12}?)\s*(?:采用|选用|选型为|型号为|型号[:：]|[（(]\s*型号[:：]?)\s*([A-Za-z][A-Za-z0-9][A-Za-z0-9\-_/.]{1,30})`)
var modelQtyRe = regexp.MustCompile(`^[^。；;]{0,8}?(?:[×xX*]\s*)?(\d+|[一二两三四五六七八九十百]+)\s*(台|套|块|个|路|节点|组|部)`)
var labelTrimRe = regexp.MustCompile(`^(本项目|项目|我方|我公司|本公司|拟|均|配置|部署|核心层|其中|系统|并|全部|统一)+`)

// Extract 抽取一个章节正文中的全部关键承诺。
func Extract(s SectionText) []Mention {
	var out []Mention
	text := strings.ReplaceAll(s.ContentMD, "\r\n", "\n")
	for _, loc := range sentenceRe.FindAllStringIndex(text, -1) {
		sent := strings.TrimSpace(text[loc[0]:loc[1]])
		if sent == "" || quotedReqRe.MatchString(sent) {
			continue
		}
		excerpt := truncateRunes(sent, 160)
		qs := Quantities(sent)
		for _, d := range metricDefs {
			for _, kw := range d.kw.FindAllStringIndex(sent, -1) {
				if q, ok := nearestValue(sent, kw, qs, d.dim); ok {
					out = append(out, Mention{Metric: d.key, Label: d.label, SectionID: s.SectionID, RevNo: s.RevNo, Seq: s.Seq,
						Value: q.Value, Dim: d.dim, Text: q.Raw, Excerpt: excerpt})
					break // 每句每指标只取一个值
				}
			}
		}
		for _, m := range modelRe.FindAllStringSubmatchIndex(sent, -1) {
			label := labelTrimRe.ReplaceAllString(sent[m[2]:m[3]], "")
			model := strings.TrimRight(sent[m[4]:m[5]], ".-_/")
			if utf8.RuneCountInString(label) < 2 {
				continue
			}
			mm := Mention{Metric: MetricModel + ":" + label, Label: label + " 型号", SectionID: s.SectionID, RevNo: s.RevNo, Seq: s.Seq,
				Dim: "model", Text: model, Excerpt: excerpt}
			if qm := modelQtyRe.FindStringSubmatch(sent[m[5]:]); qm != nil {
				if v, ok := ParseNumber(qm[1]); ok {
					mm.Quantity = &v
				}
			}
			out = append(out, mm)
		}
	}
	return out
}

// nearestValue 优先取紧贴关键词之前（中间只有「内 / 以内」，如「4 小时内到场」）的值，否则取关键词之后 16 个字符内、
// 中间不夹其他指标关键词的第一个值（如「工期为 6 个月」）。
func nearestValue(sent string, kw []int, qs []Quantity, dim string) (Quantity, bool) {
	for i := len(qs) - 1; i >= 0; i-- {
		q := qs[i]
		if q.Dim != dim || q.End > kw[0] {
			continue
		}
		switch strings.TrimSpace(sent[q.End:kw[0]]) {
		case "", "内", "以内", "之内":
			return q, true
		}
		break
	}
	for _, q := range qs {
		if q.Dim != dim || q.Start < kw[1] {
			continue
		}
		gap := sent[kw[1]:q.Start]
		if utf8.RuneCountInString(gap) > 16 || otherMetricIn(gap, dim) {
			return Quantity{}, false
		}
		return q, true
	}
	return Quantity{}, false
}

func otherMetricIn(gap, dim string) bool {
	for _, d := range metricDefs {
		if d.dim == dim && d.kw.MatchString(gap) {
			return true
		}
	}
	return false
}

// Contradiction 是同一指标前后不一致的一对承诺。
type Contradiction struct {
	Metric string  `json:"metric"`
	Label  string  `json:"label"`
	A      Mention `json:"a"`
	B      Mention `json:"b"`
	Detail string  `json:"detail"`
}

// Compare 比对全部章节的关键承诺：同一指标的值（按量纲归一、3% 容差）分成多个取值组即为矛盾；
// 以全文最早出现的取值组为基准，与其余每个取值组各输出一条矛盾（双方各取该组最早的一处）。
// 型号额外比对数量：同一型号在不同位置的数量不一致也记为矛盾。
func Compare(mentions []Mention) []Contradiction {
	sort.SliceStable(mentions, func(i, j int) bool { return mentions[i].Seq < mentions[j].Seq })
	by := map[string][]Mention{}
	var order []string
	for _, m := range mentions {
		if _, ok := by[m.Metric]; !ok {
			order = append(order, m.Metric)
		}
		by[m.Metric] = append(by[m.Metric], m)
	}
	var out []Contradiction
	for _, key := range order {
		ms := by[key]
		groups := groupMentions(ms, func(a, b Mention) bool {
			if a.Dim == "model" {
				return strings.EqualFold(a.Text, b.Text)
			}
			return Close(a.Value, b.Value)
		})
		for _, g := range groups[1:] {
			a, b := groups[0][0], g[0]
			out = append(out, Contradiction{Metric: key, Label: a.Label, A: a, B: b,
				Detail: fmt.Sprintf("%s前后不一致：%s vs %s", a.Label, describe(a), describe(b))})
		}
	}
	// 同一型号的数量。
	qty := map[string][]Mention{}
	var qorder []string
	for _, m := range mentions {
		if m.Dim != "model" || m.Quantity == nil {
			continue
		}
		k := strings.ToUpper(m.Text)
		if _, ok := qty[k]; !ok {
			qorder = append(qorder, k)
		}
		qty[k] = append(qty[k], m)
	}
	for _, k := range qorder {
		groups := groupMentions(qty[k], func(a, b Mention) bool { return *a.Quantity == *b.Quantity })
		for _, g := range groups[1:] {
			a, b := groups[0][0], g[0]
			out = append(out, Contradiction{Metric: MetricQuantity + ":" + a.Text, Label: a.Text + " 数量", A: a, B: b,
				Detail: fmt.Sprintf("%s 数量前后不一致：%v vs %v", a.Text, *a.Quantity, *b.Quantity)})
		}
	}
	return out
}

func groupMentions(ms []Mention, same func(a, b Mention) bool) [][]Mention {
	var groups [][]Mention
	for _, m := range ms {
		placed := false
		for i := range groups {
			if same(groups[i][0], m) {
				groups[i] = append(groups[i], m)
				placed = true
				break
			}
		}
		if !placed {
			groups = append(groups, []Mention{m})
		}
	}
	return groups
}

func describe(m Mention) string {
	if m.Dim == "model" {
		if m.Quantity != nil {
			return fmt.Sprintf("%s ×%v", m.Text, *m.Quantity)
		}
		return m.Text
	}
	return fmt.Sprintf("%s（≈%s）", m.Text, FormatValue(m.Dim, m.Value))
}

// Describe 返回承诺值的展示文本（原文值 + 归一值）。
func Describe(m Mention) string { return describe(m) }
