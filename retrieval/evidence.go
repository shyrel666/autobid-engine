package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
)

type Claim struct {
	Text       string   `json:"text"`
	Parameter  string   `json:"parameter"`
	Operator   string   `json:"operator"`
	Value      float64  `json:"value"`
	Unit       string   `json:"unit"`
	Product    string   `json:"product,omitempty"`
	Version    string   `json:"version,omitempty"`
	AsOf       string   `json:"as_of,omitempty"`
	Conditions []string `json:"conditions,omitempty"`
}
type Snapshot struct {
	Document        *db.KnowledgeDocument `json:"document"`
	Chunk           *db.KnowledgeChunk    `json:"chunk"`
	Citation        db.KnowledgeCitation  `json:"citation"`
	PageReliability string                `json:"page_reliability"`
}
type Verdict struct {
	Relation string `json:"relation"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
	Table    *int   `json:"table,omitempty"`
	Row      *int   `json:"row,omitempty"`
	Column   *int   `json:"column,omitempty"`
	Quote    string `json:"quote,omitempty"`
}

func insufficient(reason string) Verdict {
	return Verdict{Relation: "insufficient", Status: "unverified", Reason: reason}
}

var quantity = regexp.MustCompile(`^\s*([0-9]+(?:\.[0-9]+)?)\s*(毫秒|秒|分钟|小时|天|个月|年|ms|s|min|h|GB|MB|TB|路|人|台|核|%|万像素)\s*$`)

type unit struct {
	dimension string
	factor    float64
}

var units = map[string]unit{"毫秒": {"time", .001}, "ms": {"time", .001}, "秒": {"time", 1}, "s": {"time", 1}, "分钟": {"time", 60}, "min": {"time", 60}, "小时": {"time", 3600}, "h": {"time", 3600}, "天": {"time", 86400}, "个月": {"calendar_month", 1}, "年": {"calendar_month", 12}, "MB": {"bytes_decimal", 1e6}, "GB": {"bytes_decimal", 1e9}, "TB": {"bytes_decimal", 1e12}, "路": {"路", 1}, "人": {"人", 1}, "台": {"台", 1}, "核": {"核", 1}, "%": {"percent", 1}, "万像素": {"pixels", 10000}}

// VerifyNumeric deliberately accepts only exact scalar property values and two-column tables.
// Bounds, free-form conditions, merged/ambiguous cells and unsupported units require review.
func VerifyNumeric(s Snapshot, c Claim, now time.Time) Verdict {
	if s.Document == nil || s.Chunk == nil {
		return insufficient("source unavailable")
	}
	if c.Parameter == "" || c.Text == "" || math.IsNaN(c.Value) || math.IsInf(c.Value, 0) {
		return insufficient("structured claim required")
	}
	if len(c.Conditions) > 0 {
		return insufficient("conditional claim requires review")
	}
	for _, marker := range []string{"仅", "条件", "除外", "不适用", "不支持", "选配", "实验", "测试环境", "前提", "if ", "unless", "optional", "only "} {
		if strings.Contains(strings.ToLower(s.Chunk.Content+" "+s.Chunk.SectionPath), marker) {
			return insufficient("source contains scope restrictions; requires review")
		}
	}
	if s.Document.Category == db.KnowledgeHistoricalBid {
		return insufficient("historical promise is not current product evidence")
	}
	if s.Document.Category != db.KnowledgeProductSpec && s.Document.Category != db.KnowledgeWhitepaper {
		return insufficient("source is not product capability evidence")
	}
	if c.Product == "" {
		return insufficient("exact product required for automatic verification")
	}
	var meta map[string]any
	_ = json.Unmarshal(s.Document.Extra, &meta)
	if meta["conditions"] != nil {
		return insufficient("source applicability conditions require review")
	}
	if c.Product != "" && meta["product"] != c.Product {
		return insufficient("exact product metadata mismatch or missing")
	}
	if c.Version != "" && meta["version"] != c.Version {
		return insufficient("version mismatch or missing")
	}
	asOf := now.UTC()
	if c.AsOf != "" {
		var err error
		asOf, err = time.Parse(time.DateOnly, c.AsOf)
		if err != nil {
			return insufficient("invalid as_of date")
		}
	}
	for _, k := range []string{"valid_from", "valid_until"} {
		if value, ok := meta[k]; ok {
			v, ok := value.(string)
			if !ok {
				return insufficient("invalid validity metadata")
			}
			date, err := time.Parse(time.DateOnly, v)
			if err != nil {
				return insufficient("invalid validity date")
			}
			day := asOf.Format(time.DateOnly)
			if k == "valid_until" && day > date.Format(time.DateOnly) || k == "valid_from" && day < date.Format(time.DateOnly) {
				return insufficient("source outside validity period")
			}
		}
	}
	if c.AsOf != "" && (meta["valid_from"] == nil || meta["valid_until"] == nil) {
		return insufficient("explicit dated claim requires validity interval")
	}
	target, ok := units[c.Unit]
	if !ok {
		return insufficient("unsupported unit")
	}
	type candidate struct {
		raw             string
		table, row, col *int
	}
	values := []candidate{}
	for _, line := range strings.Split(s.Chunk.Content, "\n") {
		for _, sep := range []string{"：", ":"} {
			p := strings.SplitN(line, sep, 2)
			if len(p) == 2 && strings.TrimSpace(p[0]) == c.Parameter {
				values = append(values, candidate{raw: strings.TrimSpace(p[1])})
			}
		}
	}
	var tables []struct {
		Grid  [][]string `json:"grid"`
		Cells []struct {
			RowSpan int `json:"row_span"`
			ColSpan int `json:"col_span"`
		} `json:"cells"`
	}
	_ = json.Unmarshal(s.Chunk.Tables, &tables)
	for ti, t := range tables {
		ambiguous := false
		for _, cell := range t.Cells {
			if cell.RowSpan > 1 || cell.ColSpan > 1 {
				ambiguous = true
			}
		}
		if ambiguous {
			continue
		}
		if len(t.Grid) > 0 && len(t.Grid[0]) == 2 && strings.TrimSpace(t.Grid[0][0]) != c.Parameter {
			header := strings.TrimSpace(t.Grid[0][1])
			if header != "值" && header != "参数值" && header != "规格" && header != "指标值" && header != "Value" {
				continue
			}
		}
		for ri, row := range t.Grid {
			if len(row) == 2 && strings.TrimSpace(row[0]) == c.Parameter {
				ti, ri, ci := ti, ri, 1
				values = append(values, candidate{row[1], &ti, &ri, &ci})
			}
		}
	}
	if len(values) == 0 {
		return insufficient("no unambiguous parameter value in source")
	}
	var first *float64
	var selected candidate
	for _, v := range values {
		m := quantity.FindStringSubmatch(v.raw)
		if m == nil {
			return insufficient("non-scalar, bounded or conditional source value")
		}
		u, ok := units[m[2]]
		if !ok || u.dimension != target.dimension {
			return insufficient("incompatible units")
		}
		n, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return insufficient("invalid value")
		}
		n = n * u.factor / target.factor
		if first != nil && math.Abs(*first-n) > 1e-9 {
			return insufficient("conflicting values in source")
		}
		first = &n
		selected = v
	}
	good := false
	switch c.Operator {
	case "<=", "≤":
		good = *first <= c.Value
	case ">=", "≥":
		good = *first >= c.Value
	case "=":
		good = math.Abs(*first-c.Value) < 1e-9
	default:
		return insufficient("unsupported comparison")
	}
	relation := "contradicts"
	if good {
		relation = "supports"
	}
	return Verdict{Relation: relation, Status: "verified", Reason: "deterministic scalar comparison; scope limited to structured claim", Quote: c.Parameter + ": " + selected.raw, Table: selected.table, Row: selected.row, Column: selected.col}
}

func (s *Service) capture(ctx context.Context, t, actor string, runID int64, h db.KnowledgeHit) (db.RetrievalEvidence, error) {
	doc, c, _, err := s.DB.KnowledgeChunkWindow(ctx, t, h.Citation.DocID, h.Citation.Seq, 0, 0)
	if err != nil {
		return db.RetrievalEvidence{}, err
	}
	if c.ID != h.Chunk.ID || c.Content != h.Chunk.Content || !sameJSON(c.Tables, h.Chunk.Tables) || c.SectionPath != h.Chunk.SectionPath || c.PageStart != h.Chunk.PageStart || c.PageEnd != h.Chunk.PageEnd || doc.Title != h.Citation.DocTitle || doc.Category != h.Citation.Category || (len(h.Metadata) > 0 && !sameJSON(doc.Extra, h.Metadata)) {
		return db.RetrievalEvidence{}, db.ErrRetrievalConflict
	}
	snap := Snapshot{Document: doc, Chunk: c, Citation: c.Cite(doc), PageReliability: "parser-derived; consult document parse_meta"}
	raw, _ := json.Marshal(snap)
	e := db.RetrievalEvidence{RunID: runID, Domain: "knowledge", SourceID: c.ID, DocID: doc.ID, Hash: db.ChunkFingerprint(doc, c), Snapshot: raw, Relation: "insufficient", Status: "unverified", Reason: "retrieved candidate; claim not verified"}
	err = s.DB.SaveRetrievalEvidence(ctx, t, actor, &e)
	return e, err
}

// sameJSON compares JSON values, not bytes: a json.Marshal round trip escapes <, > and & inside RawMessage,
// which would make every real table containing them look like a changed source.
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// Evidence always rechecks the source, even if a trigger or a cache entry was missed.
func (s *Service) Evidence(ctx context.Context, t string, runID int64) ([]db.RetrievalEvidence, error) {
	list, err := s.DB.RetrievalEvidence(ctx, t, runID)
	if err != nil {
		return nil, err
	}
	run, err := s.DB.GetRetrievalRun(ctx, t, runID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		e := &list[i]
		if e.Domain != "knowledge" {
			accessible := false
			if tenant(t) == db.DefaultTenant {
				switch e.Domain {
				case "tender":
					err = s.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM document_chunks c JOIN tender_documents d ON d.id=c.doc_id WHERE c.id=$1 AND d.task_id=$2 AND d.tenant_id=$3)`, e.SourceID, run.TaskID, tenant(t)).Scan(&accessible)
				case "credential":
					err = s.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM company_credentials WHERE id=$1 AND (valid_until IS NULL OR valid_until>=current_date))`, e.SourceID).Scan(&accessible)
				}
				if err != nil {
					return nil, err
				}
			}
			if !accessible {
				e.Status = "stale"
				e.Reason = "source unavailable, out of scope or expired"
				if _, err = s.DB.ExecContext(ctx, `UPDATE retrieval_evidence SET status='stale',reason=$3 WHERE tenant_id=$1 AND id=$2`, tenant(t), e.ID, e.Reason); err != nil {
					return nil, err
				}
			}
			continue
		}
		var snap Snapshot
		if json.Unmarshal(e.Snapshot, &snap) != nil || snap.Chunk == nil {
			e.Status = "stale"
			continue
		}
		doc, c, _, err := s.DB.KnowledgeChunkWindow(ctx, t, e.DocID, snap.Chunk.Seq, 0, 0)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, err
		}
		if err != nil || c.ID != e.SourceID || db.ChunkFingerprint(doc, c) != e.Hash {
			e.Status = "stale"
			e.Reason = "source changed or deleted"
			if _, err = s.DB.ExecContext(ctx, `UPDATE retrieval_evidence SET status='stale',reason=$3 WHERE tenant_id=$1 AND id=$2`, tenant(t), e.ID, e.Reason); err != nil {
				return nil, err
			}
		}
	}
	return list, nil
}
func (s *Service) Verify(ctx context.Context, t, actor string, runID, evidenceID int64, c Claim) (db.RetrievalEvidence, error) {
	list, err := s.Evidence(ctx, t, runID)
	if err != nil {
		return db.RetrievalEvidence{}, err
	}
	for _, e := range list {
		if e.ID != evidenceID {
			continue
		}
		if e.Status == "stale" {
			return e, db.ErrRetrievalConflict
		}
		if e.Domain != "knowledge" {
			return e, fmt.Errorf("automatic verification unavailable for this source")
		}
		var snap Snapshot
		if err := json.Unmarshal(e.Snapshot, &snap); err != nil {
			return e, err
		}
		v := VerifyNumeric(snap, c, time.Now())
		claim, _ := json.Marshal(map[string]any{"input": c, "verdict": v})
		e.Claim = claim
		e.Relation = v.Relation
		e.Status = v.Status
		e.Reason = v.Reason
		// Serialize against source changes so stale evidence cannot be resurrected.
		err = s.DB.SaveRetrievalEvidence(ctx, t, actor, &e)
		return e, err
	}
	return db.RetrievalEvidence{}, db.ErrNotFound
}
