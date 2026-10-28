package bidtools

import (
	"context"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/tool"
)

func ptr[T any](v T) *T { return &v }

func TestMatchChunksKeywordsRegexAndMandatory(t *testing.T) {
	chunks := []*db.DocumentChunk{
		{ID: 1, DocID: 9, SectionCode: "2.1.1", SectionTitle: "独立法人", Content: "2.1.1 投标人须具有独立法人资格。"},
		{ID: 2, DocID: 9, SectionCode: "2.1.2", SectionTitle: "集成资质", Content: "★2.1.2 投标人须具备信息系统集成一级资质，否则投标无效。", IsMandatory: true},
		{ID: 3, DocID: 9, SectionCode: "3.1", SectionTitle: "报价", Content: "3.1 报价超过最高限价的作废标处理。"},
		{ID: 4, DocID: 9, SectionCode: "3.2", SectionTitle: "付款", Content: "3.2 按月支付服务费。"},
	}
	res, _ := compileQuery("投标人 资质", false)
	got, total := matchChunks(chunks, res, false, 10)
	if total != 1 || got[0].ChunkID != 2 || !got[0].Mandatory {
		t.Fatalf("keywords: total=%d %+v", total, got)
	}
	res, _ = compileQuery(`最高限价|一级资质`, true)
	if _, total = matchChunks(chunks, res, false, 10); total != 2 {
		t.Fatalf("regex total = %d", total)
	}
	got, total = matchChunks(chunks, nil, true, 10)
	if total != 2 || got[0].ChunkID != 2 || got[1].ChunkID != 3 || !strings.Contains(got[1].Snippet, "废标") {
		t.Fatalf("mandatory only: total=%d %+v", total, got)
	}
	got, total = matchChunks(chunks, nil, true, 1)
	if total != 2 || len(got) != 1 {
		t.Fatalf("limit: total=%d len=%d", total, len(got))
	}
	if _, err := compileQuery("(", true); err == nil {
		t.Fatal("invalid regex accepted")
	}
	if res, _ := compileQuery("a.b", false); res[0].MatchString("axb") {
		t.Fatal("keyword mode must quote regex metacharacters")
	}
}

func TestSnippetWindow(t *testing.T) {
	text := strings.Repeat("甲", 100) + "关键字" + strings.Repeat("乙", 100)
	loc := []int{len(strings.Repeat("甲", 100)), len(strings.Repeat("甲", 100)) + len("关键字")}
	s := snippet(text, loc)
	if !strings.HasPrefix(s, "…") || !strings.HasSuffix(s, "…") || !strings.Contains(s, "关键字") {
		t.Fatalf("snippet = %q", s)
	}
	if n := len([]rune(s)); n != snippetRadius*2+3+2 {
		t.Fatalf("snippet runes = %d", n)
	}
}

func TestEvaluateCredentials(t *testing.T) {
	ref, _ := time.Parse(time.DateOnly, "2026-10-28")
	creds := []*db.CompanyCredential{
		{ID: 1, Category: "qualification", Title: "信息系统集成及服务资质", Attributes: json.RawMessage(`{"level":"二级"}`), ValidUntil: ptr("2027-01-01")},
		{ID: 2, Category: "qualification", Title: "信息系统集成及服务资质（一级）", Attributes: json.RawMessage(`{"level":"一级"}`), ValidUntil: ptr("2026-01-01")},
	}
	req := credentialRequirement{Keywords: []string{"信息系统集成"}, Attributes: map[string]any{"level": "一级"}}
	v := evaluateCredentials(creds, req, ref)
	if v.Verdict != VerdictPartial || v.FullyMatched != 0 {
		t.Fatalf("expired top-level credential must not satisfy: %+v", v)
	}
	for _, c := range v.Candidates {
		if c.ID == 2 && (!c.Expired || len(c.Gaps) != 1 || !strings.Contains(c.Gaps[0], "过期")) {
			t.Fatalf("expired candidate gaps: %+v", c)
		}
		if c.ID == 1 && (c.Expired || len(c.Gaps) != 1 || !strings.Contains(c.Gaps[0], "level=二级")) {
			t.Fatalf("level gap: %+v", c)
		}
	}

	creds[1].ValidUntil = nil
	if v = evaluateCredentials(creds, req, ref); v.Verdict != VerdictSatisfied || v.BestScore != 1 {
		t.Fatalf("valid credential should satisfy: %+v", v)
	}
	req.MinCount = 2
	if v = evaluateCredentials(creds, req, ref); v.Verdict != VerdictPartial || len(v.Gaps) == 0 {
		t.Fatalf("min_count 2: %+v", v)
	}
	if v = evaluateCredentials(nil, req, ref); v.Verdict != VerdictMissing {
		t.Fatalf("no candidates: %+v", v)
	}
}

func TestAttributeSatisfies(t *testing.T) {
	cases := []struct {
		have, want any
		ok         bool
	}{
		{float64(6000000), float64(5000000), true},
		{"4000000", float64(5000000), false},
		{float64(5), "3", true},
		{"高级工程师", "高级", true},
		{[]any{"建筑", "市政公用工程"}, "市政", true},
		{true, true, true},
		{nil, "一级", false},
	}
	for _, c := range cases {
		if ok, _ := attributeSatisfies(c.have, c.want); ok != c.ok {
			t.Errorf("attributeSatisfies(%v, %v) = %v", c.have, c.want, ok)
		}
	}
}

func factNode(id int64, payload string) *db.Node {
	return &db.Node{ID: id, Kind: db.KindFact, State: db.StateConfirmed, Payload: json.RawMessage(payload)}
}

func TestSimulateScoring(t *testing.T) {
	items := []*db.Asset{
		{ID: 10, Code: "T-01", Title: "技术方案", Category: "技术", ScoreMax: ptr(30.0)},
		{ID: 11, Code: "T-02", Title: "实施计划", Category: "技术", ScoreMax: ptr(20.0)},
		{ID: 12, Code: "B-01", Title: "类似业绩", Category: "商务", ScoreMax: ptr(10.0)},
		{ID: 13, Code: "B-02", Title: "信誉", Category: "商务", ScoreMax: ptr(5.0)},
		{ID: 14, Code: "B-03", Title: "缺满分", Category: "商务"},
	}
	facts := map[int64][]*db.Node{
		11: {factNode(100, `{"summary":"旧估计","expected_score":12}`), factNode(101, `{"summary":"新估计","expected_score":16}`)},
		12: {factNode(102, `{"summary":"有 3 个业绩"}`)},
	}
	res := simulateScoring(items, facts, []scoreAssumption{
		{AssetID: 10, ExpectedScore: 33, Note: "方案完整"},
		{AssetID: 999, ExpectedScore: 1},
	})
	byID := map[int64]scoredItem{}
	for _, it := range res.Items {
		byID[it.AssetID] = it
	}
	if it := byID[10]; it.Expected != 30 || it.Basis != BasisAssumption {
		t.Fatalf("assumption clamped: %+v", it)
	}
	if it := byID[11]; it.Expected != 16 || it.Basis != BasisFact {
		t.Fatalf("latest fact score: %+v", it)
	}
	if it := byID[12]; it.Expected != 0 || it.Basis != BasisUnscored || len(it.FactIDs) != 1 {
		t.Fatalf("unscored evidence: %+v", it)
	}
	if it := byID[13]; it.Basis != BasisNone {
		t.Fatalf("no evidence: %+v", it)
	}
	if byID[14].Basis != BasisNoMax {
		t.Fatalf("missing score_max: %+v", byID[14])
	}
	if res.MaxTotal != 65 || res.ExpectedTotal != 46 || res.Ratio == nil || *res.Ratio != 0.71 {
		t.Fatalf("totals: max=%v expected=%v ratio=%v", res.MaxTotal, res.ExpectedTotal, res.Ratio)
	}
	if len(res.ByCategory) != 2 || res.ByCategory[0].Category != "技术" || res.ByCategory[0].Expected != 46 || res.ByCategory[1].Max != 15 {
		t.Fatalf("by category: %+v", res.ByCategory)
	}
	if res.TopLosses[0].AssetID != 12 || res.TopLosses[1].AssetID != 13 || res.TopLosses[2].AssetID != 11 {
		t.Fatalf("top losses: %+v", res.TopLosses)
	}
	// 不带 fact_ids 的 assumption 也算缺证据。
	if len(res.Unevidenced) != 3 || res.Unevidenced[0] != 10 || res.Unevidenced[1] != 13 || res.Unevidenced[2] != 14 {
		t.Fatalf("unevidenced: %v", res.Unevidenced)
	}
	if len(res.Warnings) != 3 {
		t.Fatalf("warnings: %v", res.Warnings)
	}
}

func commercialItems() []*db.Asset {
	return []*db.Asset{
		{ID: 1, Code: "C-01", Title: "云主机", Quantity: ptr("12"), Amount: ptr("120000")},
		{ID: 2, Code: "C-02", Title: "对象存储", Quantity: ptr("10")},
		{ID: 3, Code: "C-03", Title: "运维服务"},
	}
}

func rat(s string) *big.Rat { r, _ := new(big.Rat).SetString(s); return r }

func issueKinds(res pricingResult) map[string]string {
	out := map[string]string{}
	for _, is := range res.Issues {
		out[is.Kind] = is.Severity
	}
	return out
}

func TestVerifyCommercialPass(t *testing.T) {
	res, err := verifyCommercial(commercialItems(), rat("200000"), "input", pricingInput{
		Quotes: []quoteInput{
			{AssetID: 1, UnitPrice: "9000.10", LineTotal: "108001.20"},
			{Code: "C-02", UnitPrice: "1500", Quantity: "10"},
			{AssetID: 3, LineTotal: "30000"},
		},
		DeclaredTotal: "153001.20",
		CostEstimate:  "140000",
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := issueKinds(res)
	if res.Verdict != "pass" || res.ComputedTotal != "153001.20" || res.CeilingRatio != "0.7650" || res.ProfitMargin != "0.0850" {
		t.Fatalf("pass case: %+v issues=%v", res, kinds)
	}
	if kinds["incomplete_quote"] != db.SeverityLow || len(kinds) != 1 {
		t.Fatalf("issues: %v", kinds)
	}
}

func TestVerifyCommercialFailures(t *testing.T) {
	res, err := verifyCommercial(commercialItems(), rat("130000"), "lot_asset", pricingInput{
		Quotes: []quoteInput{
			{AssetID: 1, UnitPrice: "11000", LineTotal: "130000"},
			{AssetID: 1, UnitPrice: "1"},
			{Code: "X-99", UnitPrice: "1", Quantity: "1"},
		},
		DeclaredTotal: "100000",
		CostEstimate:  "120000",
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := issueKinds(res)
	want := map[string]string{
		"line_mismatch":     db.SeverityHigh, // 11000×12=132000 ≠ 130000
		"item_over_ceiling": db.SeverityHigh, // 132000 > 120000
		"duplicate_quote":   db.SeverityHigh,
		"unknown_item":      db.SeverityMedium,
		"missing_item":      db.SeverityHigh,
		"total_mismatch":    db.SeverityHigh,     // 132001 ≠ 100000
		"below_cost":        db.SeverityCritical, // 100000 < 120000
	}
	for k, sev := range want {
		if kinds[k] != sev {
			t.Errorf("issue %s = %q, want %q (all: %v)", k, kinds[k], sev, kinds)
		}
	}
	if res.Verdict != "fail" || strings.Join(res.MissingItems, ",") != "C-02,C-03" {
		t.Fatalf("verdict=%s missing=%v", res.Verdict, res.MissingItems)
	}

	res, _ = verifyCommercial(commercialItems()[:1], rat("100000"), "input", pricingInput{
		Quotes: []quoteInput{{AssetID: 1, UnitPrice: "9000"}},
	})
	if issueKinds(res)["over_ceiling"] != db.SeverityCritical {
		t.Fatalf("over ceiling not flagged: %+v", res.Issues)
	}
	res, _ = verifyCommercial(commercialItems()[:1], rat("1000000"), "input", pricingInput{
		Quotes: []quoteInput{{AssetID: 1, UnitPrice: "9000"}},
	})
	if issueKinds(res)["abnormally_low"] != db.SeverityMedium || res.Verdict != "warn" {
		t.Fatalf("abnormally low: %+v", res)
	}
	res, _ = verifyCommercial(commercialItems()[:1], nil, "", pricingInput{Quotes: []quoteInput{{AssetID: 1, UnitPrice: "1"}}})
	if issueKinds(res)["no_ceiling"] != db.SeverityLow {
		t.Fatalf("no ceiling: %+v", res.Issues)
	}
	if _, err := verifyCommercial(commercialItems(), nil, "", pricingInput{Quotes: []quoteInput{{AssetID: 1, UnitPrice: "abc"}}}); err == nil {
		t.Fatal("invalid number accepted")
	}
}

// 渐进式披露：延迟工具在 Skill 解锁前不暴露 Schema，解锁后 SearchExtraTools 返回 Schema。
func TestSkillsUnlockDeferredTools(t *testing.T) {
	kit := New(nil, 1, nil, nil)
	deferred := kit.DeferredTools()
	names := map[string]bool{}
	for _, d := range deferred {
		names[d.Name()] = true
	}
	skills := Skills()
	if len(skills) != 3 {
		t.Fatalf("skills = %d", len(skills))
	}
	for _, sk := range skills {
		if len(sk.Tools) != 1 || !names[sk.Tools[0]] || sk.Body == "" || sk.Description == "" {
			t.Fatalf("skill %s must unlock exactly one registered deferred tool: %+v", sk.Name, sk.Tools)
		}
		if !strings.Contains(sk.Body, "select:"+sk.Tools[0]) {
			t.Fatalf("skill %s body must explain how to load %s", sk.Name, sk.Tools[0])
		}
	}

	ctx := context.Background()
	unlock := tool.NewUnlockSet(nil)
	set := tool.NewSkillSet(skills, unlock, nil)
	search := tool.NewSearchExtraTools(deferred, unlock)
	query := json.RawMessage(`{"query":"select:` + ToolVerifyCommercial + `"}`)

	res, _ := search.Call(ctx, query, nil)
	if strings.Contains(res.Content, "input_schema") || !strings.Contains(res.Content, `"locked"`) {
		t.Fatalf("locked tool schema leaked: %s", res.Content)
	}
	res, _ = set.Tool().Call(ctx, json.RawMessage(`{"name":"`+SkillCommercialPricing+`"}`), nil)
	if res.IsError || !strings.Contains(res.Content, ToolVerifyCommercial) {
		t.Fatalf("skill result: %+v", res)
	}
	res, _ = search.Call(ctx, query, nil)
	if !strings.Contains(res.Content, "input_schema") || !strings.Contains(res.Content, "ceiling_price") {
		t.Fatalf("unlocked tool schema missing: %s", res.Content)
	}
	if unlock.IsUnlocked(ToolQueryCredentials) || unlock.IsUnlocked(ToolSimulateScoring) {
		t.Fatal("a skill must unlock only its own tool")
	}
}

func TestResidentToolsAreReadOnly(t *testing.T) {
	kit := New(nil, 1, nil, nil)
	for _, tl := range append(kit.ResidentTools(), kit.DeferredTools()...) {
		if !tl.IsReadOnly(nil) || !tl.IsConcurrencySafe(nil) {
			t.Fatalf("%s should be read-only and concurrency-safe", tl.Name())
		}
		if err := tool.ValidateInput(tl.InputSchema(), json.RawMessage(`{}`)); err != nil && tl.Name() != ToolReadChunk && tl.Name() != ToolVerifyCommercial {
			t.Fatalf("%s: unexpected required fields: %v", tl.Name(), err)
		}
	}
}
