package main

import (
	"context"
	"strings"
	"testing"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/ingest"
)

func parseFixture(t *testing.T, name string, data []byte) *ingest.Parsed {
	t.Helper()
	p, err := ingest.Parse(context.Background(), ingest.File{Name: name, MIME: demoDOCXMIME, Data: data}, ingest.ParseOptions{})
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return p
}

func sectionByCode(secs []ingest.Section, code string) *ingest.Section {
	for i := range secs {
		if secs[i].Code == code {
			return &secs[i]
		}
	}
	return nil
}

func TestFixturesAreDeterministicAndParseable(t *testing.T) {
	a := buildTenderDOCX()
	b := buildTenderDOCX()
	if string(a) != string(b) {
		t.Fatal("tender docx is not deterministic")
	}
	c1, c2 := buildCorrectionDOCX(), buildCorrectionDOCX()
	if string(c1) != string(c2) {
		t.Fatal("correction docx is not deterministic")
	}
	if string(buildG09TenderDOCX()) != string(buildG09TenderDOCX()) {
		t.Fatal("g09 docx is not deterministic")
	}

	tender := parseFixture(t, "tender.docx", a)
	secs := tender.Sections()
	wantCodes := []string{"第一章", "1.1", "1.1.1", "1.1.2", "1.2", "1.3", "1.4",
		"第二章", "2.1", "2.1.1", "2.1.2", "2.1.3", "2.2", "2.2.1", "2.2.2", "2.3", "2.3.1", "2.3.2",
		"第三章", "3.1"}
	for _, code := range wantCodes {
		if sectionByCode(secs, code) == nil {
			t.Errorf("tender chunk %s missing (chunks=%d)", code, len(secs))
		}
	}
	if s := sectionByCode(secs, "1.1.1"); s == nil || !s.Mandatory || !strings.Contains(s.Content, "安全技术防范") {
		t.Errorf("1.1.1 must be a mandatory ★ clause with its body, got %+v", s)
	}
	for code, rows := range map[string]int{"1.2": 4, "1.3": 3, "3.1": 4} {
		s := sectionByCode(secs, code)
		if s == nil || len(s.Tables) != 1 {
			t.Fatalf("%s should hold exactly one table", code)
		}
		if got := len(s.Tables[0].Grid); got != rows {
			t.Errorf("%s table rows = %d, want %d", code, got, rows)
		}
	}
	if s := sectionByCode(secs, "3.1"); s.Tables[0].Grid[1][1] != "技术方案" || s.Tables[0].Grid[1][2] != "50" {
		t.Errorf("scoring table header/row mismatch: %v", s.Tables[0].Grid)
	}

	correction := parseFixture(t, "correction.docx", c1)
	if s := sectionByCode(correction.Sections(), "2.3.2"); s == nil || !strings.Contains(s.Content, "现更正为") {
		t.Fatalf("correction chunk missing or has no amendment wording: %+v", s)
	}
	g09 := parseFixture(t, "g09.docx", buildG09TenderDOCX())
	if sectionByCode(g09.Sections(), "1.1") == nil || sectionByCode(g09.Sections(), "1.2") == nil {
		t.Fatal("g09 fixture chunks missing")
	}
}

func TestStubDraftAvoidsNegativeAndExtraNumbers(t *testing.T) {
	sec := &db.ProposalSection{Code: "5", Title: "其他必答项响应"}
	reqs := []*db.RequirementItem{
		{Code: "RQ-1", Category: db.ReqNonScoringRequired, Title: "2.1.1 工期要求",
			Description: "投标人须承诺总工期不超过 90 日历天，自合同签订之日起计算。"},
		{Code: "RQ-2", Category: db.ReqNonScoringRequired, Title: "2.2.2 修复要求",
			Description: "投标人须承诺一般故障在 24 小时内修复完毕。"},
	}
	text := stubSectionContent(sec, reqs)
	for _, banned := range []string{"不满足", "无法满足", "负偏离", "不具备", "未取得", "不符合"} {
		if strings.Contains(text, banned) {
			t.Errorf("stub text contains banned negative phrase %q", banned)
		}
	}
	if !strings.Contains(text, "90 日历天") || !strings.Contains(text, "24 小时") {
		t.Error("stub text must keep the original requirement thresholds")
	}
	if strings.Contains(text, "120") || strings.Contains(text, "48 小时") {
		t.Error("stub text must not introduce a second value for the same metric")
	}
}
