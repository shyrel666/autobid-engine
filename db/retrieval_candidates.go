package db

import (
	"context"
	"encoding/json"
	"fmt"
)

// RetrievalCandidates returns independently ranked channels. No model calls occur in SQL.
func (d *DB) RetrievalCandidates(ctx context.Context, q KnowledgeQuery, indexID int64, vector []float64) ([]KnowledgeHit, error) {
	for _, c := range q.Categories {
		if !ValidKnowledgeCategory(c) {
			return nil, fmt.Errorf("%w: category", ErrInvalidKnowledge)
		}
	}
	must := []string{}
	for _, m := range q.MustTerms {
		must = append(must, likePattern(m))
	}
	cats := q.Categories
	if cats == nil {
		cats = []string{}
	}
	filter := `c.tenant_id=$1 AND d.tenant_id=$1
 AND (cardinality($2::text[])=0 OR d.category=ANY($2::text[]))
 AND (COALESCE(cardinality($3::text[]),0)=0 OR d.tags && $3::text[])
 AND ($4::bigint=0 OR c.doc_id=$4)
 AND NOT EXISTS(SELECT 1 FROM unnest($5::text[]) m(p) WHERE NOT(d.title ILIKE m.p OR d.extra::text ILIKE m.p OR c.section_title ILIKE m.p OR c.section_path ILIKE m.p OR c.content ILIKE m.p))`
	args := []any{tenantOr(q.TenantID), cats, NormalizeTags(q.Tags), q.DocID, must}
	expr, join := "", ""
	if vector == nil {
		tsq := KnowledgeTSQuery(append(QueryTerms(q.Text), q.MustTerms...))
		if tsq == "" {
			return []KnowledgeHit{}, nil
		}
		args = append(args, tsq)
		expr = `ts_rank_cd(c.search_tsv,to_tsquery('simple',$6),1)::float8`
		filter += ` AND c.search_tsv @@ to_tsquery('simple',$6)`
	} else {
		args = append(args, vector, indexID)
		expr = `kb_cosine(e.embedding,$6::float8[])`
		join = ` JOIN knowledge_embeddings e ON e.chunk_id=c.id AND e.tenant_id=c.tenant_id AND e.index_id=$7`
	}
	args = append(args, min(max(q.Limit, 1), 100))
	tableOrder := ""
	if q.PreferTables {
		tableOrder = ",(c.table_count>0) DESC"
	}
	rows, err := d.QueryContext(ctx, `SELECT `+kchunkCols+`, `+kchunkBodyCols+`,d.title,d.category,d.file_name,to_jsonb(d.tags),d.extra,`+expr+` AS score FROM knowledge_chunks c JOIN knowledge_documents d ON d.id=c.doc_id`+join+` WHERE `+filter+fmt.Sprintf(` ORDER BY score DESC`+tableOrder+`,c.id LIMIT $%d`, len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KnowledgeHit{}
	for rows.Next() {
		var doc KnowledgeDocument
		var tags []byte
		var metadata json.RawMessage
		var score float64
		c, err := scanKnowledgeChunk(rows, true, &doc.Title, &doc.Category, &doc.FileName, &tags, &metadata, &score)
		if err != nil {
			return nil, err
		}
		doc.ID = c.DocID
		_ = json.Unmarshal(tags, &doc.Tags)
		h := KnowledgeHit{Chunk: c, Citation: c.Cite(&doc), Tags: doc.Tags, MatchedTerms: []string{}, Score: score, Metadata: metadata}
		if vector == nil {
			h.FTSRank = score
		} else {
			h.VectorScore = score
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
