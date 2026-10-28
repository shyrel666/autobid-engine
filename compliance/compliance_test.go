package compliance

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseNumberAndQuantities(t *testing.T) {
	for s, want := range map[string]float64{"12": 12, "三": 3, "十二": 12, "二十五": 25, "一百零五": 105, "两千": 2000, "3.5": 3.5} {
		if v, ok := ParseNumber(s); !ok || v != want {
			t.Fatalf("ParseNumber(%q) = %v %v, want %v", s, v, ok, want)
		}
	}
	qs := Quantities("工期不超过90日历天，质保期不少于三年，2小时内响应，提供7×24小时服务，可用性≥99.9%，团队共十二人")
	got := map[string]Quantity{}
	for _, q := range qs {
		got[q.Dim+q.Cmp] = q
	}
	if q := got[DimDuration+CmpLE]; q.Value != 90 {
		t.Fatalf("duration: %+v", qs)
	}
	if q := got[DimDuration+CmpGE]; q.Value != 3*365 {
		t.Fatalf("warranty: %+v", qs)
	}
	if q := got[DimTime+CmpLE]; q.Value != 120 {
		t.Fatalf("2 小时内: %+v", qs)
	}
	for _, q := range qs {
		if q.Dim == DimTime && q.Value == 24*60 {
			t.Fatalf("7×24 小时 must not be a time limit: %+v", qs)
		}
	}
	if q := got[DimPercent+CmpGE]; q.Value != 99.9 {
		t.Fatalf("availability: %+v", qs)
	}
	if q := got[DimCount+CmpEQ]; q.Value != 12 {
		t.Fatalf("team: %+v", qs)
	}
	if !Close(6*30, 180) || !Close(12*30, 365) || Close(90, 180) {
		t.Fatal("tolerance")
	}
}

func TestExtractAndCompare(t *testing.T) {
	sections := []SectionText{
		{SectionID: 1, Seq: 0, RevNo: 1, ContentMD: "本项目总工期 90 天，质保期 3 年。项目团队共 12 人。核心交换机采用 S12700E，共 2 台。"},
		{SectionID: 2, Seq: 1, RevNo: 2, ContentMD: "我方承诺 2 小时内响应，4 小时内到场，24 小时内修复，提供 7×24 小时服务。系统可用性不低于 99.9%。"},
		{SectionID: 5, Seq: 4, RevNo: 1, ContentMD: "实施计划：工期 6 个月，分三个阶段。\n\n全部工作 180 日内完成交付周期。质保期三十六个月。招标文件要求工期不超过 120 天。\n\n核心交换机选用 S12700E ×4 台。投入实施人员 12 名。"},
	}
	var ms []Mention
	for _, s := range sections {
		ms = append(ms, Extract(s)...)
	}
	byMetric := map[string][]Mention{}
	for _, m := range ms {
		byMetric[m.Metric] = append(byMetric[m.Metric], m)
	}
	if len(byMetric[MetricDuration]) < 2 || len(byMetric[MetricWarranty]) != 2 || len(byMetric[MetricSLAResponse]) != 1 ||
		len(byMetric[MetricSLAOnsite]) != 1 || len(byMetric[MetricSLARepair]) != 1 || len(byMetric[MetricAvailability]) != 1 || len(byMetric[MetricTeamSize]) != 2 {
		t.Fatalf("mentions = %+v", byMetric)
	}
	if v := byMetric[MetricSLAOnsite][0].Value; v != 240 {
		t.Fatalf("onsite = %v", v)
	}
	for _, m := range byMetric[MetricDuration] {
		if m.Value == 120 {
			t.Fatalf("quoted tender requirement treated as commitment: %+v", m)
		}
	}
	cs := Compare(ms)
	var dur, qty int
	for _, c := range cs {
		switch {
		case c.Metric == MetricDuration:
			dur++
			if c.A.SectionID != 1 || c.B.SectionID != 5 || c.A.Value != 90 || c.B.Value != 180 {
				t.Fatalf("duration contradiction: %+v", c)
			}
		case strings.HasPrefix(c.Metric, MetricQuantity):
			qty++
		case c.Metric == MetricWarranty || c.Metric == MetricTeamSize:
			t.Fatalf("3 年 = 36 个月 / 12 人 = 12 名 must be consistent: %+v", c)
		}
	}
	if dur != 1 || qty != 1 {
		t.Fatalf("contradictions = %+v", cs)
	}
}

func TestRuleVerifier(t *testing.T) {
	v := RuleVerifier{}
	ctx := context.Background()
	req := Requirement{ID: 1, Code: "★2.3", Category: "mandatory_clause", Title: "★2.3 售后服务响应", IsMandatory: true,
		Description: "★2.3 投标人须提供7×24小时运维服务，故障响应时间不超过2小时，否则投标无效。"}
	sec := Section{ID: 9, Code: "4", Title: "★ 实质性条款逐条响应"}
	run := func(body string, ev ...Evidence) Verdict {
		t.Helper()
		out, err := v.Verify(ctx, Input{Requirement: req, Section: sec, Revision: Revision{ID: 3, RevNo: 1, ContentMD: body}, Evidence: ev})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	ok := run("## ★2.3 售后服务响应\n\n针对条款 2.3，我方承诺提供 7×24 小时运维服务，故障响应时间不超过 1 小时，优于招标要求。")
	if ok.Status != StatusVerified || ok.Deviation != DeviationPositive || ok.Anchor == "" || !strings.HasPrefix(ok.Anchor, "p2-") {
		t.Fatalf("substantive response: %+v", ok)
	}
	for name, body := range map[string]string{
		"title only":   "## ★2.3 售后服务响应\n\n★2.3 售后服务响应",
		"see appendix": "## ★2.3 售后服务响应\n\n详见附件。",
		"restatement":  "★2.3 投标人须提供7×24小时运维服务，故障响应时间不超过2小时，否则投标无效。",
		"off topic":    "我方将组建专业团队负责系统集成与培训工作，确保项目顺利交付。",
		"slower":       "我方承诺提供 7×24 小时运维服务，故障响应时间 4 小时。",
		"no number":    "我方承诺提供 7×24 小时运维服务，故障发生后尽快响应。",
		"negation":     "我方暂不提供 7×24 小时运维服务，故障响应时间 2 小时。",
		"empty":        "",
	} {
		if out := run(body); out.Status == StatusVerified {
			t.Fatalf("%s must not be verified: %+v", name, out)
		}
	}
	if out := run("我方承诺提供 7×24 小时运维服务，故障响应时间 4 小时。"); out.Deviation != DeviationNegative {
		t.Fatalf("slower response must be a negative deviation: %+v", out)
	}
	// 只有 citation / 已核验证据但正文无实质应答：仍非 verified。
	if out := run("★2.3 售后服务响应（证据见知识库 #12）", Evidence{ID: 12, Status: "verified", Relation: "supports"}); out.Status == StatusVerified {
		t.Fatalf("citation alone verified: %+v", out)
	}
	good := "我方承诺提供 7×24 小时运维服务，故障响应时间不超过 2 小时。"
	if out := run(good, Evidence{ID: 12, Status: "stale", Reason: "source deleted"}); out.Status != StatusFailed || !strings.Contains(out.Reason, "证据") {
		t.Fatalf("stale evidence: %+v", out)
	}
	if out := run(good, Evidence{ID: 12, Status: "unverified", Relation: "insufficient"}); out.Status != StatusVerified || out.Deviation != DeviationNone {
		t.Fatalf("unverified evidence is a note, not a blocker: %+v", out)
	}
}

type stubJudge struct {
	j   Judgement
	err error
	n   int
}

func (s *stubJudge) Judge(context.Context, Input, Verdict) (Judgement, error) {
	s.n++
	return s.j, s.err
}

func TestLLMVerifierOnlyDowngrades(t *testing.T) {
	ctx := context.Background()
	in := Input{Requirement: Requirement{Title: "投标文件签章", Description: "投标文件须逐页签字盖章"},
		Revision: Revision{RevNo: 1, ContentMD: "我方投标文件已逐页签字并加盖公章。"}}
	j := &stubJudge{j: Judgement{Substantive: false, Reason: "套话"}}
	out, err := LLMVerifier{Judge: j}.Verify(ctx, in)
	if err != nil || out.Status != StatusFailed || out.Verifier != VerifierLLM || j.n != 1 {
		t.Fatalf("judge downgrade: %+v %v", out, err)
	}
	in.Revision.ContentMD = "详见附件"
	out, _ = LLMVerifier{Judge: &stubJudge{j: Judgement{Substantive: true}}}.Verify(ctx, in)
	if out.Status == StatusVerified {
		t.Fatalf("LLM must not upgrade a rule failure: %+v", out)
	}
	in.Revision.ContentMD = "我方投标文件已逐页签字并加盖公章。"
	if _, err := (LLMVerifier{Judge: &stubJudge{err: errors.New("timeout")}}).Verify(ctx, in); err == nil {
		t.Fatal("judge error must surface")
	}
}

func TestRequirementKind(t *testing.T) {
	cases := [][2]string{
		{RequirementKind("mandatory_clause", "★1.1 资质", "", true), KindStar},
		{RequirementKind("non_scoring_required", "▲ 参数", "", false), KindTriangle},
		{RequirementKind("mandatory_clause", "废标条款", "", true), KindStar},
		{RequirementKind("scoring_item", "技术方案", "", false), KindScoring},
		{RequirementKind("format_attachment", "签章", "", false), KindFormat},
		{RequirementKind("non_scoring_required", "售后承诺", "", false), KindRequired},
	}
	for i, c := range cases {
		if c[0] != c[1] {
			t.Fatalf("case %d: kind %s != %s", i, c[0], c[1])
		}
	}
}
