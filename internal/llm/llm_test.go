package llm

import "testing"

func TestCommitsOutput(t *testing.T) {
	cases := []struct {
		ev   StreamEvent
		want bool
	}{
		{StreamEvent{Type: SEMessageStart}, false},
		{StreamEvent{Type: SETextDelta}, false},
		{StreamEvent{Type: SETextDelta, Text: "a"}, true},
		{StreamEvent{Type: SEThinkingDelta}, false},
		{StreamEvent{Type: SEThinkingDelta, Text: "a"}, true},
		{StreamEvent{Type: SEToolUseStart}, true},
		{StreamEvent{Type: SEToolUseDelta}, true},
		{StreamEvent{Type: SEToolUseEnd}, true},
		{StreamEvent{Type: SEMessageStop}, false},
	}
	for _, c := range cases {
		if got := CommitsOutput(c.ev); got != c.want {
			t.Errorf("CommitsOutput(%+v) = %v, want %v", c.ev, got, c.want)
		}
	}
}

func TestAccumulatorAssemblesToolInput(t *testing.T) {
	a := NewAccumulator()
	for _, ev := range []StreamEvent{
		{Type: SEThinkingDelta, Text: "plan"},
		{Type: SETextDelta, Text: "Checking "},
		{Type: SETextDelta, Text: "clause 3.2"},
		{Type: SEToolUseStart, ToolUse: &ToolUse{ID: "t1", Name: "record_fact"}},
		{Type: SEToolUseDelta, Text: `{"summary":`},
		{Type: SEToolUseDelta, Text: `"ok"}`},
		{Type: SEToolUseEnd},
		{Type: SEMessageStop, Usage: &Usage{InputTokens: 3, OutputTokens: 5}},
	} {
		a.Add(ev)
	}
	msg, stop, usage := a.Result()
	if msg.Text() != "Checking clause 3.2" {
		t.Fatalf("text = %q", msg.Text())
	}
	uses := msg.ToolUses()
	if len(uses) != 1 || string(uses[0].Input) != `{"summary":"ok"}` || uses[0].Name != "record_fact" {
		t.Fatalf("tool uses = %+v", uses)
	}
	if stop != StopToolUse || usage.OutputTokens != 5 {
		t.Fatalf("stop=%s usage=%+v", stop, usage)
	}
}
