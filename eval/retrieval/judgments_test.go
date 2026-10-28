package retrievaleval

import "testing"

func TestReviewMissingValuesAreNotScores(t *testing.T) {
	cases := map[string]Case{"a": {ID: "a"}, "b": {ID: "b", Unanswerable: true}}
	rows := []Observation{{CaseID: "a"}, {CaseID: "b"}}
	s, err := SummarizeJudgments(cases, rows, "C", nil)
	if err != nil || s.CitationSupport != nil || s.FullyCorrectRate != nil || s.CostPerCorrect != nil {
		t.Fatalf("%+v %v", s, err)
	}
	yes, no := true, false
	one := 1
	cost := 0.25
	labels := []Judgment{{Group: "C", CaseID: "a", Reviewer: "tester", Note: "checked", Citations: &one, Supported: &one, FullyCorrect: &yes, CostUSD: &cost, PredictedUnanswerable: &no}, {Group: "C", CaseID: "b", Reviewer: "tester", Note: "checked", FullyCorrect: &yes, CostUSD: &cost, PredictedUnanswerable: &yes}}
	s, err = SummarizeJudgments(cases, rows, "C", labels)
	if err != nil || *s.FullyCorrectRate != 1 || *s.CostPerCorrect != .25 || *s.NoAnswerRecall != 1 {
		t.Fatalf("%+v %v", s, err)
	}
	labels = append(labels, labels[0])
	if _, err = SummarizeJudgments(cases, rows, "C", labels); err == nil {
		t.Fatal("accepted duplicate review")
	}
}
