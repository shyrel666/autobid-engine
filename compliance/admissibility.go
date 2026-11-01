package compliance

// D2 证据可采性纯规则：输入为要求证明规则、绑定证据的结构化字段与原件校验结果、投标截止时间与
// 投标人主体信息；输出逐证据结论与要求级聚合结论。本文件不访问数据库、文件系统或网络。
// 语义判断（同类项目、范围是否覆盖）只记 manual，不引入 LLM。

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AdmissibilityRuleVersion 写入每次运行的 checks 与规则版本；规则变更须升版本并重评。
const AdmissibilityRuleVersion = "d2-rules-v1"

// 可采性结论。manual 不是通过，必须人工复核。
const (
	Admissible   = "admissible"
	Inadmissible = "inadmissible"
	Manual       = "manual"
)

// 检查状态。
const (
	CheckPass   = "pass"
	CheckFail   = "fail"
	CheckManual = "manual"
)

// AdmissibilityCheck 是一条可复核的规则结果。
type AdmissibilityCheck struct {
	Rule     string `json:"rule"`
	Status   string `json:"status"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// AdmissibilityReason 是结论原因；Kind 用于下游门禁映射（如 min_count -> objective_evidence_missing）。
type AdmissibilityReason struct {
	Code    string `json:"code"`
	Kind    string `json:"kind,omitempty"`
	Message string `json:"message"`
}

// AdmissibilityProofRules 与 db.ProofRules 的规则字段一致；db 层负责映射。
type AdmissibilityProofRules struct {
	RequiredDocs         []string          `json:"required_docs,omitempty"`
	ContractPages        []string          `json:"contract_pages,omitempty"`
	NeedAcceptance       bool              `json:"need_acceptance,omitempty"`
	DateAnchor           string            `json:"date_anchor,omitempty"`
	YearsWindow          int               `json:"years_window,omitempty"`
	SinceDate            string            `json:"since_date,omitempty"`
	ValidOnBidDeadline   bool              `json:"valid_on_bid_deadline,omitempty"`
	SocialSecurityMonths int               `json:"social_security_months,omitempty"`
	BidderOnly           bool              `json:"bidder_only,omitempty"`
	MinCount             int               `json:"min_count,omitempty"`
	ModelParams          map[string]string `json:"model_params,omitempty"`
	ScopeKeywords        []string          `json:"scope_keywords,omitempty"`
	RawSummary           string            `json:"raw_summary,omitempty"`
}

// AdmissibilityRequirement 是可采性评估所要求的最低字段。
type AdmissibilityRequirement struct {
	Code        string                    `json:"code"`
	Category    string                    `json:"category"`
	Title       string                    `json:"title"`
	Description string                    `json:"description"`
	ProofRules  AdmissibilityProofRules   `json:"proof_rules"`
}

// AdmissibilityOriginal 是一份原件的登记与文件校验结果（校验由 db / evidence 层完成）。
type AdmissibilityOriginal struct {
	DocumentKind string `json:"document_kind"`
	PageLabel    string `json:"page_label"`
	PageStart    int    `json:"page_start"`
	PageEnd      int    `json:"page_end"`
	SHA256       string `json:"original_sha256"`
	Present      bool   `json:"present"`
	Exists       bool   `json:"exists"`
	Readable     bool   `json:"readable"`
	HashMatch    bool   `json:"hash_match"`
	Superseded   bool   `json:"superseded,omitempty"`
}

// AdmissibilityEvidence 是参与可采性判定的单条企业证据（credential 或 performance）。
type AdmissibilityEvidence struct {
	Domain         string         `json:"evidence_domain"`
	ID             int64          `json:"evidence_id"`
	Category       string         `json:"category"`
	Code           string         `json:"code"`
	Title          string         `json:"title"`
	SubjectName    string         `json:"subject_name"`
	SubjectType    string         `json:"subject_type"`
	Scope          string         `json:"scope"`
	CertificateNo  string         `json:"certificate_no,omitempty"`
	Issuer         string         `json:"issuer,omitempty"`
	IssuedOn       *time.Time     `json:"issued_on,omitempty"`
	ValidUntil     *time.Time     `json:"valid_until,omitempty"`
	Attributes     map[string]any `json:"attributes,omitempty"`
	ContractDate   *time.Time     `json:"contract_date,omitempty"`
	AcceptanceDate *time.Time     `json:"acceptance_date,omitempty"`
	Amount         string         `json:"amount,omitempty"`
	IsSubsidiary   bool           `json:"is_subsidiary,omitempty"`
	Deleted        bool           `json:"deleted,omitempty"`
	Originals      []AdmissibilityOriginal     `json:"originals"`
}

// AdmissibilityInput 是一次要求级评估的完整输入。
type AdmissibilityInput struct {
	Requirement AdmissibilityRequirement `json:"requirement"`
	Evidence    []AdmissibilityEvidence  `json:"evidence"`
	BidDeadline time.Time                `json:"bid_deadline"`
	BidderName  string                   `json:"bidder_name"`
}

// AdmissibilityItemResult 是逐证据结论。
type AdmissibilityItemResult struct {
	Domain     string   `json:"evidence_domain"`
	EvidenceID int64    `json:"evidence_id"`
	Verdict    string   `json:"verdict"`
	Checks     []AdmissibilityCheck  `json:"checks"`
	Reasons    []AdmissibilityReason `json:"reasons"`
}

// AdmissibilityResult 是要求级聚合结论。MinCount 等聚合检查只出现在这里。
type AdmissibilityResult struct {
	RuleVersion string       `json:"rule_version"`
	Verdict     string       `json:"verdict"`
	Checks      []AdmissibilityCheck      `json:"checks"`
	Reasons     []AdmissibilityReason     `json:"reasons"`
	Items       []AdmissibilityItemResult `json:"items"`
}

var cstZone = time.FixedZone("CST", 8*3600)

func dateOnly(t time.Time) time.Time {
	y, m, d := t.In(cstZone).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, cstZone)
}

func datePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := dateOnly(*t)
	return &v
}

func addCheck(checks *[]AdmissibilityCheck, rule, status, expected, actual, detail string) {
	*checks = append(*checks, AdmissibilityCheck{Rule: rule, Status: status, Expected: expected, Actual: actual, Detail: detail})
}

func addReason(reasons *[]AdmissibilityReason, code, kind, msg string) {
	*reasons = append(*reasons, AdmissibilityReason{Code: code, Kind: kind, Message: msg})
}

// Evaluate 计算逐证据与要求级可采性。任意 manual 证据都使要求级结论不得为 admissible。
func Evaluate(in AdmissibilityInput) AdmissibilityResult {
	out := AdmissibilityResult{RuleVersion: AdmissibilityRuleVersion, Checks: []AdmissibilityCheck{}, Reasons: []AdmissibilityReason{}, Items: []AdmissibilityItemResult{}}
	for _, ev := range in.Evidence {
		out.Items = append(out.Items, evaluateItem(in, ev))
	}
	admissible, manual := 0, 0
	for _, it := range out.Items {
		switch it.Verdict {
		case Admissible:
			admissible++
		case Manual:
			manual++
		}
	}
	min := in.Requirement.ProofRules.MinCount
	if min <= 0 {
		min = 1
	}
	minStatus := CheckPass
	switch {
	case admissible >= min:
		minStatus = CheckPass
	case manual > 0:
		minStatus = CheckManual
	default:
		minStatus = CheckFail
	}
	addCheck(&out.Checks, "min_count", minStatus, strconv.Itoa(min), fmt.Sprintf("admissible=%d manual=%d", admissible, manual), "")
	switch {
	case len(out.Items) == 0:
		out.Verdict = Manual
		addReason(&out.Reasons, "no_evidence_bound", "no_binding", "要求尚未绑定任何企业证据，不能自动可采")
	case manual > 0:
		out.Verdict = Manual
		addReason(&out.Reasons, "manual_evidence_present", "manual", fmt.Sprintf("%d 条证据需人工复核，要求级不得自动可采", manual))
	case admissible < min:
		out.Verdict = Inadmissible
		addReason(&out.Reasons, "min_count_not_met", "min_count", fmt.Sprintf("可采证据 %d 条，少于要求 %d 条", admissible, min))
	default:
		out.Verdict = Admissible
		addReason(&out.Reasons, "evidence_admissible", "", fmt.Sprintf("%d 条证据可采", admissible))
	}
	if status := worstItemCheck(out.Items, "original_present"); status == CheckFail {
		out.Checks = append(out.Checks, AdmissibilityCheck{Rule: "original_present", Status: CheckFail, Detail: "存在原件缺失、哈希不符或记录不完整的证据"})
	} else if status == CheckManual {
		out.Checks = append(out.Checks, AdmissibilityCheck{Rule: "original_present", Status: CheckManual, Detail: "存在原件路径不可读的证据，需人工复核"})
	}
	return out
}

func worstItemCheck(items []AdmissibilityItemResult, rule string) string {
	status := ""
	for _, it := range items {
		for _, c := range it.Checks {
			if c.Rule != rule {
				continue
			}
			if c.Status == CheckFail {
				return CheckFail
			}
			if c.Status == CheckManual {
				status = CheckManual
			}
		}
	}
	return status
}

func evaluateItem(in AdmissibilityInput, ev AdmissibilityEvidence) AdmissibilityItemResult {
	it := AdmissibilityItemResult{Domain: ev.Domain, EvidenceID: ev.ID, Verdict: Admissible, Checks: []AdmissibilityCheck{}, Reasons: []AdmissibilityReason{}}
	rules := in.Requirement.ProofRules
	deadline := dateOnly(in.BidDeadline)
	if ev.Deleted {
		addCheck(&it.Checks, "evidence_available", CheckFail, "未删除", "已软删除", "证据已删除，不能作为可采证据")
	}

	// 原件：所有证据类型都要求至少一份当前有效的原件登记，且文件存在、可读、哈希一致。
	originals := currentOriginals(ev.Originals)
	if len(ev.Originals) == 0 {
		addCheck(&it.Checks, "original_present", CheckFail, ">=1 份原件", "0", "缺少原件与页码记录")
	} else if len(originals) == 0 {
		addCheck(&it.Checks, "original_present", CheckFail, ">=1 份当前有效原件", "0", "原件均已标记为被替换")
	} else {
		status, detail := CheckPass, ""
		for _, o := range originals {
			switch {
			case !o.Present || strings.TrimSpace(o.SHA256) == "":
				status, detail = CheckFail, "原件记录缺少路径或 SHA-256"
			case !o.Exists:
				status, detail = CheckFail, "原件文件不存在"
			case !o.Readable:
				if status != CheckFail {
					status, detail = CheckManual, "原件路径不可读，需人工复核"
				}
			case !o.HashMatch:
				status, detail = CheckFail, "原件内容哈希与登记值不一致"
			}
			if status == CheckFail {
				break
			}
		}
		addCheck(&it.Checks, "original_present", status, "存在、可读且哈希一致", fmt.Sprintf("%d 份", len(originals)), detail)
	}

	// 有效期：资质 / 证书 / 人员类证据在投标截止日（含当日）仍有效。
	if rules.ValidOnBidDeadline || ev.ValidUntil != nil || isExpiringCategory(ev) {
		if ev.ValidUntil == nil {
			addCheck(&it.Checks, "valid_on_bid_deadline", CheckManual, deadline.Format(time.DateOnly), "未登记有效期", "缺少有效期，不能判定截止日有效")
		} else if dateOnly(*ev.ValidUntil).Before(deadline) {
			addCheck(&it.Checks, "valid_on_bid_deadline", CheckFail, deadline.Format(time.DateOnly), dateOnly(*ev.ValidUntil).Format(time.DateOnly), "证据在投标截止日前已过期")
		} else {
			addCheck(&it.Checks, "valid_on_bid_deadline", CheckPass, deadline.Format(time.DateOnly), dateOnly(*ev.ValidUntil).Format(time.DateOnly), "")
		}
	}

	// 主体：必须是投标人自身。
	if rules.BidderOnly {
		name, typ := strings.TrimSpace(ev.SubjectName), strings.TrimSpace(ev.SubjectType)
		switch {
		case name == "" || typ == "" || typ == "unknown":
			addCheck(&it.Checks, "bidder_only", CheckManual, in.BidderName, "主体未登记", "缺少主体名称或主体类型，需人工确认")
			addCheck(&it.Checks, "subject_relation", CheckManual, "投标人自身", name, "主体关系无法自动确认")
		case typ != "bidder" || (in.BidderName != "" && !sameName(name, in.BidderName)):
			addCheck(&it.Checks, "bidder_only", CheckFail, in.BidderName, name+"("+typ+")", "业绩/资质主体不是投标人自身（母公司、子公司或个人不可用）")
		default:
			addCheck(&it.Checks, "bidder_only", CheckPass, in.BidderName, name, "")
			addCheck(&it.Checks, "subject_relation", CheckPass, "投标人自身", name, "")
		}
	}

	// 日期窗口：锚点取验收日（默认）或签订日，终点为顺延后的投标截止日（CST）。
	if rules.YearsWindow > 0 || rules.SinceDate != "" {
		anchor := ev.AcceptanceDate
		if rules.DateAnchor == "signing_date" {
			anchor = ev.ContractDate
		}
		start := deadline.AddDate(-rules.YearsWindow, 0, 0)
		if rules.SinceDate != "" {
			if d, err := time.Parse(time.DateOnly, rules.SinceDate); err == nil {
				start = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, cstZone)
			}
		}
		switch {
		case anchor == nil:
			addCheck(&it.Checks, "date_window", CheckManual, start.Format(time.DateOnly)+"~"+deadline.Format(time.DateOnly), "日期缺失", "缺少合同 / 验收日期")
		default:
			a := dateOnly(*anchor)
			if a.Before(start) || a.After(deadline) {
				addCheck(&it.Checks, "date_window", CheckFail, start.Format(time.DateOnly)+"~"+deadline.Format(time.DateOnly), a.Format(time.DateOnly), "证据日期不在要求窗口内")
			} else {
				addCheck(&it.Checks, "date_window", CheckPass, start.Format(time.DateOnly)+"~"+deadline.Format(time.DateOnly), a.Format(time.DateOnly), "")
			}
		}
	}

	// 验收：必须有验收日期与原件的验收页。
	if rules.NeedAcceptance {
		acceptanceOriginal := false
		for _, o := range originals {
			if o.DocumentKind == "acceptance_report" || strings.Contains(o.PageLabel, "验收") {
				acceptanceOriginal = true
			}
		}
		switch {
		case ev.AcceptanceDate == nil || !acceptanceOriginal:
			addCheck(&it.Checks, "need_acceptance", CheckFail, "验收日期+验收页", fmt.Sprintf("date=%v original=%v", ev.AcceptanceDate != nil, acceptanceOriginal), "缺少验收证明")
		default:
			addCheck(&it.Checks, "need_acceptance", CheckPass, "验收日期+验收页", "ok", "")
		}
	}

	// 合同关键页：每个指定页角色都必须匹配当前有效原件的页角色。
	if len(rules.ContractPages) > 0 {
		var missing []string
		for _, role := range rules.ContractPages {
			if !hasPageRole(originals, role) {
				missing = append(missing, role)
			}
		}
		if len(missing) > 0 {
			addCheck(&it.Checks, "contract_pages", CheckFail, strings.Join(rules.ContractPages, ","), "missing="+strings.Join(missing, ","), "合同页角色缺失或未登记页码")
		} else {
			addCheck(&it.Checks, "contract_pages", CheckPass, strings.Join(rules.ContractPages, ","), "ok", "")
		}
	}

	// 必需材料：合同、验收、社保、证书、产品彩页等每一种都要有对应原件或页。
	if len(rules.RequiredDocs) > 0 {
		var missing []string
		for _, doc := range rules.RequiredDocs {
			if !hasDoc(originals, doc) {
				missing = append(missing, doc)
			}
		}
		if len(missing) > 0 {
			addCheck(&it.Checks, "required_docs", CheckFail, strings.Join(rules.RequiredDocs, "+"), "missing="+strings.Join(missing, "+"), "缺少必需材料原件")
		} else {
			addCheck(&it.Checks, "required_docs", CheckPass, strings.Join(rules.RequiredDocs, "+"), "ok", "")
		}
	}

	// 社保月数。
	if rules.SocialSecurityMonths > 0 {
		months, ok := numericAttr(ev.Attributes, "social_security_months")
		switch {
		case !ok:
			addCheck(&it.Checks, "social_security_months", CheckManual, strconv.Itoa(rules.SocialSecurityMonths), "未提供", "缺少社保月数")
		case int(months) < rules.SocialSecurityMonths:
			addCheck(&it.Checks, "social_security_months", CheckFail, strconv.Itoa(rules.SocialSecurityMonths), strconv.Itoa(int(months)), "社保月数不足")
		default:
			addCheck(&it.Checks, "social_security_months", CheckPass, strconv.Itoa(rules.SocialSecurityMonths), strconv.Itoa(int(months)), "")
		}
	}

	// 型号 / 参数：数值按 >= 比较，文本按明确包含；语义不清记 manual。
	if len(rules.ModelParams) > 0 {
		status, detail, actual := modelSatisfies(ev.Attributes, rules.ModelParams)
		addCheck(&it.Checks, "model_param", status, fmt.Sprint(rules.ModelParams), actual, detail)
	}

	// 适用范围：结构化字段明确不覆盖则不可采；为空或需语义判断记 manual。
	if len(rules.ScopeKeywords) > 0 {
		if strings.TrimSpace(ev.Scope) == "" {
			addCheck(&it.Checks, "scope_match", CheckManual, strings.Join(rules.ScopeKeywords, ","), "范围未登记", "认证 / 合同范围缺失，需人工复核")
		} else {
			hit := false
			for _, kw := range rules.ScopeKeywords {
				if kw != "" && strings.Contains(ev.Scope, kw) {
					hit = true
					break
				}
			}
			if hit {
				addCheck(&it.Checks, "scope_match", CheckPass, strings.Join(rules.ScopeKeywords, ","), ev.Scope, "")
			} else {
				addCheck(&it.Checks, "scope_match", CheckFail, strings.Join(rules.ScopeKeywords, ","), ev.Scope, "结构化范围未覆盖要求")
			}
		}
	} else if semanticScope(in.Requirement) {
		addCheck(&it.Checks, "scope_match", CheckManual, "范围覆盖 / 同类性", "需人工判断", "语义范围判断只记 manual，D2 不引入 LLM")
	}

	for _, c := range it.Checks {
		switch c.Status {
		case CheckFail:
			it.Verdict = Inadmissible
			addReason(&it.Reasons, c.Rule, c.Rule, c.Detail)
		case CheckManual:
			if it.Verdict != Inadmissible {
				it.Verdict = Manual
			}
			addReason(&it.Reasons, c.Rule, "manual", c.Detail)
		}
	}
	if it.Verdict == Admissible {
		addReason(&it.Reasons, "all_checks_passed", "", "单证据规则全部通过")
	}
	return it
}

func semanticScope(req AdmissibilityRequirement) bool {
	text := req.Title + " " + req.Description
	for _, kw := range []string{"同类", "类似", "适用范围", "认证范围", "项目范围"} {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

func isExpiringCategory(ev AdmissibilityEvidence) bool {
	switch ev.Category {
	case "qualification", "certificate", "personnel", "product":
		return true
	}
	return false
}

func currentOriginals(in []AdmissibilityOriginal) []AdmissibilityOriginal {
	out := make([]AdmissibilityOriginal, 0, len(in))
	for _, o := range in {
		if !o.Superseded {
			out = append(out, o)
		}
	}
	return out
}

func sameName(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	norm := func(s string) string {
		s = strings.ReplaceAll(s, "（", "(")
		s = strings.ReplaceAll(s, "）", ")")
		s = strings.ReplaceAll(s, " ", "")
		return s
	}
	return norm(a) == norm(b)
}

func hasPageRole(originals []AdmissibilityOriginal, role string) bool {
	role = strings.TrimSpace(role)
	if role == "" {
		return true
	}
	for _, o := range originals {
		if strings.Contains(o.PageLabel, role) || (role == "关键页" && o.PageLabel != "") {
			return true
		}
	}
	return false
}

func hasDoc(originals []AdmissibilityOriginal, doc string) bool {
	doc = strings.TrimSpace(doc)
	switch {
	case strings.Contains(doc, "合同"):
		return anyOriginal(originals, func(o AdmissibilityOriginal) bool {
			return o.DocumentKind == "contract" || strings.Contains(o.PageLabel, "合同")
		})
	case strings.Contains(doc, "验收"):
		return anyOriginal(originals, func(o AdmissibilityOriginal) bool {
			return o.DocumentKind == "acceptance_report" || strings.Contains(o.PageLabel, "验收")
		})
	case strings.Contains(doc, "社保"):
		return anyOriginal(originals, func(o AdmissibilityOriginal) bool {
			return o.DocumentKind == "social_security" || strings.Contains(o.PageLabel, "社保")
		})
	case strings.Contains(doc, "证书"), strings.Contains(doc, "资质"):
		return anyOriginal(originals, func(o AdmissibilityOriginal) bool {
			return o.DocumentKind == "certificate" || strings.Contains(o.PageLabel, "证书")
		})
	case strings.Contains(doc, "彩页"), strings.Contains(doc, "产品"), strings.Contains(doc, "型号"):
		return anyOriginal(originals, func(o AdmissibilityOriginal) bool {
			return o.DocumentKind == "product_catalog" || strings.Contains(o.PageLabel, "产品")
		})
	}
	return anyOriginal(originals, func(o AdmissibilityOriginal) bool {
		return strings.Contains(o.PageLabel, doc) || strings.Contains(o.DocumentKind, doc)
	})
}

func anyOriginal(originals []AdmissibilityOriginal, fn func(AdmissibilityOriginal) bool) bool {
	for _, o := range originals {
		if fn(o) {
			return true
		}
	}
	return false
}

func numericAttr(attrs map[string]any, key string) (float64, bool) {
	if attrs == nil {
		return 0, false
	}
	switch v := attrs[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	}
	return 0, false
}

func modelSatisfies(attrs map[string]any, want map[string]string) (status, detail, actual string) {
	if attrs == nil {
		return CheckManual, "证据未提供型号 / 参数", "未提供"
	}
	params := map[string]any{}
	if raw, ok := attrs["parameters"].(map[string]any); ok {
		params = raw
	}
	var bad []string
	var actualParts []string
	for k, v := range want {
		var have any
		switch k {
		case "model":
			have = attrs["model"]
		default:
			have = params[k]
			if have == nil {
				have = attrs[k]
			}
		}
		if have == nil {
			return CheckManual, "证据缺少参数 " + k, "未提供 " + k
		}
		actualParts = append(actualParts, fmt.Sprintf("%s=%v", k, have))
		if wf, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			hf, ok := toFloat(have)
			if !ok || hf < wf {
				bad = append(bad, fmt.Sprintf("%s=%v < %s", k, have, v))
			}
			continue
		}
		if !strings.Contains(strings.ToLower(fmt.Sprint(have)), strings.ToLower(v)) {
			bad = append(bad, fmt.Sprintf("%s=%v 不含 %s", k, have, v))
		}
	}
	if len(bad) > 0 {
		return CheckFail, strings.Join(bad, "; "), strings.Join(actualParts, ",")
	}
	return CheckPass, "", strings.Join(actualParts, ",")
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

// ObjectiveResult 是客观分计算结论。Status=manual 表示规则无法解析，不得当作已成功核验的 0 分。
type ObjectiveResult struct {
	Status string   `json:"status"`
	Score  float64  `json:"score"`
	Max    float64  `json:"max_score"`
	Gaps   []string `json:"gaps"`
}

var (
	rePerUnitScore = regexp.MustCompile(`每(?:提供|个|项|名|人|份|套|家)?[^，。；;\n]{0,24}?得\s*([0-9]+(?:\.[0-9]+)?)\s*分`)
	reMaxScore     = regexp.MustCompile(`(?:最多(?:得|计)?|最高(?:得|计)?)\s*([0-9]+(?:\.[0-9]+)?)\s*分`)
)

// ComputeObjective 按“每提供 N 个得 X 分、最多 Y 分”计算客观分；无法解析时返回 manual 与缺口。
func ComputeObjective(criteria string, scoreMax float64, minCount, admissibleCount int) ObjectiveResult {
	out := ObjectiveResult{Status: "computed", Max: scoreMax, Gaps: []string{}}
	m := rePerUnitScore.FindStringSubmatch(criteria)
	if m == nil {
		out.Status = "manual"
		out.Gaps = append(out.Gaps, "score_rule_unparsed")
		return out
	}
	unit, err := strconv.ParseFloat(m[1], 64)
	if err != nil || unit <= 0 {
		out.Status = "manual"
		out.Gaps = append(out.Gaps, "score_rule_unparsed")
		return out
	}
	maxScore := scoreMax
	if mm := reMaxScore.FindStringSubmatch(criteria); mm != nil {
		if v, err := strconv.ParseFloat(mm[1], 64); err == nil && v > 0 {
			maxScore = math.Min(v, scoreMax)
		}
	}
	out.Max = maxScore
	out.Score = math.Min(float64(admissibleCount)*unit, maxScore)
	if minCount > 0 && admissibleCount < minCount {
		out.Gaps = append(out.Gaps, "evidence_missing")
	}
	return out
}
