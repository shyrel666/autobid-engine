package compliance

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// 核验结论（compliance_matrix.verify_status / deviation）。
const (
	StatusPending  = "pending"
	StatusVerified = "verified"
	StatusFailed   = "failed"
	StatusStale    = "stale"

	DeviationNone     = "none"
	DeviationPositive = "positive"
	DeviationNegative = "negative"

	VerifierRule = "rule"
	VerifierLLM  = "llm"
)

// 要求类型（覆盖率汇总按此区分 ★ / ▲ / 评分项 / 格式项）。
const (
	KindStar     = "star"
	KindTriangle = "triangle"
	KindScoring  = "scoring"
	KindFormat   = "format"
	KindRequired = "required"
)

// RequirementKind 依据类别与 ★/▲ 标记归类；无标记的强制性条款按 ★ 处理。
func RequirementKind(category, title, description string, mandatory bool) string {
	text := title + description
	switch {
	case strings.Contains(text, "★"):
		return KindStar
	case strings.Contains(text, "▲"):
		return KindTriangle
	case category == "mandatory_clause" || mandatory:
		return KindStar
	case category == "scoring_item":
		return KindScoring
	case category == "format_attachment":
		return KindFormat
	}
	return KindRequired
}

// Requirement / Section / Revision / Evidence 是核验输入（db 层在锁定修订后组装）。
type Requirement struct {
	ID          int64   `json:"id"`
	Code        string  `json:"code"`
	Category    string  `json:"category"`
	Kind        string  `json:"kind"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	IsMandatory bool    `json:"is_mandatory"`
	ScoreWeight float64 `json:"score_weight"`
}

type Section struct {
	ID    int64  `json:"id"`
	Code  string `json:"section_code"`
	Title string `json:"title"`
}

type Revision struct {
	ID        int64  `json:"id"`
	RevNo     int    `json:"rev_no"`
	ContentMD string `json:"content_md"`
}

// Evidence 是证据实时状态（直接取自 Phase 4.3 retrieval_evidence）。
type Evidence struct {
	ID       int64  `json:"id"`
	Status   string `json:"status"`
	Relation string `json:"relation"`
	Reason   string `json:"reason"`
	Missing  bool   `json:"missing,omitempty"`
}

type Input struct {
	Requirement Requirement
	Section     Section
	Revision    Revision
	Evidence    []Evidence
}

// Check 是一项规则检查的结果，写入 compliance_matrix.checks 供人工复核。
type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// Verdict 是一条要求在一个章节修订上的核验结论。
type Verdict struct {
	Status          string  `json:"verify_status"`
	Deviation       string  `json:"deviation"`
	Anchor          string  `json:"anchor"`
	AnchorExcerpt   string  `json:"anchor_excerpt"`
	ResponseSummary string  `json:"response_summary"`
	Reason          string  `json:"failure_reason"`
	Checks          []Check `json:"checks"`
	Verifier        string  `json:"verifier"`
}

// Verifier 是可替换的正文实质响应核验器。实现必须是有界的（db 层在持有章节共享锁的事务内调用）。
type Verifier interface {
	Name() string
	Verify(ctx context.Context, in Input) (Verdict, error)
}

// ---------------------------------------------------------------------------
// 段落与锚点
// ---------------------------------------------------------------------------

// Paragraph 是正文中的一个段落；Anchor 为「p<序号>-<内容哈希前 8 位>」，正文变化即锚点变化。
type Paragraph struct {
	Index   int
	Anchor  string
	Text    string
	Heading bool
}

var headingRe = regexp.MustCompile(`^\s{0,3}#{1,6}\s+`)

// Paragraphs 按空行与 Markdown 标题切分段落；列表 / 表格行按行独立成段。
func Paragraphs(md string) []Paragraph {
	var out []Paragraph
	var buf []string
	flush := func(heading bool) {
		text := strings.TrimSpace(strings.Join(buf, "\n"))
		buf = buf[:0]
		if text == "" {
			return
		}
		sum := sha1.Sum([]byte(text))
		n := len(out) + 1
		out = append(out, Paragraph{Index: n, Anchor: fmt.Sprintf("p%d-%s", n, hex.EncodeToString(sum[:4])), Text: text, Heading: heading})
	}
	for _, line := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "":
			flush(false)
		case headingRe.MatchString(line):
			flush(false)
			buf = append(buf, headingRe.ReplaceAllString(line, ""))
			flush(true)
		case strings.HasPrefix(t, "|") || strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || listNumRe.MatchString(t):
			flush(false)
			buf = append(buf, t)
			flush(false)
		default:
			buf = append(buf, t)
		}
	}
	flush(false)
	return out
}

var listNumRe = regexp.MustCompile(`^\d+[.、)）]\s*\S`)

// ---------------------------------------------------------------------------
// 文本归一与关键词
// ---------------------------------------------------------------------------

// squash 去掉空白、标点与 ★▲ 标记，用于「复述 / 只挂标题」比对。
func squash(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) || r == '★' || r == '▲' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// 去掉否定后果与通用措辞，避免关键词覆盖率被「投标人须提供……否则投标无效」这类套话拉高。
var (
	consequenceRe = regexp.MustCompile(`[，,；;]?\s*(否则|不满足|未提供|未按).{0,30}?(无效|废标|否决|不得分|扣分)[^，,；;。]*`)
	stopTerms     = []string{"投标人", "供应商", "招标人", "采购人", "本项目", "须", "必须", "应当", "应", "需", "要求", "提供", "具备", "具有",
		"不少于", "不低于", "不超过", "不大于", "至少", "以上", "以内", "以下", "的", "和", "与", "及", "或", "等", "并", "在", "对", "按", "为", "是",
		"进行", "相关", "有关", "其他", "包括", "满足", "符合", "以及", "并且", "投标文件", "招标文件"}
	fillerRe = regexp.MustCompile(`详见[^，。；]*|参见[^，。；]*|见附件[^，。；]*|见本章[^，。；]*|如下[：:]?|同上|略|待补充|此处省略`)
)

// keyTerms 返回要求文本的中文二元组与英数词元（去重）。
func keyTerms(req Requirement) []string {
	text := consequenceRe.ReplaceAllString(req.Title+"。"+req.Description, "")
	text = quantityRe.ReplaceAllString(text, " ")
	for _, w := range stopTerms {
		text = strings.ReplaceAll(text, w, " ")
	}
	seen := map[string]bool{}
	var out []string
	add := func(t string) {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	var han []rune
	flushHan := func() {
		for i := 0; i+1 < len(han); i++ {
			add(string(han[i : i+2]))
		}
		han = han[:0]
	}
	var word []rune
	flushWord := func() {
		if len(word) >= 2 {
			add(strings.ToLower(string(word)))
		}
		word = word[:0]
	}
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
			flushWord()
			han = append(han, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushHan()
			word = append(word, r)
		default:
			flushHan()
			flushWord()
		}
	}
	flushHan()
	flushWord()
	return out
}

func coverage(terms []string, text string) (float64, []string) {
	if len(terms) == 0 {
		return 1, nil
	}
	lower := strings.ToLower(text)
	var missing []string
	hit := 0
	for _, t := range terms {
		if strings.Contains(lower, t) {
			hit++
		} else {
			missing = append(missing, t)
		}
	}
	return float64(hit) / float64(len(terms)), missing
}

var (
	responseCueRe = regexp.MustCompile(`承诺|响应|满足|符合|保证|确保|提供|具备|具有|已取得|已获得|持有|配置|部署|采用|选用|实现|支持|达到|优于|按照|严格|落实|负责|组建|安排|投入|签署|编制|装订|盖章|签字|我方|我公司|本公司|我单位|将`)
	negativeCueRe = regexp.MustCompile(`不满足|无法满足|不能满足|不能提供|无法提供|暂不提供|暂无|不响应|负偏离|不具备|未取得|尚未取得|不符合`)
	positiveCueRe = regexp.MustCompile(`正偏离|优于|超出招标|高于招标|超过招标要求`)
	clauseNoRe    = regexp.MustCompile(`\d+(?:\.\d+)+`)
)

func hanAlnumCount(s string) int {
	n := 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
		}
	}
	return n
}

func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

func firstSentence(s string, n int) string {
	if i := strings.IndexAny(s, "。；;\n"); i > 0 {
		s = s[:i]
	}
	return truncateRunes(s, n)
}

// ---------------------------------------------------------------------------
// RuleVerifier
// ---------------------------------------------------------------------------

// RuleVerifier 是确定性规则核验：在章节当前修订正文中寻找对该要求的实质性应答段落。
// verified 要同时满足：段落去掉章节名 / 要求标题 / 原文复述 / 「详见」类引用后仍有实质内容；关键词覆盖率达标或命中条款号；
// 有明确响应表述或数值应答；要求中的数值门槛全部有同量纲应答且满足；无否定 / 负偏离表述；引用证据未失效、无矛盾。
// 只挂章节名、只有 citation、只复述要求，都不会被判为 verified。
type RuleVerifier struct {
	MinCoverage  float64 // 关键词二元组覆盖率门槛，默认 0.45
	MinSubstance int     // 去掉标题 / 复述 / 引用后的最少有效字符，默认 4
}

func (RuleVerifier) Name() string { return VerifierRule }

type paraEval struct {
	p         Paragraph
	cov       float64
	missing   []string
	remainder int
	cue       bool
	neg       bool
	pos       bool
	clauseHit bool
	numOK     int
	numBad    []string
	numBetter bool
	score     float64
}

func (v RuleVerifier) Verify(_ context.Context, in Input) (Verdict, error) {
	minCov, minSub := v.MinCoverage, v.MinSubstance
	if minCov <= 0 {
		minCov = 0.45
	}
	if minSub <= 0 {
		minSub = 4
	}
	out := Verdict{Status: StatusFailed, Deviation: DeviationNone, Verifier: VerifierRule, Checks: []Check{}}
	req := in.Requirement
	if strings.TrimSpace(in.Revision.ContentMD) == "" || in.Revision.RevNo == 0 {
		out.Reason = "章节尚无有效修订正文"
		out.Checks = append(out.Checks, Check{Name: "has_text", Detail: out.Reason})
		return out, nil
	}
	terms := keyTerms(req)
	var wants []Quantity
	for _, q := range Quantities(consequenceRe.ReplaceAllString(req.Title+"。"+req.Description, "")) {
		if q.Cmp != CmpEQ || q.Dim != DimItems {
			wants = append(wants, q)
		}
	}
	clause := ""
	if m := clauseNoRe.FindString(req.Code); m != "" {
		clause = m
	}
	strip := []string{squash(req.Title), squash(req.Description), squash(in.Section.Title), squash(req.Code), squash(in.Section.Code)}

	var best *paraEval
	for _, p := range Paragraphs(in.Revision.ContentMD) {
		if p.Heading {
			continue
		}
		e := paraEval{p: p}
		body := fillerRe.ReplaceAllString(p.Text, "")
		rem := squash(body)
		for _, s := range strip {
			if len([]rune(s)) >= 2 {
				rem = strings.ReplaceAll(rem, s, "")
			}
		}
		e.remainder = hanAlnumCount(rem)
		e.cov, e.missing = coverage(terms, body)
		e.cue = responseCueRe.MatchString(rem)
		e.neg = negativeCueRe.MatchString(body)
		e.pos = positiveCueRe.MatchString(body)
		e.clauseHit = clause != "" && strings.Contains(body, clause)
		gots := Quantities(body)
		for _, w := range wants {
			// 同量纲优先匹配数值相同的应答，避免日期等复合数值被第一个分量误配。
			var chosen *Quantity
			for i := range gots {
				if gots[i].Dim != w.Dim {
					continue
				}
				if chosen == nil {
					chosen = &gots[i]
				}
				if Close(gots[i].Value, w.Value) {
					chosen = &gots[i]
					break
				}
			}
			if chosen == nil {
				continue
			}
			if ok, better := Satisfies(w, *chosen); ok {
				e.numOK++
				e.numBetter = e.numBetter || better
			} else {
				e.numBad = append(e.numBad, fmt.Sprintf("要求 %s，应答 %s", w.Raw, chosen.Raw))
			}
		}
		e.score = e.cov
		if e.cue {
			e.score += 0.2
		}
		if e.clauseHit {
			e.score += 0.2
		}
		if len(wants) > 0 {
			e.score += 0.3 * float64(e.numOK) / float64(len(wants))
		}
		if e.remainder < minSub {
			e.score -= 1
		}
		if best == nil || e.score > best.score {
			cp := e
			best = &cp
		}
	}
	if best == nil {
		out.Reason = "正文只有标题，没有任何正文段落"
		out.Checks = append(out.Checks, Check{Name: "substance", Detail: out.Reason})
		return out, nil
	}
	out.Anchor, out.AnchorExcerpt = best.p.Anchor, truncateRunes(best.p.Text, 200)
	out.ResponseSummary = firstSentence(best.p.Text, 120)
	add := func(name string, ok bool, detail string) {
		out.Checks = append(out.Checks, Check{Name: name, Passed: ok, Detail: detail})
	}

	subOK := best.remainder >= minSub
	add("substance", subOK, fmt.Sprintf("去掉章节名 / 要求标题 / 原文复述 / 引用后剩余 %d 个有效字符", best.remainder))
	covOK := best.cov >= minCov || best.clauseHit
	detail := fmt.Sprintf("关键词覆盖 %.0f%%（门槛 %.0f%%）", best.cov*100, minCov*100)
	if len(best.missing) > 0 {
		detail += "，未覆盖：" + strings.Join(head(best.missing, 8), "、")
	}
	add("keywords", covOK, detail)
	if clause != "" {
		add("clause_no", best.clauseHit, "条款号 "+clause)
	}
	cueOK := best.cue || (len(wants) > 0 && best.numOK == len(wants))
	add("response_phrase", cueOK, "明确的响应 / 承诺表述")
	numOK := best.numOK == len(wants)
	if len(wants) > 0 {
		d := fmt.Sprintf("数值门槛 %d 项，已满足 %d 项", len(wants), best.numOK)
		if len(best.numBad) > 0 {
			d += "；不满足：" + strings.Join(best.numBad, "；")
		}
		add("numeric", numOK, d)
	}
	add("no_negation", !best.neg, "无「不满足 / 负偏离 / 不具备」等否定表述")

	evOK, evDetail := true, []string{}
	for _, e := range in.Evidence {
		switch {
		case e.Missing:
			evOK = false
			evDetail = append(evDetail, fmt.Sprintf("证据 %d 不存在或不可见", e.ID))
		case e.Status == "stale":
			evOK = false
			evDetail = append(evDetail, fmt.Sprintf("证据 %d 已失效：%s", e.ID, e.Reason))
		case e.Relation == "contradicts":
			evOK = false
			evDetail = append(evDetail, fmt.Sprintf("证据 %d 与承诺矛盾：%s", e.ID, e.Reason))
		case e.Status != "verified" || e.Relation != "supports":
			evDetail = append(evDetail, fmt.Sprintf("证据 %d 未核验（%s / %s），不作为已证实依据", e.ID, e.Status, e.Relation))
		}
	}
	if len(in.Evidence) > 0 {
		add("evidence", evOK, strings.Join(evDetail, "；"))
	}

	switch {
	case !subOK:
		out.Reason = "正文仅有章节名、要求复述或「详见」类引用，没有实质应答"
	case len(best.numBad) > 0 && best.cov >= minCov/2: // 同量纲数值明确不满足门槛：即使关键词覆盖一般也判负偏离
		out.Reason, out.Deviation = "数值应答不满足要求："+strings.Join(best.numBad, "；"), DeviationNegative
	case !covOK:
		out.Reason = "正文未覆盖该要求的关键内容"
	case len(best.numBad) > 0:
		out.Reason, out.Deviation = "数值应答不满足要求："+strings.Join(best.numBad, "；"), DeviationNegative
	case best.neg:
		out.Reason, out.Deviation = "正文存在不满足 / 负偏离表述", DeviationNegative
	case !numOK:
		out.Reason = "要求含数值门槛，正文未给出同量纲的数值应答"
	case !cueOK:
		out.Reason = "未出现明确的响应 / 承诺表述"
	case !evOK:
		out.Reason = "引用证据失效或矛盾：" + strings.Join(evDetail, "；")
	default:
		out.Status = StatusVerified
		if best.pos || best.numBetter {
			out.Deviation = DeviationPositive
		}
	}
	return out, nil
}

func head(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---------------------------------------------------------------------------
// LLMVerifier（可选实现：规则先行，LLM 只能把规则通过的结论降级，不能把规则失败的结论升级）
// ---------------------------------------------------------------------------

// Judgement 是 LLM 对一个规则已通过段落的复核结论。
type Judgement struct {
	Substantive bool
	Deviation   string
	Reason      string
}

// Judge 是 LLM 复核接口；未实现生产版本（测试使用替身），真实模型上的核验质量未评估。
type Judge interface {
	Judge(ctx context.Context, in Input, rule Verdict) (Judgement, error)
}

type LLMVerifier struct {
	Rule  RuleVerifier
	Judge Judge
}

func (LLMVerifier) Name() string { return VerifierLLM }

func (v LLMVerifier) Verify(ctx context.Context, in Input) (Verdict, error) {
	rule, err := v.Rule.Verify(ctx, in)
	if err != nil || rule.Status != StatusVerified || v.Judge == nil {
		return rule, err
	}
	j, err := v.Judge.Judge(ctx, in, rule)
	if err != nil {
		return Verdict{}, err
	}
	out := rule
	out.Verifier = VerifierLLM
	out.Checks = append(append([]Check{}, rule.Checks...), Check{Name: "llm_judge", Passed: j.Substantive, Detail: j.Reason})
	if !j.Substantive {
		out.Status, out.Reason = StatusFailed, "LLM 复核判定非实质应答："+j.Reason
	}
	if j.Deviation == DeviationNegative {
		out.Status, out.Deviation = StatusFailed, DeviationNegative
		if out.Reason == "" {
			out.Reason = "LLM 复核判定负偏离：" + j.Reason
		}
	}
	return out, nil
}
