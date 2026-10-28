package db

// 招标原文、企业资料库与推演报告的集成测试：需要 AUTOBID_TEST_DATABASE_URL。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestTenderDocumentIdempotentAndWindow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tenderNo := uniq("ZB")
	chunks := []DocumentChunk{
		{Seq: 0, Level: 1, SectionCode: "第一章", SectionTitle: "总则", PageNo: 1, Content: "第一章 总则"},
		{Seq: 1, Level: 4, SectionCode: "1.1", SectionTitle: "资格", PageNo: 1, Content: "1.1 资格", TokenEstimate: 3},
		{Seq: 2, Level: 4, SectionCode: "1.2", SectionTitle: "★资质", PageNo: 2, Content: "★1.2 一级资质", IsMandatory: true},
		{Seq: 3, Level: 4, SectionCode: "1.3", SectionTitle: "业绩", PageNo: 0, Content: "1.3 业绩"},
	}
	doc := &TenderDocument{TaskID: f.task.ID, TenderNo: tenderNo, Title: "招标文件", RawText: "原文"}
	id, created, err := f.d.InsertTenderDocument(ctx, doc, chunks)
	if err != nil || !created {
		t.Fatalf("insert: id=%d created=%v err=%v", id, created, err)
	}
	again, created, err := f.d.InsertTenderDocument(ctx, doc, chunks)
	if err != nil || created || again != id {
		t.Fatalf("re-insert must be idempotent: id=%d created=%v err=%v", again, created, err)
	}
	docs, err := f.d.ListTenderDocuments(ctx, f.task.ID)
	if err != nil || len(docs) != 1 || docs[0].ChunkCount != 4 || docs[0].RawText != "" {
		t.Fatalf("list: %+v err=%v", docs, err)
	}
	outline, _ := f.d.DocumentOutline(ctx, f.task.ID, 0, 100)
	if len(outline) != 4 || outline[0].Content != "" || outline[3].PageNo != 1 {
		t.Fatalf("outline: %+v", outline)
	}
	target, window, err := f.d.ChunkWindow(ctx, f.task.ID, outline[2].ID, 1, 5)
	if err != nil || target.SectionCode != "1.2" || !target.IsMandatory || len(window) != 3 || window[0].SectionCode != "1.1" {
		t.Fatalf("window: target=%+v window=%d err=%v", target, len(window), err)
	}

	other := newFixture(t)
	if _, _, err := other.d.ChunkWindow(ctx, other.task.ID, outline[2].ID, 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("chunks of another task must not be readable: %v", err)
	}
	if _, err := other.d.GetTenderDocument(ctx, other.task.ID, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("document of another task: %v", err)
	}
	if _, _, err := f.d.InsertTenderDocument(ctx, &TenderDocument{TaskID: f.task.ID}, nil); err == nil {
		t.Fatal("missing tender_no accepted")
	}
}

func TestCompanyCredentialsUpsertAndFilter(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	tag := uniq("cred")
	valid := "2030-12-31"
	c, created, err := d.UpsertCompanyCredential(ctx, &CompanyCredential{Category: CredentialQualification, Code: tag,
		Title: "信息系统集成一级 " + tag, Attributes: json.RawMessage(`{"level":"一级"}`), ValidUntil: &valid})
	if err != nil || !created || c.ValidUntil == nil || *c.ValidUntil != valid {
		t.Fatalf("create: %+v created=%v err=%v", c, created, err)
	}
	c2, created, err := d.UpsertCompanyCredential(ctx, &CompanyCredential{Category: CredentialQualification, Code: tag,
		Title: "信息系统集成一级（换证）" + tag, Attributes: json.RawMessage(`{"cert_no":"XZ-1"}`)})
	if err != nil || created || c2.ID != c.ID || c2.ValidUntil != nil {
		t.Fatalf("upsert: %+v created=%v err=%v", c2, created, err)
	}
	var attrs map[string]string
	_ = json.Unmarshal(c2.Attributes, &attrs)
	if attrs["level"] != "一级" || attrs["cert_no"] != "XZ-1" {
		t.Fatalf("attributes must merge: %v", attrs)
	}

	expired := "2020-01-01"
	if _, _, err := d.UpsertCompanyCredential(ctx, &CompanyCredential{Category: CredentialPersonnel, Title: "项目经理 " + tag,
		Attributes: json.RawMessage(`{"title":"高级工程师"}`), ValidUntil: &expired}); err != nil {
		t.Fatal(err)
	}
	all, err := d.ListCompanyCredentials(ctx, CredentialFilter{Terms: []string{tag}})
	if err != nil || len(all) != 2 {
		t.Fatalf("terms filter: %d err=%v", len(all), err)
	}
	cur, _ := d.ListCompanyCredentials(ctx, CredentialFilter{Terms: []string{tag}, ValidOn: "2026-10-28"})
	if len(cur) != 1 || cur[0].ID != c.ID {
		t.Fatalf("valid_on filter: %+v", cur)
	}
	people, _ := d.ListCompanyCredentials(ctx, CredentialFilter{Category: CredentialPersonnel, Terms: []string{"高级工程师", tag}})
	if len(people) == 0 || people[0].Category != CredentialPersonnel {
		t.Fatalf("category + attribute text filter: %+v", people)
	}
	if got, _ := d.ListCompanyCredentials(ctx, CredentialFilter{Terms: []string{"%"}}); len(got) > 0 {
		for _, g := range got {
			if !strings.Contains(g.Code+g.Title+string(g.Attributes), "%") {
				t.Fatalf("LIKE wildcard not escaped: %+v", g)
			}
		}
	}
	if _, _, err := d.UpsertCompanyCredential(ctx, &CompanyCredential{Category: "x", Title: "y", ValidUntil: &tag}); err == nil {
		t.Fatal("invalid valid_until accepted")
	}
}

func TestTaskReportEvidenceChain(t *testing.T) {
	tenderNo := uniq("ZB")
	f := newFixture(t, TaskScope{TenderNo: tenderNo})
	ctx := context.Background()
	ref := json.RawMessage(`{"doc_id":1,"chunk_id":2,"section_code":"2.1.2","page_no":3}`)
	clause, _, err := f.as.UpsertAsset(ctx, &Asset{Type: AssetQualification, TenderNo: tenderNo, Code: "2.1.2", Title: "集成一级资质",
		IsMandatory: true, SourceRef: ref}, 0)
	if err != nil {
		t.Fatal(err)
	}
	unverified, _, _ := f.as.UpsertAsset(ctx, &Asset{Type: AssetClause, TenderNo: tenderNo, Code: "9.9", IsMandatory: true}, 0)
	intent, err := f.es.AddIntentTx(ctx, AddIntentInput{Payload: map[string]any{"summary": "核验资质"}, Parents: []int64{f.origin}})
	if err != nil {
		t.Fatal(err)
	}
	fact, err := f.es.RecordFactWithYieldsTx(ctx, intent, map[string]any{"summary": "我方持有一级资质", "evidence": "证书 XZ-1",
		"source_ref": ref}, 0, "work#1", []int64{clause})
	if err != nil {
		t.Fatal(err)
	}
	byAsset, err := f.es.ConfirmedFactsByAsset(ctx, []int64{clause, unverified})
	if err != nil || len(byAsset[clause]) != 1 || byAsset[clause][0].ID != fact || len(byAsset[unverified]) != 0 {
		t.Fatalf("ConfirmedFactsByAsset: %+v err=%v", byAsset, err)
	}
	if _, err := f.es.RecordFinding(ctx, RecordFindingInput{TaskID: f.task.ID, IntentID: intent, Category: FindingCompliance,
		Severity: SeverityInfo, Title: "资质满足", Detail: map[string]any{"source_refs": []json.RawMessage{ref}},
		Evidence: []int64{fact}, Anchors: []int64{clause}, Worker: "work#1"}); err != nil {
		t.Fatal(err)
	}

	rep, err := f.d.TaskReport(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Goal == "" || rep.Coverage.Target != 2 || rep.Coverage.Tested != 1 {
		t.Fatalf("goal/coverage: %q %+v", rep.Goal, rep.Coverage)
	}
	s := rep.Summary
	if s.Elements != 2 || s.MandatoryElements != 2 || s.MandatoryVerified != 1 || s.Facts != 1 || s.Findings != 1 || s.BySeverity[SeverityInfo] != 1 {
		t.Fatalf("summary: %+v", s)
	}
	for _, el := range rep.Elements {
		if el.AssetID == clause && (!el.Verified || len(el.FactIDs) != 1 || len(el.FindingIDs) != 1 || len(el.SourceRef) == 0) {
			t.Fatalf("verified element: %+v", el)
		}
		if el.AssetID == unverified && (el.Verified || el.SourceRef != nil) {
			t.Fatalf("unverified element: %+v", el)
		}
	}
	fi := rep.Findings[0]
	if len(fi.SourceRefs) != 1 || len(fi.Anchors) != 1 || fi.Anchors[0].AssetID != clause || len(fi.Evidence) != 1 {
		t.Fatalf("finding: %+v", fi)
	}
	ev := fi.Evidence[0]
	var evRef struct {
		ChunkID int64 `json:"chunk_id"`
	}
	_ = json.Unmarshal(ev.SourceRef, &evRef)
	if ev.NodeID != fact || ev.Evidence != "证书 XZ-1" || evRef.ChunkID != 2 ||
		len(ev.Anchors) != 1 || ev.Inherited {
		t.Fatalf("evidence: %+v", ev)
	}
}
