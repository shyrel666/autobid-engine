package bidtools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/tool"
)

// credentialRequirement 是一条招标资格要求的可核验表达。
type credentialRequirement struct {
	Requirement   string         `json:"requirement"`
	Category      string         `json:"category"`
	Keywords      []string       `json:"keywords"`
	Attributes    map[string]any `json:"attributes"`
	MinCount      int            `json:"min_count"`
	ReferenceDate string         `json:"reference_date"`
}

type credentialCandidate struct {
	ID         int64           `json:"id"`
	Category   string          `json:"category"`
	Code       string          `json:"code,omitempty"`
	Title      string          `json:"title"`
	ValidUntil *string         `json:"valid_until,omitempty"`
	Expired    bool            `json:"expired,omitempty"`
	Score      float64         `json:"score"`
	Satisfied  []string        `json:"satisfied"`
	Gaps       []string        `json:"gaps"`
	Attributes json.RawMessage `json:"attributes,omitempty"`
}

type credentialVerdict struct {
	Requirement   string                `json:"requirement,omitempty"`
	ReferenceDate string                `json:"reference_date"`
	Verdict       string                `json:"verdict"` // satisfied | partial | missing
	MinCount      int                   `json:"min_count"`
	FullyMatched  int                   `json:"fully_matched"`
	BestScore     float64               `json:"best_score"`
	Candidates    []credentialCandidate `json:"candidates"`
	Gaps          []string              `json:"gaps,omitempty"`
}

const (
	VerdictSatisfied = "satisfied"
	VerdictPartial   = "partial"
	VerdictMissing   = "missing"
)

// evaluateCredentials 对候选资料逐条打分：有效期、每个关键词、每个属性门槛各算一项检查，score = 通过数 / 检查数。
// 数值门槛按 “>=” 比较；字符串门槛按包含比较（不区分大小写）；数组属性任一元素满足即可。
func evaluateCredentials(creds []*db.CompanyCredential, req credentialRequirement, ref time.Time) credentialVerdict {
	if req.MinCount <= 0 {
		req.MinCount = 1
	}
	out := credentialVerdict{Requirement: req.Requirement, ReferenceDate: ref.Format(time.DateOnly), MinCount: req.MinCount, Candidates: []credentialCandidate{}}
	attrKeys := make([]string, 0, len(req.Attributes))
	for k := range req.Attributes {
		attrKeys = append(attrKeys, k)
	}
	sort.Strings(attrKeys)
	for _, c := range creds {
		cand := credentialCandidate{ID: c.ID, Category: c.Category, Code: c.Code, Title: c.Title, ValidUntil: c.ValidUntil,
			Attributes: c.Attributes, Satisfied: []string{}, Gaps: []string{}}
		var attrs map[string]any
		_ = json.Unmarshal(c.Attributes, &attrs)
		checks, passed := 0, 0
		check := func(ok bool, satisfied, gap string) {
			checks++
			if ok {
				passed++
				cand.Satisfied = append(cand.Satisfied, satisfied)
			} else {
				cand.Gaps = append(cand.Gaps, gap)
			}
		}
		valid := true
		if c.ValidUntil != nil {
			if until, err := time.Parse(time.DateOnly, *c.ValidUntil); err == nil && until.Before(ref) {
				valid = false
			}
		}
		cand.Expired = !valid
		check(valid, "在有效期内", fmt.Sprintf("已于 %s 过期", deref(c.ValidUntil)))
		hay := strings.ToLower(c.Code + " " + c.Title + " " + string(c.Attributes))
		for _, kw := range req.Keywords {
			kw = strings.TrimSpace(kw)
			if kw == "" {
				continue
			}
			check(strings.Contains(hay, strings.ToLower(kw)), "包含“"+kw+"”", "未体现“"+kw+"”")
		}
		for _, key := range attrKeys {
			ok, have := attributeSatisfies(attrs[key], req.Attributes[key])
			want := fmt.Sprint(req.Attributes[key])
			check(ok, fmt.Sprintf("%s=%s 满足要求 %s", key, have, want), fmt.Sprintf("%s=%s 不满足要求 %s", key, have, want))
		}
		cand.Score = float64(passed) / float64(checks)
		if cand.Score == 1 {
			out.FullyMatched++
		}
		out.BestScore = max(out.BestScore, cand.Score)
		out.Candidates = append(out.Candidates, cand)
	}
	sort.SliceStable(out.Candidates, func(i, j int) bool { return out.Candidates[i].Score > out.Candidates[j].Score })
	switch {
	case out.FullyMatched >= req.MinCount:
		out.Verdict = VerdictSatisfied
	case len(out.Candidates) > 0:
		out.Verdict = VerdictPartial
		if out.FullyMatched > 0 {
			out.Gaps = append(out.Gaps, fmt.Sprintf("完全满足的只有 %d 项，要求 %d 项", out.FullyMatched, req.MinCount))
		} else {
			out.Gaps = append(out.Gaps, "没有完全满足要求的资料："+strings.Join(out.Candidates[0].Gaps, "；"))
		}
	default:
		out.Verdict = VerdictMissing
		out.Gaps = append(out.Gaps, "企业资料库中没有匹配的记录")
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// attributeSatisfies 判断资料属性 have 是否满足门槛 want，并返回 have 的展示值。
func attributeSatisfies(have, want any) (bool, string) {
	if have == nil {
		return false, "缺失"
	}
	if arr, ok := have.([]any); ok {
		for _, el := range arr {
			if ok, _ := attributeSatisfies(el, want); ok {
				return true, fmt.Sprint(el)
			}
		}
		return false, fmt.Sprint(have)
	}
	shown := fmt.Sprint(have)
	switch w := want.(type) {
	case float64:
		h, ok := numeric(have)
		return ok && h >= w, shown
	case bool:
		b, ok := have.(bool)
		return ok && b == w, shown
	case string:
		if wn, err := strconv.ParseFloat(w, 64); err == nil {
			if h, ok := numeric(have); ok {
				return h >= wn, shown
			}
		}
		return strings.Contains(strings.ToLower(shown), strings.ToLower(w)), shown
	}
	return fmt.Sprint(want) == shown, shown
}

func numeric(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

func (k *Kit) queryCredentialsTool() tool.CoreTool {
	return readTool(tool.Spec{
		Name: ToolQueryCredentials,
		Description: "Check our company credential library (qualifications, past performance, personnel certificates) against one tender requirement. " +
			"Returns each candidate's match score, satisfied checks and gaps, plus an overall verdict (satisfied | partial | missing).",
		Schema: obj(map[string]any{
			"requirement": str("the tender requirement being checked, quoted or paraphrased"),
			"category":    str("qualification | performance | personnel | financial | certificate | ... (omit to search all)"),
			"keywords":    strs("terms that a matching credential must mention, e.g. [\"信息系统集成\",\"一级\"]"),
			"attributes": map[string]any{"type": "object", "description": "thresholds on credential attributes: numbers compare >=, strings by containment, " +
				"e.g. {\"level\":\"一级\",\"amount\":5000000}"},
			"min_count":      integer("how many fully matching credentials are required, default 1"),
			"reference_date": str("YYYY-MM-DD for validity checks (e.g. bid deadline), default today"),
		}),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[credentialRequirement](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		ref := time.Now()
		if req.ReferenceDate != "" {
			if ref, err = time.Parse(time.DateOnly, req.ReferenceDate); err != nil {
				return tool.Errorf("reference_date must be YYYY-MM-DD"), nil
			}
		}
		if req.Category == "" && len(req.Keywords) == 0 {
			return tool.Errorf("category or keywords is required"), nil
		}
		creds, err := k.pg.ListCompanyCredentials(ctx, db.CredentialFilter{Category: req.Category, Terms: req.Keywords, Limit: 50})
		if err != nil {
			return tool.Result{}, err
		}
		return tool.JSON(evaluateCredentials(creds, req, ref)), nil
	})
}
