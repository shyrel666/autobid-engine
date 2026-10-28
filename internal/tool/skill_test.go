package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestSkillUnlocksAndDisclosesNames(t *testing.T) {
	unlock := NewUnlockSet([]string{"global_lookup"})
	set := NewSkillSet([]Skill{
		{Name: "资质核验", Description: "核对资质证书", Body: "按 4.x 条逐项核对。", Tools: []string{"cert_query", "global_lookup"}},
	}, unlock, nil)
	sk := set.Tool()
	if !strings.Contains(sk.Description(), "资质核验") {
		t.Fatalf("skill catalog missing from description: %s", sk.Description())
	}
	res, err := sk.Call(context.Background(), json.RawMessage(`{"name":"资质核验"}`), nil)
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	if !strings.HasPrefix(res.Content, "按 4.x 条逐项核对。") || !strings.Contains(res.Content, "- cert_query") ||
		strings.Contains(res.Content, "- global_lookup") {
		t.Fatalf("result must include body and only newly unlocked names: %q", res.Content)
	}
	if !unlock.IsUnlocked("cert_query") {
		t.Fatal("skill tools not unlocked")
	}
	if again, _ := sk.Call(context.Background(), json.RawMessage(`{"name":"资质核验"}`), nil); strings.Contains(again.Content, "<deferred-tools>") {
		t.Fatal("second invocation must not re-disclose already unlocked names")
	}
	if bad, _ := sk.Call(context.Background(), json.RawMessage(`{"name":"nope"}`), nil); !bad.IsError {
		t.Fatal("unknown skill must error")
	}

	invoked := ""
	dyn := NewSkillSet([]Skill{{Name: "mcp"}}, NewUnlockSet(nil), func(name string) []string {
		invoked = name
		return []string{"mcp_search"}
	})
	if fresh := dyn.Unlock("mcp"); len(fresh) != 1 || invoked != "mcp" {
		t.Fatalf("OnInvoke not used: %v %q", fresh, invoked)
	}
}
