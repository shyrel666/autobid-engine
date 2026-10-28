package retrievaleval

import (
	"math"
	"testing"
)

func TestEquivalentEvidenceDoesNotInflateRecall(t *testing.T) {
	c := Case{Evidence: []EvidenceGroup{{[]string{"a", "a-copy"}, 3}, {[]string{"b"}, 2}}}
	s := Measure(c, []string{"a", "a", "a-copy", "noise", "b"})
	if s.Recall20 != 1 || !s.Complete20 || s.NDCG10 >= 1 || math.IsNaN(s.NDCG10) {
		t.Fatalf("%+v", s)
	}
	if v := Measure(c, []string{"a", "a-copy"}); v.Recall20 != .5 || v.Complete20 {
		t.Fatalf("%+v", v)
	}
}
func TestRejectsFamilyLeakage(t *testing.T) {
	d := Dataset{Version: "v1", Snapshot: "x", SourcePermission: "synthetic", Cases: []Case{
		{ID: "1", Family: "p", Split: "tune", Category: "none", Query: "a", Unanswerable: true},
		{ID: "2", Family: "p", Split: "heldout", Category: "none", Query: "b", Unanswerable: true},
	}}
	if d.Validate() == nil {
		t.Fatal("accepted family leakage")
	}
	d.Cases[1].Family = "q"
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
}
