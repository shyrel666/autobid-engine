// retrieval-eval executes actual PostgreSQL retrieval. It never treats synthetic scores as business acceptance.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
	ev "github.com/autobid/autobid-engine/eval/retrieval"
	"github.com/autobid/autobid-engine/retrieval"
	"github.com/autobid/autobid-engine/server"
)

type groupReport struct {
	Judgments  ev.JudgmentSummary    `json:"human_review"`
	Group      string                `json:"group"`
	Skipped    string                `json:"skipped,omitempty"`
	Summary    *ev.Summary           `json:"summary"`
	ByCategory map[string]ev.Summary `json:"by_category"`
	Rows       []ev.Observation      `json:"observations"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("dataset", "eval/retrieval/synthetic.json", "versioned dataset")
	out := flag.String("out", "", "report JSON file (default stdout)")
	groups := flag.String("groups", "A,C", "A legacy, B semantic with baseline fusion, C RRF/rerank, D bounded single, E bounded multi")
	repeat := flag.Int("repeat", 3, "repeats per case")
	split := flag.String("split", "heldout", "tune or heldout")
	tenant := flag.String("tenant", "", "existing isolated dataset tenant, required without -seed")
	seed := flag.Bool("seed", false, "seed manifest documents into a fresh tenant; clean up only that tenant afterwards")
	profile := flag.Int64("profile-id", 0, "existing LLM profile for D/E; zero skips model experiments")
	judgmentPath := flag.String("judgments", "", "optional independently reviewed judgment JSON array")
	flag.Parse()
	if *repeat < 1 || *repeat > 20 || (*split != "heldout" && *split != "tune") {
		return fmt.Errorf("invalid repeat/split")
	}
	raw, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	var data ev.Dataset
	if err = json.Unmarshal(raw, &data); err != nil {
		return err
	}
	if err = data.Validate(); err != nil {
		return err
	}
	dsn := os.Getenv("AUTOBID_TEST_DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("AUTOBID_TEST_DATABASE_URL is required; production URL is never used")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	pg, err := db.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("cannot connect to evaluation database")
	}
	defer pg.Close()
	if err = pg.Migrate(ctx); err != nil {
		return err
	}
	emb, rank, err := retrieval.ModelsFromEnv()
	if err != nil {
		return err
	}
	svc := retrieval.New(pg, emb, rank)
	keys := map[int64]string{}
	if *seed {
		if len(data.Documents) == 0 {
			return fmt.Errorf("manifest contains no documents")
		}
		*tenant = fmt.Sprintf("eval-%d", time.Now().UnixNano())
		defer func() {
			cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
			defer done()
			for _, table := range []string{"retrieval_runs", "knowledge_documents", "knowledge_indexes", "knowledge_catalogs"} {
				if _, e := pg.ExecContext(cleanup, "DELETE FROM "+table+" WHERE tenant_id=$1", *tenant); e != nil {
					fmt.Fprintln(os.Stderr, "fixture cleanup failed:", table)
				}
			}
		}()
		seen := map[string]bool{}
		for _, d := range data.Documents {
			if d.Key == "" || seen[d.Key] || d.Family == "" || len(d.Chunks) == 0 {
				return fmt.Errorf("invalid fixture document")
			}
			seen[d.Key] = true
			chunks := []db.KnowledgeChunk{}
			for i, text := range d.Chunks {
				chunks = append(chunks, db.KnowledgeChunk{Seq: i, SectionTitle: d.Title, SectionPath: d.Title, Content: text})
			}
			doc := &db.KnowledgeDocument{TenantID: *tenant, Title: d.Title, Category: d.Category, RawText: strings.Join(d.Chunks, "\n"), Extra: d.Extra, ContentHash: db.ContentHash(d.Key + strings.Join(d.Chunks, "\n"))}
			id, _, e := pg.InsertKnowledgeDocument(ctx, doc, chunks, db.DefaultEmbedder())
			if e != nil {
				return e
			}
			keys[id] = d.Key
		}
	} else if *tenant == "" || *tenant == db.DefaultTenant {
		return fmt.Errorf("specify a non-default isolated -tenant or -seed")
	}
	var indexID int64
	var baselineIndexID int64
	if emb != nil {
		if *seed {
			dim := emb.(*retrieval.HTTPModel).Dimension
			idx, e := svc.BuildIndex(ctx, *tenant, "evaluation", dim)
			if e != nil {
				return e
			}
			if e = pg.ActivateKnowledgeIndex(ctx, *tenant, idx.ID, "evaluation"); e != nil {
				return e
			}
			indexID = idx.ID
			if strings.Contains(*groups, "B") {
				baseline, e := svc.BuildBaselineIndex(ctx, *tenant, "evaluation-B", dim)
				if e != nil {
					return e
				}
				baselineIndexID = baseline.ID
			}
		} else {
			indexes, e := pg.KnowledgeIndexes(ctx, *tenant)
			if e != nil {
				return e
			}
			epoch, e := pg.KnowledgeEpoch(ctx, *tenant)
			if e != nil {
				return e
			}
			for _, idx := range indexes {
				if idx.Status == "ready" && idx.Model == emb.Model() && idx.Epoch == epoch && idx.Preprocessing == "title-path-body-v1" {
					baselineIndexID = idx.ID
				}
				if idx.Active && idx.Model == emb.Model() && idx.Epoch == epoch {
					indexID = idx.ID
				}
			}
		}
	}
	var reasoner retrieval.Reasoner
	var vectorAvailable bool
	if err = pg.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_available_extensions WHERE name='vector')`).Scan(&vectorAvailable); err != nil {
		return err
	}
	var snapshotParts []string
	var cursor int64
	for {
		chunks, e := pg.IndexChunks(ctx, *tenant, cursor, 128)
		if e != nil {
			return e
		}
		if len(chunks) == 0 {
			break
		}
		for _, chunk := range chunks {
			snapshotParts = append(snapshotParts, db.ChunkFingerprint(chunk.Document, chunk.Chunk))
			cursor = chunk.Chunk.ID
		}
	}
	if *profile > 0 {
		p, e := pg.GetLLMProfile(ctx, *profile)
		if e != nil {
			return e
		}
		provider, e := server.DefaultProviderFactory(p, nil)
		if e != nil {
			return e
		}
		reasoner = &agent.RetrievalReasoner{Planner: provider, Worker: provider}
	}
	caseMap := map[string]ev.Case{}
	var judgments []ev.Judgment
	if *judgmentPath != "" {
		b, e := os.ReadFile(*judgmentPath)
		if e != nil {
			return e
		}
		if e = json.Unmarshal(b, &judgments); e != nil {
			return e
		}
	}
	for _, c := range data.Cases {
		caseMap[c.ID] = c
	}
	reports := []groupReport{}
	for _, group := range strings.Split(*groups, ",") {
		if !strings.Contains("ABCDE", group) || len(group) != 1 {
			return fmt.Errorf("unknown group %q", group)
		}
		g := groupReport{Group: group, ByCategory: map[string]ev.Summary{}}
		if group == "B" && (emb == nil || baselineIndexID == 0) {
			g.Skipped = "semantic model/current index unavailable"
		}
		if (group == "D" || group == "E") && reasoner == nil {
			g.Skipped = "reasoner profile unavailable"
		}
		if g.Skipped != "" {
			reports = append(reports, g)
			continue
		}
		categories := map[string][]ev.Observation{}
		for _, c := range data.Cases {
			if c.Split != *split {
				continue
			}
			for n := 0; n < *repeat; n++ {
				observation := ev.Observation{CaseID: c.ID, Repeat: n, Ranked: []string{}}
				q := db.KnowledgeQuery{TenantID: *tenant, Text: c.Query, MustTerms: c.MustTerms, Limit: 20}
				start := time.Now()
				var hits []db.KnowledgeHit
				var e error
				switch group {
				case "A":
					hits, e = pg.SearchKnowledge(ctx, q, db.DefaultEmbedder())
				case "B":
					hits, e = pg.SearchKnowledgeWithIndex(ctx, q, emb, baselineIndexID)
				case "C":
					var r retrieval.Result
					r, e = svc.Search(ctx, q, 0, "evaluation")
					hits = r.Hits
					observation.RunID = r.RunID
					observation.Degraded = r.Degraded
				case "D", "E":
					coord := retrieval.Coordinator{Service: svc, Reasoner: reasoner, MultiEnabled: group == "E"}
					var r retrieval.ResearchResult
					r, e = coord.Run(ctx, *tenant, 0, "evaluation", retrieval.ResearchRequest{Key: fmt.Sprintf("%s-%s-%d-%d", group, c.ID, n, time.Now().UnixNano()), Question: c.Query, Query: q, Multi: group == "E"})
					observation.RunID = r.RunID
					for _, evidence := range r.Evidence {
						if evidence.Domain == "knowledge" {
							var snap retrieval.Snapshot
							if json.Unmarshal(evidence.Snapshot, &snap) == nil && snap.Chunk != nil {
								hits = append(hits, db.KnowledgeHit{Chunk: snap.Chunk})
							}
						}
					}
					observation.Degraded = append(r.Degraded, r.Gaps...)
				}
				observation.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
				if observation.RunID > 0 {
					run, re := pg.GetRetrievalRun(ctx, *tenant, observation.RunID)
					if re != nil {
						return re
					}
					observation.UsedCalls = run.UsedCalls
					observation.ReservedTokens = run.ReservedTokens
					observation.ActualReasonerTokens = run.ActualTokens
					observation.Status = run.Status
				}
				if e != nil {
					observation.Error = e.Error()
				}
				for _, h := range hits {
					k := keys[h.Chunk.DocID]
					if k == "" {
						k = fmt.Sprint(h.Chunk.DocID)
					}
					observation.Ranked = append(observation.Ranked, fmt.Sprintf("%s:%d", k, h.Chunk.Seq))
				}
				observation.Score = ev.Measure(c, observation.Ranked)
				g.Rows = append(g.Rows, observation)
				categories[c.Category] = append(categories[c.Category], observation)
			}
		}
		if len(g.Rows) == 0 {
			return fmt.Errorf("no cases for split %s", *split)
		}
		summary := ev.Summarize(caseMap, g.Rows)
		g.Summary = &summary
		g.Judgments, err = ev.SummarizeJudgments(caseMap, g.Rows, group, judgments)
		if err != nil {
			return err
		}
		for category, rows := range categories {
			g.ByCategory[category] = ev.Summarize(caseMap, rows)
		}
		reports = append(reports, g)
	}
	model, reranker := "unconfigured", "unconfigured"
	if emb != nil {
		model = emb.Model()
	}
	if rank != nil {
		reranker = rank.Model()
	}
	report := map[string]any{"dataset": data.Version, "dataset_sha256": db.ContentHash(string(raw)), "snapshot": data.Snapshot, "synthetic": data.Synthetic, "split": *split, "created_at": time.Now().UTC(), "embedding": model, "reranker": reranker, "strategy": retrieval.Strategy, "repeat": *repeat, "groups": reports, "business_acceptance": false, "unmeasured": []string{"human citation support", "unanswerable classification", "complete correct task rate", "cost per correct task", "mandatory false positives"}, "note": "D/E evidence union is ordered by first capture, not a comparable reranked list. Inspect human adjudications before enabling any route; synthetic fixtures are regression data only."}
	report["runtime"] = map[string]any{"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "logical_cpus": runtime.NumCPU(), "concurrency": 1, "pgvector_available": vectorAvailable, "vector_path": "exact PostgreSQL float8 cosine", "chunks": len(snapshotParts), "actual_corpus_hash": db.ContentHash(strings.Join(snapshotParts, "\n")), "preprocessing": retrieval.Preprocessing}
	report["index_ids"] = map[string]int64{"B": baselineIndexID, "C": indexID}
	// Provider-reported usage (response usage.total_tokens) for this run, index builds included; null when the provider reports none.
	used := map[string]any{"embedding": nil, "reranker": nil}
	if m, ok := emb.(*retrieval.HTTPModel); ok && m.TokensUsed() > 0 {
		used["embedding"] = m.TokensUsed()
	}
	if m, ok := rank.(*retrieval.HTTPModel); ok && m.TokensUsed() > 0 {
		used["reranker"] = m.TokensUsed()
	}
	report["provider_reported_tokens"] = used
	output, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	output = append(output, '\n')
	if *out != "" {
		return os.WriteFile(*out, output, 0600)
	}
	fmt.Print(string(output))
	return nil
}
