package retrievaleval

import (
	"fmt"
	"math"
)

// Judgment is independently reviewed against a saved observation. Nil means unmeasured.
type Judgment struct {
	Group                  string   `json:"group"`
	CaseID                 string   `json:"case_id"`
	Repeat                 int      `json:"repeat"`
	Reviewer               string   `json:"reviewer"`
	Note                   string   `json:"note"`
	Citations              *int     `json:"citations"`
	Supported              *int     `json:"supported_citations"`
	PredictedUnanswerable  *bool    `json:"predicted_unanswerable"`
	FullyCorrect           *bool    `json:"fully_correct"`
	MandatoryFalsePositive *bool    `json:"mandatory_false_positive"`
	CostUSD                *float64 `json:"cost_usd"`
}
type JudgmentSummary struct {
	Reviewed                int      `json:"reviewed_observations"`
	CitationSupport         *float64 `json:"citation_support_rate"`
	NoAnswerRecall          *float64 `json:"unanswerable_recall"`
	NoAnswerPrecision       *float64 `json:"unanswerable_precision"`
	FullyCorrectRate        *float64 `json:"fully_correct_task_rate"`
	CostPerCorrect          *float64 `json:"cost_per_correct_task_usd"`
	MandatoryFalsePositives *int     `json:"mandatory_false_positives"`
}

func SummarizeJudgments(cases map[string]Case, rows []Observation, group string, judgments []Judgment) (JudgmentSummary, error) {
	out := JudgmentSummary{}
	known := map[string]bool{}
	seen := map[string]bool{}
	key := func(id string, n int) string { return fmt.Sprintf("%s/%d", id, n) }
	for _, r := range rows {
		known[key(r.CaseID, r.Repeat)] = true
	}
	citations, supports, unanswerable, predicted, truePositive, classified, correct, correctDenom, mandatory, mandatoryDenom := 0, 0, 0, 0, 0, 0, 0, 0, 0, 0
	cost, costCount := 0.0, 0
	for _, j := range judgments {
		if j.Group != group {
			continue
		}
		k := key(j.CaseID, j.Repeat)
		if !known[k] || seen[k] || j.Reviewer == "" || j.Note == "" {
			return out, fmt.Errorf("invalid, duplicate or unreviewed judgment %s", k)
		}
		seen[k] = true
		out.Reviewed++
		if (j.Citations == nil) != (j.Supported == nil) {
			return out, fmt.Errorf("both citation counts required")
		}
		if j.Citations != nil {
			if *j.Citations < 0 || *j.Supported < 0 || *j.Supported > *j.Citations {
				return out, fmt.Errorf("invalid citation counts")
			}
			citations += *j.Citations
			supports += *j.Supported
		}
		if j.PredictedUnanswerable != nil {
			classified++
			if cases[j.CaseID].Unanswerable {
				unanswerable++
			}
			if *j.PredictedUnanswerable {
				predicted++
				if cases[j.CaseID].Unanswerable {
					truePositive++
				}
			}
		}
		if j.FullyCorrect != nil {
			correctDenom++
			if *j.FullyCorrect {
				correct++
			}
		}
		if j.MandatoryFalsePositive != nil {
			mandatoryDenom++
			if *j.MandatoryFalsePositive {
				mandatory++
			}
		}
		if j.CostUSD != nil {
			if *j.CostUSD < 0 || math.IsNaN(*j.CostUSD) || math.IsInf(*j.CostUSD, 0) {
				return out, fmt.Errorf("invalid cost")
			}
			cost += *j.CostUSD
			costCount++
		}
	}
	ratio := func(n, d int) *float64 {
		if d == 0 {
			return nil
		}
		v := float64(n) / float64(d)
		return &v
	}
	out.CitationSupport = ratio(supports, citations)
	// Partial review remains visible via reviewed count; no silent all-case denominators.
	if classified > 0 {
		out.NoAnswerRecall = ratio(truePositive, unanswerable)
		out.NoAnswerPrecision = ratio(truePositive, predicted)
	}
	out.FullyCorrectRate = ratio(correct, correctDenom)
	if correct > 0 && costCount == len(rows) && correctDenom == len(rows) {
		v := cost / float64(correct)
		out.CostPerCorrect = &v
	}
	if mandatoryDenom > 0 {
		out.MandatoryFalsePositives = &mandatory
	}
	return out, nil
}
