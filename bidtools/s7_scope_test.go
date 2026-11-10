package bidtools

import (
	"regexp"
	"strings"
	"testing"

	"github.com/autobid/autobid-engine/filework"
	"github.com/autobid/autobid-engine/internal/tool"
	"github.com/autobid/autobid-engine/toolhealth"
)

func toolNames(tools []tool.CoreTool) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		out[t.Name()] = true
	}
	return out
}

// TestSectionWriterToolScope: the HTTP section writer only reads and binds evidence. Saving the
// revision is the host's job, so a model that talks its way past the prompt still has no tool
// for writing a section, starting a background job, reviewing or exporting.
func TestSectionWriterToolScope(t *testing.T) {
	kit := New(nil, 1, nil, nil)
	writer := toolNames(kit.SectionWriterTools())
	for _, want := range []string{ToolKBSearchProposals, ToolKBSearchProducts, ToolKBReadChunk, ToolKBVerifyEvidence, ToolRequirementBindEvidence} {
		if !writer[want] {
			t.Errorf("section writer lost %s", want)
		}
	}
	for _, banned := range []string{ToolSectionWriteDraft, ToolChartRenderSubmit, ToolProposalExportSubmit, ToolDocumentParseSubmit} {
		if writer[banned] {
			t.Errorf("section writer must not hold %s", banned)
		}
	}
	if n := len(writer); n > 9 {
		t.Errorf("section writer has %d tools; a new one needs a deliberate decision", n)
	}
}

// TestWorkerHasNoHumanDecisionTools: Skill visibility is not authority. Adoption, approval and
// sign-off stay with people, so no tool may exist for them at all.
func TestWorkerHasNoHumanDecisionTools(t *testing.T) {
	kit := New(nil, 1, nil, nil)
	human := regexp.MustCompile(`(?i)(adopt|approve|signoff|sign_off|reject|confirm)`)
	for name := range toolNames(append(kit.ResidentTools(), kit.DeferredTools()...)) {
		if human.MatchString(name) {
			t.Errorf("worker tool %s looks like a human decision", name)
		}
	}
}

// TestFourSkillsAreComplete checks the acceptance item for the four delivered skills: each has
// real tools with schemas and tells the agent what to do on failure.
func TestFourSkillsAreComplete(t *testing.T) {
	kit := New(nil, 1, nil, nil)
	registered := map[string]tool.CoreTool{}
	for _, c := range append(kit.ResidentTools(), kit.DeferredTools()...) {
		registered[c.Name()] = c
	}
	failure := regexp.MustCompile(`error_code|缺口|失败|找不到`)
	skills := map[string]tool.Skill{}
	for _, s := range Skills() {
		skills[s.Name] = s
	}
	for _, name := range []string{SkillDocumentProcessing, SkillKnowledgeReuse, SkillDiagramGeneration, SkillProposalExport} {
		s, ok := skills[name]
		if !ok {
			t.Errorf("skill %s is missing", name)
			continue
		}
		if len(s.Tools) == 0 {
			t.Errorf("skill %s unlocks no tools", name)
		}
		for _, n := range s.Tools {
			c, ok := registered[n]
			if !ok {
				t.Errorf("skill %s lists unregistered tool %s", name, n)
				continue
			}
			if len(c.InputSchema()) == 0 {
				t.Errorf("tool %s has no schema", n)
			}
		}
		if !failure.MatchString(s.Body) {
			t.Errorf("skill %s says nothing about failure", name)
		}
		if strings.TrimSpace(s.Description) == "" {
			t.Errorf("skill %s has no description", name)
		}
	}
}

// TestSkillCatalogFollowsHostSwitches: a skill whose server switch is off stays in the catalog, so the
// agent can explain the gap, but it says up front that it is unavailable; tool_runtime_status reports the
// same switches. Turning the switches on restores the plain catalog.
func TestSkillCatalogFollowsHostSwitches(t *testing.T) {
	base := map[string]tool.Skill{}
	for _, s := range Skills() {
		base[s.Name] = s
	}
	off := New(nil, 1, nil, nil).WithRuntime(toolhealth.New())
	for _, s := range off.Skills() {
		gated := s.Name == SkillDocumentProcessing || s.Name == SkillProposalExport
		if gated && (!strings.HasPrefix(s.Description, "【") || !strings.Contains(s.Description, "FEATURE_DISABLED") || !strings.HasPrefix(s.Body, "【")) {
			t.Errorf("%s must say it is unavailable: %q", s.Name, s.Description)
		}
		if !gated && (s.Description != base[s.Name].Description || s.Body != base[s.Name].Body) {
			t.Errorf("%s changed although it has no server switch", s.Name)
		}
	}
	out, _ := callKBTool(t, off.RuntimeTools(), ToolRuntimeStatus, `{}`)
	features, ok := out["features"].(map[string]any)
	if !ok {
		t.Fatalf("tool_runtime_status has no features: %v", out)
	}
	for _, name := range []string{"file_processing", "agent_export"} {
		f, _ := features[name].(map[string]any)
		if f["enabled"] != false || f["note"] == nil {
			t.Errorf("%s while off: %v", name, f)
		}
	}

	on := New(nil, 1, nil, nil).WithRuntime(toolhealth.New()).WithExportSubmit(true).WithFileProcessing(&filework.Processor{}, nil)
	for _, s := range on.Skills() {
		if s.Description != base[s.Name].Description || s.Body != base[s.Name].Body {
			t.Errorf("%s still marked unavailable with its switch on", s.Name)
		}
	}
	out, _ = callKBTool(t, on.RuntimeTools(), ToolRuntimeStatus, `{}`)
	features, _ = out["features"].(map[string]any)
	for _, name := range []string{"file_processing", "agent_export"} {
		f, _ := features[name].(map[string]any)
		if f["enabled"] != true || f["note"] != nil {
			t.Errorf("%s while on: %v", name, f)
		}
	}
	if out, _ = callKBTool(t, on.RuntimeTools(), ToolRuntimeStatus, `{"capability":"office"}`); out["features"] != nil {
		t.Error("a single-capability query must not carry the server switches")
	}
}
