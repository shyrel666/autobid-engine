package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyntheticRehearsal(t *testing.T) {
	dir := filepath.Join("..", "fixtures", "d5")
	result, err := rehearse(filepath.Join(dir, "plan.json"), filepath.Join(dir, "scores.csv"), filepath.Join(dir, "key.json"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Level != "E" || result.Mode != "synthetic_rehearsal" ||
		result.Reviewers != 3 || result.Drafts != 8 || len(result.Projects) != 4 || result.MarginMet {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(result.InputSHA256.Plan) != 64 || len(result.InputSHA256.Scores) != 64 || len(result.InputSHA256.Key) != 64 {
		t.Fatalf("missing input fingerprints: %+v", result.InputSHA256)
	}
	requireNear(t, result.MeanDifferencePP, 0.3125)
	requireNear(t, result.Paired95CI.Lower, -5.838352206226966)
	requireNear(t, result.Paired95CI.Upper, 6.463352206226966)
	if result.ICCAbsoluteAgreement == nil {
		t.Fatal("missing ICC")
	}
	requireNear(t, *result.ICCAbsoluteAgreement, 0.9018956850489817)
	requireNear(t, result.Projects[1].ManualMeanPct, 100*143.0/180.0)
	if first, last := result.Projects[0], result.Projects[3]; first.ManualMinusReference == nil ||
		*first.ManualMinusReference != 1 || last.ManualMinusReference != nil {
		t.Fatalf("invalid calibration: first=%+v last=%+v", first, last)
	}
}

func TestPanelRejectsInvalidScores(t *testing.T) {
	dir := filepath.Join("..", "fixtures", "d5")
	plan, _, err := loadJSON[reviewPlan](filepath.Join(dir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	drafts, err := validatePlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "scores.csv"))
	if err != nil {
		t.Fatal(err)
	}
	const entry = "R01,T2C,S1,49"
	for _, tc := range []struct {
		name, replacement, reason string
	}{
		{"missing", "", "缺少评审"},
		{"duplicate", entry + "\n" + entry, "重复记录"},
		{"unknown reviewer", "R99,T2C,S1,49", "编号未知"},
		{"unknown draft", "R01,F9F,S1,49", "编号未知"},
		{"wrong criterion", "R01,T2C,S3,49", "不属于稿件"},
		{"over maximum", "R01,T2C,S1,61", "分数须在"},
		{"negative", "R01,T2C,S1,-1", "分数须在"},
		{"nonfinite", "R01,T2C,S1,NaN", "分数须在"},
		{"missing value", "R01,T2C,S1,", "分数须在"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readPanel(strings.NewReader(strings.Replace(string(data), entry, tc.replacement, 1)), plan, drafts)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("got %v, want %q", err, tc.reason)
			}
		})
	}
}

func TestRehearsalRejectsDuplicateFamiliesAndInvalidKey(t *testing.T) {
	dir := filepath.Join("..", "fixtures", "d5")
	plan, _, err := loadJSON[reviewPlan](filepath.Join(dir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := loadJSON[unblindKey](filepath.Join(dir, "key.json"))
	if err != nil {
		t.Fatal(err)
	}
	key.Pairs[0].AssistedDraftID = key.Pairs[1].AssistedDraftID
	if err := validateKey(plan, key); err == nil {
		t.Fatal("accepted a draft assigned to another project")
	}
	plan.Projects[0].Family = plan.Projects[1].Family
	if _, err := validatePlan(plan); err == nil {
		t.Fatal("accepted correlated projects from a duplicate family")
	}
}

func TestICCAbsoluteAgreementPenalizesReviewerOffset(t *testing.T) {
	result := iccAgreement([][]float64{{1, 2, 3}, {2, 3, 4}, {3, 4, 5}, {4, 5, 6}})
	if result == nil {
		t.Fatal("missing ICC")
	}
	requireNear(t, *result, 0.625)

	result = iccAgreement([][]float64{{10, 10, 10}, {10, 10, 10}})
	if result != nil {
		t.Fatalf("ICC should be undefined for a constant panel, got %v", *result)
	}
}

func requireNear(t *testing.T, got, want float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %g, want %g", got, want)
	}
}
