package bidtools

// proposal-review 工具对真实 PostgreSQL 的集成测试（需要 AUTOBID_TEST_DATABASE_URL）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
)

func TestReviewToolsDetectContradictionAndOmission(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	task, err := d.CreateTask(ctx, db.CreateTaskInput{Name: fmt.Sprintf("p6a-tool-%d", time.Now().UnixNano()), Goal: "技术标审查"})
	if err != nil {
		t.Fatal(err)
	}
	kit := New(d, task.ID, db.NewAssetStore(d, task.ID), db.NewExplorationStore(d, task.ExplorationID))
	call := func(name, in string) map[string]any {
		t.Helper()
		for _, tl := range kit.DeferredTools() {
			if tl.Name() != name {
				continue
			}
			res, err := tl.Call(ctx, json.RawMessage(in), nil)
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]any
			if res.IsError {
				return map[string]any{"error": res.Content}
			}
			if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
				t.Fatalf("%s: %s", name, res.Content)
			}
			return out
		}
		t.Fatalf("tool %s not registered", name)
		return nil
	}
	if out := call(ToolFulltextConsistent, `{}`); !strings.Contains(fmt.Sprint(out["error"]), "no approved outline") {
		t.Fatalf("without outline: %+v", out)
	}

	s := func(v string) *string { return &v }
	yes := true
	star, err := d.CreateRequirement(ctx, db.DefaultTenant, task.ID, "r", db.RequirementInput{Code: s("★1"), Category: s(db.ReqMandatoryClause),
		Title: s("★ 售后响应"), Description: s("★ 投标人须承诺故障响应时间不超过2小时。"), IsMandatory: &yes})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ConfirmRequirements(ctx, db.DefaultTenant, task.ID, "r", 0); err != nil {
		t.Fatal(err)
	}
	det, err := d.CreateOutline(ctx, db.DefaultTenant, task.ID, "e", db.OutlineInput{Sections: []db.SectionInput{
		{Code: "1", Title: "总体方案", Level: 1}, {Code: "2", Title: "售后服务", Level: 1, RequirementIDs: []int64{star.ID}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SubmitOutline(ctx, db.DefaultTenant, task.ID, 1, "e"); err != nil {
		t.Fatal(err)
	}
	if det, err = d.ApproveOutline(ctx, db.DefaultTenant, task.ID, 1, "a"); err != nil {
		t.Fatal(err)
	}
	bodies := map[string]string{"1": "本项目总工期 90 天，项目团队共 10 人。", "2": "售后服务：项目团队 12 人驻场。"}
	for _, sec := range det.Sections {
		if _, err := d.SaveSectionRevision(ctx, db.DefaultTenant, "w", db.SaveRevisionInput{TaskID: task.ID, SectionID: sec.ID, BasedOnOutlineVersion: 1,
			Source: db.RevHumanEdit, ContentMD: bodies[sec.Code]}); err != nil {
			t.Fatal(err)
		}
	}
	v := call(ToolComplianceVerify, `{"section_code":"2"}`)
	if v["checked"].(float64) != 1 || v["failed"].(float64) != 1 {
		t.Fatalf("compliance_verify: %+v", v)
	}
	out := call(ToolFulltextConsistent, `{"reverify":true}`)
	cons, _ := out["contradictions"].([]any)
	oms, _ := out["omissions"].([]any)
	if len(cons) != 1 || !strings.Contains(fmt.Sprint(cons[0]), "项目团队人数") || len(oms) != 1 || !strings.Contains(fmt.Sprint(oms[0]), "★1") {
		t.Fatalf("fulltext_consistency_check: %+v", out)
	}
}
