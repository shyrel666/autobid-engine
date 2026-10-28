// Package retrievaleval evaluates retrieved evidence groups, not answer fluency.
package retrievaleval

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

type EvidenceGroup struct {
	IDs   []string `json:"ids"`
	Grade int      `json:"grade"`
}
type Case struct {
	ID           string          `json:"id"`
	Family       string          `json:"family"`
	Split        string          `json:"split"`
	Category     string          `json:"category"`
	Query        string          `json:"query"`
	MustTerms    []string        `json:"must_terms,omitempty"`
	Evidence     []EvidenceGroup `json:"evidence"`
	Unanswerable bool            `json:"unanswerable"`
}
type Dataset struct {
	Documents        []Document `json:"documents,omitempty"`
	Version          string     `json:"version"`
	Synthetic        bool       `json:"synthetic"`
	Snapshot         string     `json:"snapshot"`
	SourcePermission string     `json:"source_permission"`
	Cases            []Case     `json:"cases"`
}

// Documents are optional fixtures, seeded only with explicit -seed into a fresh tenant.
type Document struct {
	Key      string          `json:"key"`
	Family   string          `json:"family"`
	Title    string          `json:"title"`
	Category string          `json:"category"`
	Extra    json.RawMessage `json:"extra"`
	Chunks   []string        `json:"chunks"`
}

func (d Dataset) Validate() error {
	if d.Version == "" || d.Snapshot == "" || d.SourcePermission == "" || len(d.Cases) == 0 {
		return fmt.Errorf("version, snapshot, source_permission and cases are required")
	}
	ids, families := map[string]bool{}, map[string]string{}
	documents := map[string]Document{}
	for _, doc := range d.Documents {
		if doc.Key == "" || strings.Contains(doc.Key, ":") || doc.Family == "" || len(doc.Chunks) == 0 {
			return fmt.Errorf("invalid document fixture")
		}
		if _, exists := documents[doc.Key]; exists {
			return fmt.Errorf("duplicate document fixture")
		}
		documents[doc.Key] = doc
	}
	for _, c := range d.Cases {
		if c.ID == "" || ids[c.ID] || c.Family == "" || c.Query == "" || c.Category == "" || (c.Split != "tune" && c.Split != "heldout") {
			return fmt.Errorf("invalid/duplicate case %q", c.ID)
		}
		ids[c.ID] = true
		if s, ok := families[c.Family]; ok && s != c.Split {
			return fmt.Errorf("document-family leakage: %s", c.Family)
		}
		families[c.Family] = c.Split
		if c.Unanswerable != (len(c.Evidence) == 0) {
			return fmt.Errorf("case %s: unanswerable must agree with empty gold evidence", c.ID)
		}
		seen := map[string]bool{}
		for _, g := range c.Evidence {
			if g.Grade < 1 || g.Grade > 3 || len(g.IDs) == 0 {
				return fmt.Errorf("invalid evidence group in %s", c.ID)
			}
			for _, id := range g.IDs {
				if id == "" || seen[id] {
					return fmt.Errorf("ambiguous evidence ID in %s", c.ID)
				}
				seen[id] = true
				if len(documents) > 0 {
					key, seq, ok := strings.Cut(id, ":")
					doc, exists := documents[key]
					n, e := strconv.Atoi(seq)
					if !ok || !exists || e != nil || n < 0 || n >= len(doc.Chunks) || doc.Family != c.Family {
						return fmt.Errorf("case %s: invalid source or family boundary %s", c.ID, id)
					}
				}
			}
		}
	}
	return nil
}

type Score struct {
	Recall20   float64 `json:"recall_at_20"`
	Complete20 bool    `json:"complete_evidence_at_20"`
	NDCG10     float64 `json:"ndcg_at_10"`
}

func Measure(c Case, ranked []string) Score {
	if c.Unanswerable {
		return Score{}
	} // excluded from retrieval denominator
	groups := map[string]int{}
	grades := make([]int, len(c.Evidence))
	for i, g := range c.Evidence {
		grades[i] = g.Grade
		for _, id := range g.IDs {
			groups[id] = i
		}
	}
	seenIDs, seenGroups := map[string]bool{}, map[int]bool{}
	n, found, dcg := 0, 0, 0.0
	for _, id := range ranked {
		if seenIDs[id] {
			continue
		}
		seenIDs[id] = true
		n++
		if n > 20 {
			break
		}
		if g, ok := groups[id]; ok && !seenGroups[g] {
			seenGroups[g] = true
			found++
			if n <= 10 {
				dcg += (math.Pow(2, float64(grades[g])) - 1) / math.Log2(float64(n+1))
			}
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(grades)))
	idcg := 0.0
	for i, g := range grades {
		if i == 10 {
			break
		}
		idcg += (math.Pow(2, float64(g)) - 1) / math.Log2(float64(i+2))
	}
	return Score{Recall20: float64(found) / float64(len(grades)), Complete20: found == len(grades), NDCG10: dcg / idcg}
}

type Observation struct {
	UsedCalls            int      `json:"used_calls,omitempty"`
	ReservedTokens       int      `json:"reserved_tokens,omitempty"`
	ActualReasonerTokens int      `json:"actual_reasoner_tokens,omitempty"`
	Status               string   `json:"status,omitempty"`
	CaseID               string   `json:"case_id"`
	Repeat               int      `json:"repeat"`
	Ranked               []string `json:"ranked"`
	Score                Score    `json:"score"`
	LatencyMS            float64  `json:"latency_ms"`
	RunID                int64    `json:"run_id,omitempty"`
	Degraded             []string `json:"degraded,omitempty"`
	Error                string   `json:"error,omitempty"`
}
type Summary struct {
	Count      int     `json:"count"`
	Answerable int     `json:"answerable"`
	Recall20   float64 `json:"recall_at_20"`
	Complete20 float64 `json:"complete_evidence_at_20"`
	NDCG10     float64 `json:"ndcg_at_10"`
	P95MS      float64 `json:"p95_ms"`
	Errors     int     `json:"errors"`
}

func Summarize(cases map[string]Case, rows []Observation) Summary {
	s := Summary{Count: len(rows)}
	times := []float64{}
	for _, r := range rows {
		times = append(times, r.LatencyMS)
		if r.Error != "" {
			s.Errors++
		}
		if !cases[r.CaseID].Unanswerable {
			s.Answerable++
			s.Recall20 += r.Score.Recall20
			s.NDCG10 += r.Score.NDCG10
			if r.Score.Complete20 {
				s.Complete20++
			}
		}
	}
	if s.Answerable > 0 {
		n := float64(s.Answerable)
		s.Recall20 /= n
		s.Complete20 /= n
		s.NDCG10 /= n
	}
	if len(times) > 0 {
		sort.Float64s(times)
		s.P95MS = times[int(math.Ceil(.95*float64(len(times))))-1]
	}
	return s
}
