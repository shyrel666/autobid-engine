package harness_test

import (
	"context"
	"strings"
	"testing"

	"github.com/autobid/autobid-engine/internal/harness"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/tool"
)

// §4.3 三阶段闭环：未解锁直调被拒 -> Skill 解锁并披露名字 -> SearchExtraTools 取 Schema -> 直调成功。
func TestSkillProgressiveDisclosureLoop(t *testing.T) {
	rec := &recorder{}
	secret := rec.tool("cert_query", nil)
	unlock := tool.NewUnlockSet(nil)
	skills := tool.NewSkillSet([]tool.Skill{{Name: "资质核验", Body: "逐项核对证书", Tools: []string{"cert_query"}}}, unlock, nil)
	prov := &scripted{replies: []llm.Message{
		call("t1", "cert_query", `{}`),
		call("t2", tool.SkillToolName, `{"name":"资质核验"}`),
		call("t3", tool.SearchExtraToolsName, `{"query":"select:cert_query"}`),
		call("t4", "cert_query", `{}`),
		say("done"),
	}}
	opts := harness.Options{
		Provider:      prov,
		Tools:         []tool.CoreTool{skills.Tool(), tool.NewSearchExtraTools([]tool.CoreTool{secret}, unlock)},
		DeferredTools: []tool.CoreTool{secret},
		UnlockSet:     unlock,
	}
	term := harness.Run(context.Background(), opts, []llm.Message{llm.UserText("go")}, nil)
	res := toolResults(term)
	if len(res) != 4 {
		t.Fatalf("results: %+v", res)
	}
	if !res[0].IsError || !strings.Contains(res[0].Text, "locked") {
		t.Fatalf("stage 0 must be denied: %+v", res[0])
	}
	if !strings.Contains(res[1].Text, "逐项核对证书") || !strings.Contains(res[1].Text, "- cert_query") {
		t.Fatalf("stage B must return body and disclose names: %q", res[1].Text)
	}
	if !strings.Contains(res[2].Text, "input_schema") {
		t.Fatalf("stage C must return schema: %q", res[2].Text)
	}
	if res[3].IsError || len(rec.calls) != 1 {
		t.Fatalf("direct call after unlock: %+v calls=%v", res[3], rec.calls)
	}
}
