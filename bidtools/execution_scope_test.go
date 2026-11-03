package bidtools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
)

func TestExecutionScopeResolvesCodesBeforeAuthorizing(t *testing.T) {
	d := dbtest.Open(t)
	p := dbtest.Ready(t, d, dbtest.DefaultSpec(0))
	ctx := context.Background()
	task, err := d.GetTask(ctx, p.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	kit := New(d, p.TaskID, db.NewAssetStore(d, p.TaskID), db.NewExplorationStore(d, task.ExplorationID))
	ctx = db.WithExecutionScope(ctx, &db.IntentMandate{RestrictObjects: true, AllowedSectionIDs: []int64{p.Sections["1"]}})
	if _, err = kit.resolveSection(ctx, 0, "5"); err == nil {
		t.Fatal("section code bypassed scope")
	}
	if _, err = kit.resolveSection(ctx, p.Sections["5"], ""); err == nil {
		t.Fatal("section ID bypassed scope")
	}
	if _, err = kit.resolveSection(ctx, 0, "1"); err != nil {
		t.Fatal(err)
	}
	reqs, err := d.ListRequirements(context.Background(), db.DefaultTenant, p.TaskID)
	if err != nil || len(reqs.Items) == 0 {
		t.Fatalf("requirements: %v", err)
	}
	raw, _ := json.Marshal(map[string]any{"requirement_code": reqs.Items[0].Code, "evidence_gap": "越界修改"})
	for _, tl := range kit.DeferredTools() {
		if tl.Name() != ToolRequirementBindEvidence {
			continue
		}
		res, err := tl.Call(ctx, raw, nil)
		if err != nil || !res.IsError || !strings.Contains(res.Content, "execution scope") {
			t.Fatalf("requirement code bypassed scope: %+v %v", res, err)
		}
		return
	}
	t.Fatal(fmt.Sprintf("%s not registered", ToolRequirementBindEvidence))
}
