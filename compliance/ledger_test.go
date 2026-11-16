package compliance

import (
	"strings"
	"testing"
)

func fp(v float64) *float64 { return &v }

func ledgerMentions(texts ...string) []Mention {
	var out []Mention
	for i, t := range texts {
		out = append(out, Extract(SectionText{SectionID: int64(i + 1), Code: "s", Seq: i, RevNo: 1, ContentMD: t})...)
	}
	return out
}

func TestLedgerValueNormalises(t *testing.T) {
	cases := []struct {
		c    LedgerCommitment
		want float64
		key  string
		ok   bool
	}{
		{LedgerCommitment{Metric: "warranty", Value: fp(5), Unit: "年"}, 5 * 365, MetricWarranty, true},
		{LedgerCommitment{Metric: "duration", Raw: "90 个日历天"}, 90, MetricDuration, true},
		{LedgerCommitment{Metric: "sla_response", Value: fp(2), Unit: "小时"}, 120, MetricSLAResponse, true},
		{LedgerCommitment{Metric: "sla_response", Raw: "30", Unit: "分钟"}, 30, MetricSLAResponse, true},
		{LedgerCommitment{Metric: "availability", Value: fp(99.9)}, 99.9, MetricAvailability, true},
		{LedgerCommitment{Metric: "team_size", Value: fp(12), Unit: "人"}, 12, MetricTeamSize, true},
		// committed_value 的口径不统一：原文「5 年」自动解析出的值已归一到天（1825），页面录入的是 5。两种都以原文为准。
		{LedgerCommitment{Metric: "warranty", Raw: "5 年", Unit: "年", Value: fp(1825)}, 1825, MetricWarranty, true},
		{LedgerCommitment{Metric: "warranty", Raw: "5", Unit: "年", Value: fp(5)}, 1825, MetricWarranty, true},
		{LedgerCommitment{Metric: "duration", Raw: "90", Unit: "天", Value: fp(90)}, 90, MetricDuration, true},
		{LedgerCommitment{Metric: "availability", Raw: "99.9", Unit: "%", Value: fp(99.9)}, 99.9, MetricAvailability, true},
		// 单位与指标量纲不符、原文也解析不出同量纲的值：不可比，不能硬比。
		{LedgerCommitment{Metric: "warranty", Value: fp(5), Unit: "台"}, 0, MetricWarranty, false},
		{LedgerCommitment{Metric: "price", Value: fp(100)}, 0, "", false},
		{LedgerCommitment{Metric: "other", Raw: "3 年"}, 0, "", false},
	}
	for _, tc := range cases {
		v, key, ok := LedgerValue(tc.c)
		if ok != tc.ok || key != tc.key || (ok && v != tc.want) {
			t.Errorf("LedgerValue(%+v) = %v %q %v, want %v %q %v", tc.c, v, key, ok, tc.want, tc.key, tc.ok)
		}
	}
}

func TestCompareWithLedger(t *testing.T) {
	ledger := []LedgerCommitment{
		{ID: 1, Code: "C-WARRANTY", Version: 2, Metric: "warranty", Label: "质保期", Raw: "5 年", Unit: "年", Value: fp(5)},
		{ID: 2, Code: "C-DURATION", Version: 1, Metric: "duration", Label: "总工期", Raw: "90", Unit: "天", Value: fp(90)},
		{ID: 3, Code: "C-BAD", Version: 1, Metric: "sla_repair", Label: "修复时限", Raw: "尽快", Unit: ""},
	}
	// 两个章节都把质保写成 3 年：章节之间互相一致，Compare 看不出来；与台账比对才能发现。
	ms := ledgerMentions("我方承诺免费质保期为 3 年。总工期为 90 天。", "设备质保期 3 年，到期后提供有偿维保。")
	if cs := Compare(append([]Mention(nil), ms...)); len(cs) != 0 {
		t.Fatalf("sections agree with each other, Compare found %+v", cs)
	}
	out, uncomparable := CompareWithLedger(ms, ledger)
	if len(out) != 2 || len(uncomparable) != 1 || uncomparable[0] != "C-BAD" {
		t.Fatalf("mismatches = %+v uncomparable = %v", out, uncomparable)
	}
	for i, m := range out {
		if m.Metric != MetricWarranty || m.Commitment.Code != "C-WARRANTY" || m.Mention.SectionID != int64(i+1) ||
			!strings.Contains(m.Detail, "C-WARRANTY") || !strings.Contains(m.LedgerText, "5 年") {
			t.Fatalf("mismatch %d = %+v", i, m)
		}
	}
	// 与台账一致（换算单位后相等）不报；同一章节同一取值只报一次。
	ok := ledgerMentions("质保期 60 个月。工期 3 个月。")
	if out, _ := CompareWithLedger(ok, ledger); len(out) != 0 {
		t.Fatalf("consistent text flagged: %+v", out)
	}
	dup := ledgerMentions("质保期 3 年。\n保修期 3 年。")
	if out, _ := CompareWithLedger(dup, ledger); len(out) != 1 {
		t.Fatalf("duplicate mention reported %d times", len(out))
	}
	// 同一指标有多条台账（核心设备 5 年、其余 3 年）：与任意一条一致即通过，都不一致时指向最接近的一条。
	multi := append(ledger, LedgerCommitment{ID: 4, Code: "C-WARRANTY-OTHER", Version: 1, Metric: "warranty", Label: "其余设备质保期", Raw: "3 年", Unit: "年", Value: fp(3)})
	if out, _ := CompareWithLedger(ms, multi); len(out) != 0 {
		t.Fatalf("value matching a second ledger entry flagged: %+v", out)
	}
	two := ledgerMentions("质保期 2 年。")
	if out, _ := CompareWithLedger(two, multi); len(out) != 1 || out[0].Commitment.Code != "C-WARRANTY-OTHER" {
		t.Fatalf("nearest ledger entry = %+v", out)
	}
	// 台账里没有的指标不比对；引述招标要求的句子不是我方承诺。
	none := ledgerMentions("故障响应时间不超过 30 分钟。招标文件要求质保期不少于 1 年。")
	if out, _ := CompareWithLedger(none, ledger); len(out) != 0 {
		t.Fatalf("metric absent from the ledger or a quoted requirement flagged: %+v", out)
	}
}

// consistency-v2：引述只作用于引述词之后；承诺在前、引述在后的句子照常抽取（v1 整句跳过，承诺从不参与比对）。
func TestExtractVoiceAroundQuotedRequirements(t *testing.T) {
	cases := []struct {
		name, text string
		want       map[string]float64 // 指标 -> 归一值；空表示不应抽出任何承诺
	}{
		{"commitment before the quote", "我方承诺免费质保期 5 年，优于招标要求。", map[string]float64{MetricWarranty: 5 * 365}},
		{"trailing compliance phrase", "总工期 90 天，质保期 3 年，均满足招标文件要求。", map[string]float64{MetricDuration: 90, MetricWarranty: 3 * 365}},
		{"quote then first person", "招标文件要求工期不超过 120 天，我方承诺工期 90 天。", map[string]float64{MetricDuration: 90}},
		{"quote then first person after a bracket", "响应要求 S1「质保」（招标文件要求：质保期不少于 3 年），我方承诺质保期 5 年。", map[string]float64{MetricWarranty: 5 * 365}},
		{"pure restatement", "招标文件要求工期不超过 120 天，质保期不少于 3 年。", nil},
		{"restatement after a lead-in", "根据招标文件要求，本项目工期为 120 天。", nil},
		{"tender threshold after our keyword", "质保期优于招标要求的 3 年。", nil},
		{"tender asks the bidder to commit", "招标文件要求投标人承诺质保期不少于 3 年。", nil},
	}
	for _, c := range cases {
		got := map[string]float64{}
		for _, m := range Extract(SectionText{SectionID: 1, RevNo: 1, ContentMD: c.text}) {
			got[m.Metric] = m.Value
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: mentions = %v, want %v", c.name, got, c.want)
			continue
		}
		for k, v := range c.want {
			if !Close(got[k], v) {
				t.Errorf("%s: %s = %v, want %v", c.name, k, got[k], v)
			}
		}
	}
	// 型号同样只在我方表述内抽取。
	if ms := Extract(SectionText{SectionID: 1, RevNo: 1, ContentMD: "招标文件要求核心交换机采用 S12700E。"}); len(ms) != 0 {
		t.Errorf("quoted model treated as ours: %+v", ms)
	}
	if ms := Extract(SectionText{SectionID: 1, RevNo: 1, ContentMD: "核心交换机采用 S12700E，满足招标文件要求。"}); len(ms) != 1 {
		t.Errorf("our model before a trailing quote = %+v", ms)
	}
}
