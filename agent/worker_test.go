package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/autobid/autobid-engine/internal/tool"
)

func stub(name string) tool.CoreTool {
	return tool.New(tool.Spec{Name: name, Description: name},
		func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
			return tool.Text("ok"), nil
		})
}

func toolNames(ts []tool.CoreTool) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

func TestAssembleDeferredInjectsSearchAndSkill(t *testing.T) {
	def := DeferredInfo{
		Deferred:    []tool.CoreTool{stub("tender_search"), stub("cert_query")},
		GlobalNames: []string{"tender_search", "not_deferred"},
		Skills:      []tool.Skill{{Name: "资质核验", Tools: []string{"cert_query"}}},
	}
	tools, block, unlock, def := assembleDeferred([]tool.CoreTool{stub("record_fact")}, def)
	got := toolNames(tools)
	if !slices.Contains(got, tool.SearchExtraToolsName) || !slices.Contains(got, tool.ExecuteExtraToolName) || !slices.Contains(got, tool.SkillToolName) {
		t.Fatalf("tools = %v", got)
	}
	if slices.Contains(got, "cert_query") || slices.Contains(got, "tender_search") {
		t.Fatal("deferred tools must not be in the visible tool list")
	}
	if !unlock.IsUnlocked("tender_search") || unlock.IsUnlocked("cert_query") || unlock.IsUnlocked("not_deferred") {
		t.Fatalf("initial unlock set = %v", unlock.Names())
	}
	if !strings.Contains(block, "- tender_search") || strings.Contains(block, "cert_query") {
		t.Fatalf("name catalog must list only globally unlocked deferred tools: %s", block)
	}
	// 续跑时经 UnlockSkill 重放历史 Skill 调用。
	def.UnlockSkill("资质核验")
	if !unlock.IsUnlocked("cert_query") {
		t.Fatal("UnlockSkill did not replay the skill")
	}

	plain, block, _, _ := assembleDeferred([]tool.CoreTool{stub("record_fact")}, DeferredInfo{})
	if len(plain) != 1 || block != "" {
		t.Fatal("no deferred config must leave tools untouched")
	}
}
