package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/autobid/autobid-engine/db"
)

type ccgpProofAnchor struct {
	Seq     int    `json:"seq"`
	Page    int    `json:"page"`
	Section string `json:"section"`
	Table   *int   `json:"table"`
	Row     *int   `json:"row"`
	Quote   string `json:"quote"`
}

type ccgpProofExpectation struct {
	Name          string          `json:"name"`
	Lot           string          `json:"lot"`
	Points        float64         `json:"points"`
	ScoreGroup    string          `json:"score_group"`
	SourceGroup   string          `json:"source_group"`
	Source        ccgpProofAnchor `json:"source"`
	ExpectProof   map[string]any  `json:"expect_proof"`
	Finding       string          `json:"finding"`
	MissingProof  []string        `json:"missing_proof"`
	ScoreConflict []float64       `json:"score_conflict"`
}

func loadCCGPProofExpectations(t *testing.T, projects []ccgpTender) map[string][]ccgpProofExpectation {
	t.Helper()
	all := map[string][]ccgpProofExpectation{}
	for _, file := range []string{"public-proof-a.json", "public-proof-b.json"} {
		raw, err := os.ReadFile("../eval/bid/fixtures/d2/" + file)
		if err != nil {
			t.Fatal(err)
		}
		var cases map[string][]ccgpProofExpectation
		if err := json.Unmarshal(raw, &cases); err != nil {
			t.Fatal(err)
		}
		for key, entries := range cases {
			if len(all[key]) > 0 || len(entries) == 0 {
				t.Fatalf("duplicated or empty proof audit for %s", key)
			}
			all[key] = entries
		}
	}
	if len(all) != len(projects) {
		t.Fatalf("proof audit covers %d of %d projects", len(all), len(projects))
	}
	for _, p := range projects {
		if len(all[p.Key]) == 0 {
			t.Fatalf("proof audit missing %s", p.Key)
		}
	}
	return all
}

func compactTenderQuote(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

func checkCCGPProofAnchor(t *testing.T, anchor ccgpProofAnchor, chunks []*db.DocumentChunk) *db.DocumentChunk {
	t.Helper()
	if anchor.Seq < 0 || anchor.Seq >= len(chunks) || chunks[anchor.Seq].Seq != anchor.Seq || strings.TrimSpace(anchor.Quote) == "" {
		t.Fatalf("invalid proof source: %+v", anchor)
	}
	c := chunks[anchor.Seq]
	if anchor.Page > 0 && (anchor.Page < c.PageStart || anchor.Page > c.PageEnd) {
		t.Errorf("source page %d is outside chunk %d pages %d–%d", anchor.Page, anchor.Seq, c.PageStart, c.PageEnd)
	}
	if anchor.Section != "" && anchor.Section != c.SectionTitle {
		t.Errorf("chunk %d section = %q, want %q", anchor.Seq, c.SectionTitle, anchor.Section)
	}
	text := c.Content
	if anchor.Table != nil && anchor.Row != nil {
		var tables []struct {
			Grid [][]string `json:"grid"`
		}
		if err := json.Unmarshal(c.Tables, &tables); err != nil {
			t.Fatal(err)
		}
		ti, ri := *anchor.Table, *anchor.Row
		if ti < 0 || ti >= len(tables) || ri < 0 || ri >= len(tables[ti].Grid) {
			t.Fatalf("missing source table %d row %d at chunk %d", ti, ri, anchor.Seq)
		}
		text = strings.Join(tables[ti].Grid[ri], "")
	}
	if !strings.Contains(compactTenderQuote(text), compactTenderQuote(anchor.Quote)) {
		t.Errorf("quote %q missing at chunk %d source table/row", anchor.Quote, anchor.Seq)
	}
	return c
}

func proofSubset(want map[string]any, rules db.ProofRules) []string {
	data, _ := json.Marshal(rules)
	var observed map[string]any
	_ = json.Unmarshal(data, &observed)
	var missing []string
	for key, v := range want {
		if list, ok := v.([]any); ok {
			got, _ := observed[key].([]any)
			if slices.ContainsFunc(list, func(x any) bool {
				return !slices.ContainsFunc(got, func(y any) bool { return reflect.DeepEqual(x, y) })
			}) {
				missing = append(missing, key)
			}
		} else if !reflect.DeepEqual(observed[key], v) {
			missing = append(missing, key)
		}
	}
	slices.Sort(missing)
	return missing
}

func checkCCGPProofExpectations(t *testing.T, ctx context.Context, d *db.DB, taskID int64, res *Result, view *db.RequirementsView, cases []ccgpProofExpectation) {
	t.Helper()
	chunks, err := d.TaskChunks(ctx, taskID, res.Document.ID, res.Chunks)
	if err != nil || len(chunks) != res.Chunks {
		t.Fatalf("load proof source chunks: %d/%d, %v", len(chunks), res.Chunks, err)
	}
	if ev := view.BidEvaluation; ev.ObjectiveScoreLower != 0 || ev.ObjectiveScoreUpper != 0 || ev.EvidenceBackedScore != 0 {
		t.Errorf("public tender without company evidence shows evidence-backed points: %+v", ev)
	}
	for _, want := range cases {
		source := checkCCGPProofAnchor(t, want.Source, chunks)
		var item *db.RequirementItem
		for _, candidate := range view.Items {
			if candidate.Category != db.ReqScoringItem || candidate.ParentID != nil || candidate.IsSuperseded() {
				continue
			}
			name := strings.TrimPrefix(candidate.Title, "【"+db.LotLabel(candidate.Lot)+"】")
			if compactTenderQuote(name) != compactTenderQuote(want.Name) {
				continue
			}
			if item == nil || candidate.Lot == want.Lot && item.Lot != want.Lot ||
				candidate.Lot == item.Lot && candidate.SourceChunkID != nil && *candidate.SourceChunkID == source.ID {
				item = candidate
			}
		}
		var findings []string
		if item == nil {
			findings = append(findings, "score_missing")
		} else {
			var loc struct {
				Group    string    `json:"group"`
				Conflict []float64 `json:"score_conflict"`
			}
			if err := json.Unmarshal(item.SourceLoc, &loc); err != nil {
				t.Fatal(err)
			}
			if item.ScoreWeight != want.Points {
				findings = append(findings, "points_mismatch")
			}
			if item.ScoreGroup != want.ScoreGroup || want.SourceGroup != "" && loc.Group != want.SourceGroup {
				findings = append(findings, "group_mismatch")
			}
			if item.Lot != want.Lot {
				findings = append(findings, "scope_mismatch")
			}
			fields := proofSubset(want.ExpectProof, item.ProofRules)
			if len(fields) > 0 || len(want.MissingProof) > 0 {
				findings = append(findings, "proof_incomplete")
			}
			if len(fields) > 0 {
				t.Logf("D2 proof fields %v differ for %q lot %q", fields, want.Name, want.Lot)
			}
			if len(want.ScoreConflict) > 0 && slices.Equal(loc.Conflict, want.ScoreConflict) && view.BidEvaluation.PriceScoreUnresolved ||
				want.Finding == "source_conflict" && len(want.ScoreConflict) == 0 &&
					slices.ContainsFunc(view.BidEvaluation.Gaps, func(gap string) bool {
						return strings.Contains(gap, "包C") && strings.Contains(gap, "声明 60 分") && strings.Contains(gap, "已提取 50 分")
					}) {
				findings = append(findings, "source_conflict")
			}
		}
		label := fmt.Sprintf("%q lot %q at chunk %d", want.Name, want.Lot, want.Source.Seq)
		if want.Finding == "" && len(findings) > 0 || want.Finding != "" && !slices.Contains(findings, want.Finding) {
			t.Errorf("proof audit %s findings %v, want %q (item %+v)", label, findings, want.Finding, item)
		}
		t.Logf("D2 proof audit %s findings=%v", label, findings)
	}
}
