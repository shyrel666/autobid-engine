package compliance

import (
	"testing"
	"time"
)

func d2Date(y int, m time.Month, d int) *time.Time {
	t := time.Date(y, m, d, 0, 0, 0, 0, cstZone)
	return &t
}

func d2Original(kind, label string) AdmissibilityOriginal {
	return AdmissibilityOriginal{DocumentKind: kind, PageLabel: label, PageStart: 1, PageEnd: 1,
		SHA256: "abc", Present: true, Exists: true, Readable: true, HashMatch: true}
}

func TestEvaluateQualificationRules(t *testing.T) {
	deadline := time.Date(2026, 12, 5, 9, 0, 0, 0, cstZone)
	req := AdmissibilityRequirement{Code: "REQ", Category: "mandatory_clause",
		ProofRules: AdmissibilityProofRules{BidderOnly: true, ValidOnBidDeadline: true, RequiredDocs: []string{"certificate"},
			ScopeKeywords: []string{"安防"}, MinCount: 1}}
	base := AdmissibilityEvidence{Domain: "credential", ID: 1, Category: "qualification", Title: "资质",
		SubjectName: "演示企业", SubjectType: "bidder", Scope: "安防工程", ValidUntil: d2Date(2027, 6, 30),
		Originals: []AdmissibilityOriginal{d2Original("certificate", "证书首页")}}
	res := Evaluate(AdmissibilityInput{Requirement: req, Evidence: []AdmissibilityEvidence{base}, BidDeadline: deadline, BidderName: "演示企业"})
	if res.Verdict != Admissible || len(res.Items) != 1 || res.Items[0].Verdict != Admissible {
		t.Fatalf("valid qualification: %+v", res)
	}
	expired := base
	expired.ValidUntil = d2Date(2026, 1, 1)
	res = Evaluate(AdmissibilityInput{Requirement: req, Evidence: []AdmissibilityEvidence{expired}, BidDeadline: deadline, BidderName: "演示企业"})
	if res.Verdict != Inadmissible || res.Items[0].Verdict != Inadmissible {
		t.Fatalf("expired: %+v", res)
	}
	subsidiary := base
	subsidiary.SubjectType, subsidiary.SubjectName = "subsidiary", "子公司"
	res = Evaluate(AdmissibilityInput{Requirement: req, Evidence: []AdmissibilityEvidence{subsidiary}, BidDeadline: deadline, BidderName: "演示企业"})
	if res.Verdict != Inadmissible || !hasReason(res.Items[0], "bidder_only") {
		t.Fatalf("subsidiary: %+v", res)
	}
	manualScope := base
	manualScope.Scope = ""
	res = Evaluate(AdmissibilityInput{Requirement: req, Evidence: []AdmissibilityEvidence{manualScope}, BidDeadline: deadline, BidderName: "演示企业"})
	if res.Verdict != Manual || !hasReason(res.Items[0], "scope_match") {
		t.Fatalf("empty scope must be manual: %+v", res)
	}
}

func TestEvaluateMinCountAndDateWindow(t *testing.T) {
	deadline := time.Date(2026, 12, 5, 9, 0, 0, 0, cstZone)
	req := AdmissibilityRequirement{Code: "PERF", Category: "scoring_item", ProofRules: AdmissibilityProofRules{
		BidderOnly: true, NeedAcceptance: true, DateAnchor: "acceptance_date", YearsWindow: 3, MinCount: 2,
		RequiredDocs: []string{"contract", "acceptance_report"}}}
	ev := func(id int64, accepted time.Time) AdmissibilityEvidence {
		return AdmissibilityEvidence{Domain: "performance", ID: id, Category: "performance", Title: "业绩",
			SubjectName: "演示企业", SubjectType: "bidder", AcceptanceDate: &accepted,
			Originals: []AdmissibilityOriginal{d2Original("contract", "标的页"), d2Original("acceptance_report", "验收结论")}}
	}
	one := ev(1, *d2Date(2025, 1, 1))
	res := Evaluate(AdmissibilityInput{Requirement: req, Evidence: []AdmissibilityEvidence{one}, BidDeadline: deadline, BidderName: "演示企业"})
	if res.Verdict != Inadmissible || !runHasReason(res, "min_count") {
		t.Fatalf("min_count: %+v", res)
	}
	two := ev(2, *d2Date(2025, 2, 1))
	res = Evaluate(AdmissibilityInput{Requirement: req, Evidence: []AdmissibilityEvidence{one, two}, BidDeadline: deadline, BidderName: "演示企业"})
	if res.Verdict != Admissible {
		t.Fatalf("two valid: %+v", res)
	}
	old := ev(3, *d2Date(2020, 1, 1))
	res = Evaluate(AdmissibilityInput{Requirement: req, Evidence: []AdmissibilityEvidence{one, two, old}, BidDeadline: deadline, BidderName: "演示企业"})
	if res.Verdict != Admissible {
		t.Fatalf("extra invalid evidence must not drag satisfied min_count: %+v", res)
	}
	if res.Items[2].Verdict != Inadmissible || !hasReason(res.Items[2], "date_window") {
		t.Fatalf("old item: %+v", res.Items[2])
	}
}

func TestComputeObjective(t *testing.T) {
	res := ComputeObjective("每提供一个类似安防项目业绩得10分，最多得30分，须提供合同关键页及验收报告。", 30, 2, 2)
	if res.Status != "computed" || res.Score != 20 || res.Max != 30 {
		t.Fatalf("objective: %+v", res)
	}
	res = ComputeObjective("每提供一个类似项目业绩得10分，最多得30分", 30, 2, 1)
	if res.Status != "computed" || res.Score != 10 || len(res.Gaps) == 0 || res.Gaps[0] != "evidence_missing" {
		t.Fatalf("missing evidence: %+v", res)
	}
	res = ComputeObjective("由评标委员会酌情打分", 30, 0, 3)
	if res.Status != "manual" || len(res.Gaps) == 0 || res.Gaps[0] != "score_rule_unparsed" {
		t.Fatalf("unparsed rule must be manual: %+v", res)
	}
}

func hasReason(it AdmissibilityItemResult, code string) bool {
	for _, r := range it.Reasons {
		if r.Code == code {
			return true
		}
	}
	return false
}

func runHasReason(res AdmissibilityResult, code string) bool {
	for _, r := range res.Reasons {
		if r.Code == code || r.Kind == code {
			return true
		}
	}
	return false
}
