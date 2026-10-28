package retrieval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/autobid/autobid-engine/db"
)

func (c *Coordinator) searchFacet(ctx context.Context, run *db.RetrievalRun, f Facet, actor string, req ResearchRequest, degraded *[]string) ([]db.RetrievalEvidence, error) {
	out := []db.RetrievalEvidence{}
	if f.Domain == "knowledge" {
		q := req.Query
		q.TenantID = run.TenantID
		q.Text = f.Query
		q.Limit = 8
		result, err := c.Service.candidates(ctx, q)
		if err != nil {
			return nil, err
		}
		*degraded = append(*degraded, result.Degraded...)
		for _, h := range result.Hits {
			e, err := c.Service.capture(ctx, run.TenantID, actor, run.ID, h)
			if err != nil {
				return nil, err
			}
			if req.Claim != nil {
				e, err = c.Service.Verify(ctx, run.TenantID, actor, run.ID, e.ID, *req.Claim)
				if err != nil {
					return nil, err
				}
			}
			out = append(out, e)
		}
		return out, nil
	}
	// Legacy tender/credential tables are not fully multi-tenant yet. Never expose them outside default.
	if err := c.Service.reserve(ctx, 1, 0); err != nil {
		return nil, err
	}
	if run.TenantID != db.DefaultTenant {
		return nil, fmt.Errorf("legacy source unavailable outside default tenant")
	}
	match := func(s string) bool {
		lower := strings.ToLower(s)
		for _, m := range req.Query.MustTerms {
			if !strings.Contains(lower, strings.ToLower(m)) {
				return false
			}
		}
		for _, term := range db.QueryTerms(f.Query) {
			if strings.Contains(lower, strings.ToLower(term)) {
				return true
			}
		}
		return false
	}
	save := func(id, docID int64, snapshot any) error {
		b, _ := json.Marshal(snapshot)
		e := db.RetrievalEvidence{RunID: run.ID, Domain: f.Domain, SourceID: id, DocID: docID, Hash: db.ContentHash(string(b)), Snapshot: b, Relation: "insufficient", Status: "unverified", Reason: "source retrieved; requires domain review"}
		if err := c.Service.DB.SaveRetrievalEvidence(ctx, run.TenantID, actor, &e); err != nil {
			return err
		}
		out = append(out, e)
		return nil
	}
	switch f.Domain {
	case "tender":
		if run.TaskID == nil {
			return nil, fmt.Errorf("tender search requires a task")
		}
		chunks, err := c.Service.DB.TaskChunks(ctx, *run.TaskID, 0, 20000)
		if err != nil {
			return nil, err
		}
		for _, ch := range chunks {
			if match(ch.SectionTitle + " " + ch.Content) {
				if err = save(ch.ID, ch.DocID, ch); err != nil {
					return nil, err
				}
				if len(out) == 8 {
					break
				}
			}
		}
	case "credential":
		creds, err := c.Service.DB.ListCompanyCredentials(ctx, db.CredentialFilter{Terms: db.QueryTerms(f.Query), Limit: 1000})
		if err != nil {
			return nil, err
		}
		for _, cred := range creds {
			if match(cred.Title + " " + cred.Code + " " + string(cred.Attributes)) {
				if err = save(cred.ID, cred.ID, cred); err != nil {
					return nil, err
				}
				if len(out) == 8 {
					break
				}
			}
		}
	default:
		return nil, fmt.Errorf("unsupported evidence source")
	}
	return out, nil
}
