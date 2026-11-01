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
		Description: "对照招标资格要求或评分项核验本企业资信库（企业资质 qualification、项目业绩 performance、人员证书 personnel、财务信誉 financial、体系认证 certificate）。" +
			"返回每条候选资信的匹配得分、满足项与差距说明，并给出总体判定结论 verdict（satisfied 满足 | partial 部分满足 | missing 缺失）。",
		Schema: obj(map[string]any{
			"requirement": str("待核验的招标资格或评分要求原文摘录或概括描述"),
			"category":    str("资信类别过滤：qualification（企业资质）| performance（项目业绩）| personnel（人员证书）| financial（财务信誉）| certificate（体系认证），不填则检索全部类别"),
			"keywords":    strs("匹配资信必须包含的关键词列表（如 [\"信息系统集成\", \"一级\"]）"),
			"attributes": map[string]any{"type": "object", "description": "资信结构化属性阈值要求：数值按 >= 比对，文本按包含关系比对，" +
				"例如 {\"level\":\"一级\",\"amount\":5000000}"},
			"min_count":      integer("要求完全满足条件的资信最低数量（如要求 3 个同类业绩则填 3，默认 1）"),
			"reference_date": str("证书有效期核验基准日期 YYYY-MM-DD（不填时优先取本任务投标截止日，否则取今天）"),
		}),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[credentialRequirement](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		refDate := time.Now().Format(time.DateOnly)
		if req.ReferenceDate != "" {
			if _, err := time.Parse(time.DateOnly, req.ReferenceDate); err != nil {
				return tool.Errorf("reference_date must be YYYY-MM-DD"), nil
			}
			refDate = req.ReferenceDate
		} else if k.pg != nil && k.taskID > 0 {
			if m, err := k.pg.RequirementManifest(ctx, k.tenant, k.taskID); err == nil && m != nil && m.BidDeadline != nil {
				refDate = m.BidDeadline.In(time.FixedZone("CST", 8*3600)).Format(time.DateOnly)
			}
		}
		ref, _ := time.Parse(time.DateOnly, refDate)
		if req.Category == "" && len(req.Keywords) == 0 {
			return tool.Errorf("category or keywords is required"), nil
		}
		creds, err := k.pg.ListCompanyCredentials(ctx, db.CredentialFilter{Category: req.Category, Terms: req.Keywords, Limit: 50,
			Tenant: k.tenant, TaskID: k.taskID})
		if err != nil {
			return tool.Result{}, err
		}
		return tool.JSON(evaluateCredentials(creds, req, ref)), nil
	})
}
