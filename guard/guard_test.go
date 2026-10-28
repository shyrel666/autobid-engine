package guard

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/intercept"
	"github.com/autobid/autobid-engine/intercept/intercepttest"
)

func TestGuardDenyPassAndAskExecutionBackfill(t *testing.T) {
	st := intercepttest.New(
		db.InterceptRule{Name: "deny-rm", MatchTarget: "tool_input", Pattern: "rm -rf", Action: intercept.ActionDeny, Message: "禁止删除", Priority: 10},
		db.InterceptRule{Name: "ask-deploy", MatchTarget: "tool_input", Pattern: "deploy", Action: intercept.ActionAsk},
	)
	ic := intercept.New(st, intercept.Options{EnabledTools: []string{"Bash"}})
	g := New(ic)
	ctx := context.Background()

	block, msg, _ := g.PreToolUse(ctx, "Bash", []byte(`{"cmd":"rm -rf /"}`))
	if !block || !strings.Contains(msg, "禁止删除") {
		t.Fatalf("deny: block=%v msg=%q", block, msg)
	}
	if block, _, _ := g.PreToolUse(ctx, "record_fact", []byte(`{}`)); block {
		t.Fatal("non-intercepted tool must pass")
	}

	st.AfterCreate = func(id int64) {
		go func() {
			for !ic.Waiting(id) {
				time.Sleep(time.Millisecond)
			}
			_ = ic.Decide(context.Background(), id, true, "")
		}()
	}
	input := []byte(`{"cmd":"deploy"}`)
	if block, msg, _ := g.PreToolUse(ctx, "Bash", input); block {
		t.Fatalf("approved call blocked: %s", msg)
	}
	g.PostToolUse(ctx, "Bash", input, []byte("deployed"), false)
	if r := st.Row(1); r.Status != db.InterceptAllowed || r.ExecOutcome != "executed" || r.ExecDetail != "deployed" {
		t.Fatalf("pending row = %+v", r)
	}

	audit := g.Audit()
	if len(audit) != 3 || audit[0].Decision != "deny" || audit[1].Decision != "pass" || audit[2].Decision != "ask:allow" || audit[2].PendingID != 1 {
		t.Fatalf("audit = %v", audit)
	}
	if a := g.Attribution(); a["Bash:ok"] != 1 {
		t.Fatalf("attrib = %v", a)
	}
	if prevent, blocking, _ := g.Stop(ctx, nil); prevent || blocking != nil {
		t.Fatal("guard has no Stop semantics")
	}
}

func TestGuardAuditCap(t *testing.T) {
	g := New(nil)
	for k := 0; k < maxAudit+10; k++ {
		g.record(AuditEntry{Tool: "x"})
	}
	if n := len(g.Audit()); n != maxAudit {
		t.Fatalf("audit len = %d", n)
	}
}
