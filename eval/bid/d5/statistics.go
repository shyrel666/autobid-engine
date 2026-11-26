package main

import "math"

type report struct {
	Version              int             `json:"version"`
	ExerciseID           string          `json:"exercise_id"`
	Level                string          `json:"evidence_level"`
	Mode                 string          `json:"mode"`
	InputSHA256          inputHashes     `json:"input_sha256"`
	Reviewers            int             `json:"reviewers"`
	Drafts               int             `json:"drafts"`
	Projects             []projectResult `json:"projects"`
	MeanDifferencePP     float64         `json:"mean_difference_pp"`
	Paired95CI           interval        `json:"paired_95_ci_pp"`
	ICCAbsoluteAgreement *float64        `json:"icc_a_1"`
	MarginPP             float64         `json:"illustrative_margin_pp"`
	MarginMet            bool            `json:"illustrative_margin_met"`
}

type inputHashes struct {
	Plan   string `json:"plan"`
	Scores string `json:"scores"`
	Key    string `json:"key"`
}

type projectResult struct {
	ProjectID             string   `json:"project_id"`
	Family                string   `json:"family"`
	AssistedDraftID       string   `json:"assisted_draft_id"`
	ManualDraftID         string   `json:"manual_draft_id"`
	AssistedMeanPct       float64  `json:"assisted_mean_pct"`
	ManualMeanPct         float64  `json:"manual_mean_pct"`
	DifferencePP          float64  `json:"difference_pp"`
	ReferenceTechnicalPct *float64 `json:"reference_technical_pct,omitempty"`
	ManualMinusReference  *float64 `json:"manual_minus_reference_pp,omitempty"`
}

type interval struct {
	Lower    float64 `json:"lower"`
	Upper    float64 `json:"upper"`
	Critical float64 `json:"t_critical"`
	DF       int     `json:"df"`
}

func summarize(plan reviewPlan, panel map[string][]float64, key unblindKey) report {
	byProject := make(map[string]draftPair, len(key.Pairs))
	for _, pair := range key.Pairs {
		byProject[pair.ProjectID] = pair
	}
	result := report{
		Version:    schemaVersion,
		ExerciseID: plan.ExerciseID,
		Level:      "E",
		Mode:       "synthetic_rehearsal",
		Reviewers:  len(plan.Reviewers),
		Drafts:     len(panel),
		MarginPP:   *plan.MarginPP,
	}
	differences := make([]float64, 0, len(plan.Projects))
	ratings := make([][]float64, 0, len(panel))
	for _, p := range plan.Projects {
		pair := byProject[p.ID]
		assisted, manual := panel[pair.AssistedDraftID], panel[pair.ManualDraftID]
		row := projectResult{
			ProjectID:             p.ID,
			Family:                p.Family,
			AssistedDraftID:       pair.AssistedDraftID,
			ManualDraftID:         pair.ManualDraftID,
			AssistedMeanPct:       mean(assisted),
			ManualMeanPct:         mean(manual),
			ReferenceTechnicalPct: pair.ReferenceTechnicalPct,
		}
		row.DifferencePP = row.AssistedMeanPct - row.ManualMeanPct
		if row.ReferenceTechnicalPct != nil {
			calibration := row.ManualMeanPct - *row.ReferenceTechnicalPct
			row.ManualMinusReference = &calibration
		}
		result.Projects = append(result.Projects, row)
		differences = append(differences, row.DifferencePP)
		ratings = append(ratings, assisted, manual)
	}
	result.MeanDifferencePP = mean(differences)
	result.Paired95CI = pairedInterval(differences)
	result.ICCAbsoluteAgreement = iccAgreement(ratings)
	result.MarginMet = result.Paired95CI.Lower >= -result.MarginPP
	return result
}

func mean(values []float64) float64 {
	var sum float64
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

func pairedInterval(differences []float64) interval {
	center := mean(differences)
	var sumSquares float64
	for _, value := range differences {
		deviation := value - center
		sumSquares += deviation * deviation
	}
	df := len(differences) - 1
	critical := t975(df)
	halfWidth := critical * math.Sqrt(sumSquares/float64(df)/float64(len(differences)))
	return interval{Lower: center - halfWidth, Upper: center + halfWidth, Critical: critical, DF: df}
}

func t975(df int) float64 {
	// NIST 0.975 quantiles (df 1 through 30). For larger samples, use the
	// slightly more conservative df=30 quantile.
	critical := [...]float64{
		12.706, 4.303, 3.182, 2.776, 2.571, 2.447, 2.365, 2.306, 2.262, 2.228,
		2.201, 2.179, 2.160, 2.145, 2.131, 2.120, 2.110, 2.101, 2.093, 2.086,
		2.080, 2.074, 2.069, 2.064, 2.060, 2.056, 2.052, 2.048, 2.045, 2.042,
	}
	return critical[min(df, len(critical))-1]
}

// iccAgreement calculates the two-way absolute-agreement single-rating
// ICC(A,1). Each row is one draft rated by the same panel.
func iccAgreement(ratings [][]float64) *float64 {
	draftCount, reviewerCount := len(ratings), len(ratings[0])
	reviewerMeans := make([]float64, reviewerCount)
	draftMeans := make([]float64, draftCount)
	for i, row := range ratings {
		draftMeans[i] = mean(row)
		for j, rating := range row {
			reviewerMeans[j] += rating
		}
	}
	drafts, reviewers := float64(draftCount), float64(reviewerCount)
	for j := range reviewerMeans {
		reviewerMeans[j] /= drafts
	}
	grandMean := mean(draftMeans)

	var ssDrafts, ssReviewers, ssResidual float64
	for i, row := range ratings {
		draftDelta := draftMeans[i] - grandMean
		ssDrafts += draftDelta * draftDelta
		for j, rating := range row {
			residual := rating - draftMeans[i] - reviewerMeans[j] + grandMean
			ssResidual += residual * residual
		}
	}
	for _, reviewerMean := range reviewerMeans {
		reviewerDelta := reviewerMean - grandMean
		ssReviewers += reviewerDelta * reviewerDelta
	}

	msDrafts := reviewers * ssDrafts / (drafts - 1)
	msReviewers := drafts * ssReviewers / (reviewers - 1)
	msResidual := ssResidual / ((drafts - 1) * (reviewers - 1))
	denom := msDrafts + (reviewers-1)*msResidual + reviewers*(msReviewers-msResidual)/drafts
	if denom <= 0 {
		return nil
	}
	icc := (msDrafts - msResidual) / denom
	return &icc
}
