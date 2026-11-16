package compliance

// D4：以承诺台账为基准的一致性比对（纯规则，不访问数据库）。
//
// Compare 只能发现「章节之间互相矛盾」：两处都写错成同一个值时它看不出来，也说不清哪一处是对的。承诺台账是先定下来、
// 经交付确认的基线，所以正文里每一处关键承诺都应与台账一致。CompareWithLedger 把 Extract 抽出的每处承诺与同指标的
// 台账条目比对：与任意一条一致即通过（同一指标可以有多条，如核心设备与其他设备的质保期不同）；全部不一致则输出一条
// 结论，并指明最接近的那条台账。台账中没有该指标、或台账值无法归一到该指标的量纲时不比对，由调用方统计后如实报告。

import (
	"fmt"
	"math"
	"strings"
)

// LedgerCommitment 是一条已确认 / 已会签的承诺台账条目。
type LedgerCommitment struct {
	ID      int64
	Code    string
	Version int
	Metric  string // commitment_ledger.metric：duration / warranty / sla_response / sla_onsite / sla_repair / availability / team_size
	Label   string
	Raw     string   // 承诺原文值，如「5 年」「99.9%」
	Unit    string   // 承诺值单位
	Value   *float64 // 承诺数值（与 Unit 同单位）
}

// ledgerMetricKeys 把台账指标对应到 Extract 的指标键。价格与 other 没有对应的正文指标，不参与比对。
var ledgerMetricKeys = map[string]string{
	"duration": MetricDuration, "warranty": MetricWarranty, "sla_response": MetricSLAResponse, "sla_onsite": MetricSLAOnsite,
	"sla_repair": MetricSLARepair, "availability": MetricAvailability, "team_size": MetricTeamSize,
}

func metricDim(key string) (string, bool) {
	for _, d := range metricDefs {
		if d.key == key {
			return d.dim, true
		}
	}
	return "", false
}

// LedgerValue 把台账承诺值归一到其指标的量纲（天 / 分钟 / 百分比 / 人）；key 是对应的 Extract 指标键。
// 指标不参与比对或值无法归一时 ok 为 false。
func LedgerValue(c LedgerCommitment) (value float64, key string, ok bool) {
	key, known := ledgerMetricKeys[c.Metric]
	if !known {
		return 0, "", false
	}
	want, _ := metricDim(key)
	// 承诺原文是渲染进正文的文字，优先按它解析（「5 年」或「90」+ 单位「天」）。committed_value 按单位计，但 D4 之前由原文
	// 自动解析出来的旧记录是归一到天 / 分钟的，所以只在原文解析不出时才用它乘以单位系数。
	for _, text := range []string{c.Raw, strings.TrimSpace(c.Raw) + strings.TrimSpace(c.Unit)} {
		for _, q := range Quantities(text) {
			if q.Dim == want {
				return q.Value, key, true
			}
		}
	}
	if c.Value != nil && strings.TrimSpace(c.Raw) == "" {
		if dim, factor := unitDim(strings.TrimSpace(c.Unit)); dim == want {
			return *c.Value * factor, key, true
		}
	}
	if c.Value != nil && strings.TrimSpace(c.Unit) == "" && (want == DimPercent || want == DimCount) {
		return *c.Value, key, true
	}
	return 0, key, false
}

// LedgerMismatch 是正文中一处与承诺台账不一致的承诺。
type LedgerMismatch struct {
	Metric     string           `json:"metric"`
	Label      string           `json:"label"`
	Mention    Mention          `json:"mention"`
	Commitment LedgerCommitment `json:"commitment"` // 同指标中数值最接近的一条
	LedgerText string           `json:"ledger_text"`
	Detail     string           `json:"detail"`
}

// CompareWithLedger 比对正文承诺与台账。uncomparable 是指标可比、但承诺值无法归一的台账条目编号。
func CompareWithLedger(mentions []Mention, ledger []LedgerCommitment) (out []LedgerMismatch, uncomparable []string) {
	type entry struct {
		c LedgerCommitment
		v float64
	}
	by := map[string][]entry{}
	for _, c := range ledger {
		v, key, ok := LedgerValue(c)
		if key == "" {
			continue
		}
		if !ok {
			uncomparable = append(uncomparable, c.Code)
			continue
		}
		by[key] = append(by[key], entry{c: c, v: v})
	}
	seen := map[string]bool{}
	for _, m := range mentions {
		entries := by[m.Metric]
		if len(entries) == 0 {
			continue
		}
		nearest, matched := entries[0], false
		for _, e := range entries {
			if Close(m.Value, e.v) {
				matched = true
				break
			}
			if math.Abs(e.v-m.Value) < math.Abs(nearest.v-m.Value) {
				nearest = e
			}
		}
		if matched {
			continue
		}
		k := fmt.Sprintf("%d/%s/%v", m.SectionID, m.Metric, m.Value)
		if seen[k] {
			continue
		}
		seen[k] = true
		raw := strings.TrimSpace(nearest.c.Raw)
		if raw == "" && nearest.c.Value != nil {
			raw = strings.TrimSpace(fmt.Sprintf("%g %s", *nearest.c.Value, nearest.c.Unit))
		}
		text := fmt.Sprintf("%s（≈%s）", raw, FormatValue(m.Dim, nearest.v))
		out = append(out, LedgerMismatch{Metric: m.Metric, Label: m.Label, Mention: m, Commitment: nearest.c, LedgerText: text,
			Detail: fmt.Sprintf("%s与承诺台账不一致：正文 %s，台账 %s 为 %s", m.Label, describe(m), nearest.c.Code, text)})
	}
	return out, uncomparable
}
