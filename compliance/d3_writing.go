package compliance

// D3 故事板、句子级证据绑定与写作核验纯规则：
// 1. 引用标记生成与确定性渲染（RenderGroundedDraft）：将 [[EVID:...]] / [[KB:...]] / [[COMMIT:...]]
//    从已可采企业证据、已核验知识库证据与已确认承诺台账确定性展开或归一化，阻断未核验事实与承诺进入正文；
// 2. 句子级断言抽取与证据绑定校验（VerifySectionClaims）：按段落与句子切分正文，识别投标人事实断言
//    （bidder_fact）、关键承诺断言（commitment）、招标要求复述（tender_restatement）与通用技术叙述
//    （general_narrative），校验每条事实/承诺句必须绑定有效证据或承诺台账且数值/主体/证书/型号一致；
// 3. 针对性度量与反套话相似度（Specificity & Boilerplate Similarity）：确定性计算招标专属事实覆盖率、
//    事实句绑定率、承诺句绑定率、未绑定断言数，以及与知识库/通用套话模板的最大 n-gram 相似度。
//
// 本文件不访问数据库、文件系统或网络。

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// WritingVerificationRuleVersion 标识 D3 写作核验规则版本。
const WritingVerificationRuleVersion = "d3-writing-v1"

// 写作核验结论。
const (
	WritingVerdictPassed = "passed"
	WritingVerdictFailed = "failed"
	WritingVerdictStale  = "stale"
)

// 句子断言分类。
const (
	ClaimTypeBidderFact        = "bidder_fact"
	ClaimTypeCommitment        = "commitment"
	ClaimTypeTenderRestatement = "tender_restatement"
	ClaimTypeGeneralNarrative  = "general_narrative"
)

// 句子断言核验状态。
const (
	ClaimStatusGrounded   = "grounded"
	ClaimStatusUngrounded = "ungrounded"
	ClaimStatusExempt     = "exempt"
)

// 默认 E 级写作核验门槛。
const (
	DefaultMinSpecificityRatio      = 0.25
	DefaultMaxBoilerplateSimilarity = 0.70
	DefaultMaxKBSimilarity          = 0.85
)

// GroundedFactSource 是可供正文引用与核验的单条企业证据（来自 admissibility_items 关联的资质或业绩）。
type GroundedFactSource struct {
	AdmissibilityItemID  int64    `json:"admissibility_item_id"`
	RequirementCode      string   `json:"requirement_code,omitempty"`
	EvidenceDomain       string   `json:"evidence_domain"` // credential | performance
	EvidenceID           int64    `json:"evidence_id"`
	Code                 string   `json:"code"`
	Category             string   `json:"category,omitempty"`
	Title                string   `json:"title"`
	SubjectName          string   `json:"subject_name"`
	SubjectType          string   `json:"subject_type"`
	CertificateNo        string   `json:"certificate_no,omitempty"`
	QualificationLevel   string   `json:"qualification_level,omitempty"`
	Scope                string   `json:"scope,omitempty"`
	ModelSpec            string   `json:"model_spec,omitempty"`
	SocialSecurityMonths int      `json:"social_security_months,omitempty"`
	ProjectName          string   `json:"project_name,omitempty"`
	ClientName           string   `json:"client_name,omitempty"`
	ContractAmount       float64  `json:"contract_amount,omitempty"`
	AcceptanceDate       string   `json:"acceptance_date,omitempty"`
	Verdict              string   `json:"verdict"` // admissible | inadmissible | manual | stale
	PageLabels           []string `json:"page_labels,omitempty"`
}

// CanonicalFactText 生成该企业证据的确定性标准事实表述（附带 [EVID:<id>] 引用标记）。
func (f GroundedFactSource) CanonicalFactText() string {
	pageSuffix := ""
	if len(f.PageLabels) > 0 {
		pageSuffix = "，原件页码：" + strings.Join(f.PageLabels, "、")
	}
	switch f.EvidenceDomain {
	case "performance":
		proj := strings.TrimSpace(f.ProjectName)
		if proj == "" {
			proj = strings.TrimSpace(f.Title)
		}
		var parts []string
		if f.ClientName != "" {
			parts = append(parts, "业主："+f.ClientName)
		}
		if f.ContractAmount > 0 {
			amtWan := f.ContractAmount
			if amtWan >= 100000 {
				amtWan = amtWan / 10000.0
			}
			parts = append(parts, fmt.Sprintf("合同金额：%.2f万元", amtWan))
		}
		if f.AcceptanceDate != "" {
			parts = append(parts, "验收日期："+f.AcceptanceDate)
		}
		if f.Scope != "" {
			parts = append(parts, "范围："+f.Scope)
		}
		detail := strings.Join(parts, "，")
		if detail != "" {
			return fmt.Sprintf("我方（%s）已完成同类项目《%s》（%s%s）[EVID:%d]",
				nonEmpty(f.SubjectName, "投标人"), proj, detail, pageSuffix, f.AdmissibilityItemID)
		}
		return fmt.Sprintf("我方（%s）已完成同类项目《%s》（编码：%s%s）[EVID:%d]",
			nonEmpty(f.SubjectName, "投标人"), proj, f.Code, pageSuffix, f.AdmissibilityItemID)
	default:
		var parts []string
		if f.CertificateNo != "" {
			parts = append(parts, "证书编号："+f.CertificateNo)
		}
		if f.QualificationLevel != "" {
			parts = append(parts, "等级："+f.QualificationLevel)
		}
		if f.ModelSpec != "" {
			parts = append(parts, "型号规格："+f.ModelSpec)
		}
		if f.SocialSecurityMonths > 0 {
			parts = append(parts, fmt.Sprintf("连续缴纳社保：%d个月", f.SocialSecurityMonths))
		}
		if f.Scope != "" {
			parts = append(parts, "范围："+f.Scope)
		}
		detail := strings.Join(parts, "，")
		if detail != "" {
			return fmt.Sprintf("我方（%s）持有《%s》（%s%s）[EVID:%d]",
				nonEmpty(f.SubjectName, "投标人"), f.Title, detail, pageSuffix, f.AdmissibilityItemID)
		}
		return fmt.Sprintf("我方（%s）持有《%s》（编码：%s%s）[EVID:%d]",
			nonEmpty(f.SubjectName, "投标人"), f.Title, f.Code, pageSuffix, f.AdmissibilityItemID)
	}
}

// GroundedKBSource 是可供正文引用的已核验知识库证据（来自 retrieval_evidence）。
type GroundedKBSource struct {
	EvidenceID int64  `json:"evidence_id"`
	Status     string `json:"status"`   // verified | contested | stale | unverified
	Relation   string `json:"relation"` // supports | contradicts | partial
	Title      string `json:"title,omitempty"`
	Excerpt    string `json:"excerpt"`
}

// GroundedCommitmentSource 是可供正文引用的承诺台账项（来自 commitment_ledger）。
type GroundedCommitmentSource struct {
	ID             int64    `json:"id"`
	CommitmentCode string   `json:"commitment_code"`
	Category       string   `json:"category"`
	Label          string   `json:"label"`
	Dimension      string   `json:"dimension"`
	CommittedValue *float64 `json:"committed_value,omitempty"`
	CommittedUnit  string   `json:"committed_unit,omitempty"`
	CommittedRaw   string   `json:"committed_raw"`
	Status         string   `json:"status"` // draft | confirmed | conflict | signed_off | superseded
}

// CanonicalCommitmentText 生成该承诺台账项的确定性标准承诺表述（附带 [COMMIT:<id>] 标记）。
func (c GroundedCommitmentSource) CanonicalCommitmentText() string {
	val := strings.TrimSpace(c.CommittedRaw)
	if val == "" && c.CommittedValue != nil {
		if math.Mod(*c.CommittedValue, 1) == 0 {
			val = fmt.Sprintf("%.0f%s", *c.CommittedValue, c.CommittedUnit)
		} else {
			val = fmt.Sprintf("%g%s", *c.CommittedValue, c.CommittedUnit)
		}
	}
	label := strings.TrimSpace(c.Label)
	if label == "" {
		label = c.CommitmentCode
	}
	if strings.HasPrefix(c.CommitmentCode, "model:") {
		prod := strings.TrimPrefix(c.CommitmentCode, "model:")
		return fmt.Sprintf("我方承诺%s选用型号 %s [COMMIT:%d]", prod, val, c.ID)
	}
	return fmt.Sprintf("我方承诺%s为 %s [COMMIT:%d]", label, val, c.ID)
}

// RenderGroundedInput 是确定性引用渲染的输入。
type RenderGroundedInput struct {
	TemplateMD      string                     `json:"template_md"`
	AdmissibleFacts []GroundedFactSource       `json:"admissible_facts"`
	VerifiedKB      []GroundedKBSource         `json:"verified_kb,omitempty"`
	Commitments     []GroundedCommitmentSource `json:"commitments"`
	// 若非空，限制只允许渲染故事板已声明的证据与承诺 ID。
	AllowedAdmissibilityItemIDs []int64 `json:"allowed_admissibility_item_ids,omitempty"`
	AllowedEvidenceIDs          []int64 `json:"allowed_evidence_ids,omitempty"`
	AllowedCommitmentIDs        []int64 `json:"allowed_commitment_ids,omitempty"`
}

// UnresolvedToken 记录无法展开或未通过核验的引用 token。
type UnresolvedToken struct {
	Token  string `json:"token"`
	Kind   string `json:"kind"` // evid | kb | commit
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
}

// RenderGroundedOutput 是确定性引用渲染的结果。
type RenderGroundedOutput struct {
	RenderedMD                  string            `json:"rendered_md"`
	UsedAdmissibilityItemIDs    []int64           `json:"used_admissibility_item_ids,omitempty"`
	UsedEvidenceIDs             []int64           `json:"used_evidence_ids,omitempty"`
	UsedCommitmentIDs           []int64           `json:"used_commitment_ids,omitempty"`
	Unresolved                  []UnresolvedToken `json:"unresolved,omitempty"`
}

var (
	// [[EVID:101]] 或 [[EVID:CRED-ISO9001]] 或 [[EVID:101:inline]] / [[EVID:101:full]]
	doubleTokenRe = regexp.MustCompile(`\[\[(EVID|KB|COMMIT):([^\]:\s]+)(?::(inline|full))?\]\]`)
	// 句内已归一引用标记：[EVID:101] / [KB:202] / [COMMIT:303]
	singleTokenRe = regexp.MustCompile(`\[(EVID|KB|COMMIT):([^\]\s]+)\]`)
)

// RenderGroundedDraft 将模板中的 [[EVID:...]] / [[KB:...]] / [[COMMIT:...]] 确定性渲染为受控文本与标准标记。
// 任何引用不存在、未可采、未核验、未确认或不在故事板白名单内的 token 均记录到 Unresolved 并保持原样返回。
func RenderGroundedDraft(in RenderGroundedInput) RenderGroundedOutput {
	factByID := make(map[int64]GroundedFactSource, len(in.AdmissibleFacts))
	factByCode := make(map[string]GroundedFactSource, len(in.AdmissibleFacts))
	for _, f := range in.AdmissibleFacts {
		factByID[f.AdmissibilityItemID] = f
		if f.Code != "" {
			factByCode[strings.ToUpper(strings.TrimSpace(f.Code))] = f
		}
	}
	kbByID := make(map[int64]GroundedKBSource, len(in.VerifiedKB))
	for _, k := range in.VerifiedKB {
		kbByID[k.EvidenceID] = k
	}
	commitByID := make(map[int64]GroundedCommitmentSource, len(in.Commitments))
	commitByCode := make(map[string]GroundedCommitmentSource, len(in.Commitments))
	for _, c := range in.Commitments {
		commitByID[c.ID] = c
		if c.CommitmentCode != "" {
			commitByCode[strings.ToLower(strings.TrimSpace(c.CommitmentCode))] = c
		}
	}

	allowedFact := toInt64Set(in.AllowedAdmissibilityItemIDs)
	allowedKB := toInt64Set(in.AllowedEvidenceIDs)
	allowedCommit := toInt64Set(in.AllowedCommitmentIDs)

	usedFacts := map[int64]bool{}
	usedKBs := map[int64]bool{}
	usedCommits := map[int64]bool{}
	var unresolved []UnresolvedToken

	lines := strings.Split(strings.ReplaceAll(in.TemplateMD, "\r\n", "\n"), "\n")
	for li, line := range lines {
		matches := doubleTokenRe.FindAllStringSubmatchIndex(line, -1)
		if len(matches) == 0 {
			continue
		}
		// 判断整行去掉列表前缀与双括号 token 后是否为空（即独立占位符模式）。
		strippedLine := doubleTokenRe.ReplaceAllString(line, "")
		strippedLine = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(strippedLine), "-*0123456789.、)）"))
		standaloneLine := hanAlnumCount(strippedLine) < 4

		var b strings.Builder
		last := 0
		for _, m := range matches {
			b.WriteString(line[last:m[0]])
			rawToken := line[m[0]:m[1]]
			kind := line[m[2]:m[3]]
			ref := strings.TrimSpace(line[m[4]:m[5]])
			mode := ""
			if m[6] >= 0 {
				mode = line[m[6]:m[7]]
			}
			// 若该 token 所在分句（按句号/分号切分）除 token 外无其他实质文字，则默认展开为完整标准事实/承诺句。
			segClean := surroundingClauseClean(line, m[0], m[1])
			fullExpand := mode == "full" || (mode == "" && (standaloneLine || hanAlnumCount(segClean) < 4))

			switch kind {
			case "EVID":
				f, ok := lookupFact(ref, factByID, factByCode)
				if !ok {
					unresolved = append(unresolved, UnresolvedToken{Token: rawToken, Kind: "evid", Ref: ref, Reason: "evidence_not_found"})
					b.WriteString(rawToken)
					break
				}
				if f.Verdict != Admissible {
					unresolved = append(unresolved, UnresolvedToken{Token: rawToken, Kind: "evid", Ref: ref, Reason: "evidence_not_admissible:" + f.Verdict})
					b.WriteString(rawToken)
					break
				}
				if len(allowedFact) > 0 && !allowedFact[f.AdmissibilityItemID] {
					unresolved = append(unresolved, UnresolvedToken{Token: rawToken, Kind: "evid", Ref: ref, Reason: "evidence_not_in_storyboard"})
					b.WriteString(rawToken)
					break
				}
				usedFacts[f.AdmissibilityItemID] = true
				if fullExpand {
					b.WriteString(f.CanonicalFactText())
				} else {
					b.WriteString(fmt.Sprintf("[EVID:%d]", f.AdmissibilityItemID))
				}

			case "KB":
				id, err := strconv.ParseInt(ref, 10, 64)
				k, ok := kbByID[id]
				if err != nil || !ok {
					unresolved = append(unresolved, UnresolvedToken{Token: rawToken, Kind: "kb", Ref: ref, Reason: "kb_evidence_not_found"})
					b.WriteString(rawToken)
					break
				}
				if k.Status != "verified" || k.Relation != "supports" {
					unresolved = append(unresolved, UnresolvedToken{Token: rawToken, Kind: "kb", Ref: ref, Reason: fmt.Sprintf("kb_evidence_unverified:%s/%s", k.Status, k.Relation)})
					b.WriteString(rawToken)
					break
				}
				if len(allowedKB) > 0 && !allowedKB[k.EvidenceID] {
					unresolved = append(unresolved, UnresolvedToken{Token: rawToken, Kind: "kb", Ref: ref, Reason: "kb_evidence_not_in_storyboard"})
					b.WriteString(rawToken)
					break
				}
				usedKBs[k.EvidenceID] = true
				if fullExpand {
					excerpt := strings.TrimSpace(k.Excerpt)
					if excerpt == "" {
						excerpt = strings.TrimSpace(k.Title)
					}
					b.WriteString(fmt.Sprintf("%s [KB:%d]", excerpt, k.EvidenceID))
				} else {
					b.WriteString(fmt.Sprintf("[KB:%d]", k.EvidenceID))
				}

			case "COMMIT":
				c, ok := lookupCommit(ref, commitByID, commitByCode)
				if !ok {
					unresolved = append(unresolved, UnresolvedToken{Token: rawToken, Kind: "commit", Ref: ref, Reason: "commitment_not_found"})
					b.WriteString(rawToken)
					break
				}
				if c.Status != "confirmed" && c.Status != "signed_off" {
					unresolved = append(unresolved, UnresolvedToken{Token: rawToken, Kind: "commit", Ref: ref, Reason: "commitment_unconfirmed:" + c.Status})
					b.WriteString(rawToken)
					break
				}
				if len(allowedCommit) > 0 && !allowedCommit[c.ID] {
					unresolved = append(unresolved, UnresolvedToken{Token: rawToken, Kind: "commit", Ref: ref, Reason: "commitment_not_in_storyboard"})
					b.WriteString(rawToken)
					break
				}
				usedCommits[c.ID] = true
				if fullExpand {
					b.WriteString(c.CanonicalCommitmentText())
				} else {
					b.WriteString(fmt.Sprintf("[COMMIT:%d]", c.ID))
				}
			}
			last = m[1]
		}
		b.WriteString(line[last:])
		lines[li] = b.String()
	}

	rendered := strings.Join(lines, "\n")
	// 同时统计正文中直接书写的单括号 [EVID:...] / [KB:...] / [COMMIT:...]
	for _, m := range singleTokenRe.FindAllStringSubmatch(rendered, -1) {
		kind, ref := m[1], strings.TrimSpace(m[2])
		switch kind {
		case "EVID":
			if f, ok := lookupFact(ref, factByID, factByCode); ok {
				usedFacts[f.AdmissibilityItemID] = true
			}
		case "KB":
			if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
				if _, ok := kbByID[id]; ok {
					usedKBs[id] = true
				}
			}
		case "COMMIT":
			if c, ok := lookupCommit(ref, commitByID, commitByCode); ok {
				usedCommits[c.ID] = true
			}
		}
	}

	return RenderGroundedOutput{
		RenderedMD:               rendered,
		UsedAdmissibilityItemIDs: sortedKeys(usedFacts),
		UsedEvidenceIDs:          sortedKeys(usedKBs),
		UsedCommitmentIDs:        sortedKeys(usedCommits),
		Unresolved:               unresolved,
	}
}

func surroundingClauseClean(line string, start, end int) string {
	left := strings.LastIndexAny(line[:start], "。；;！!？?")
	if left < 0 {
		left = 0
	} else {
		left++
	}
	rightRel := strings.IndexAny(line[end:], "。；;！!？?")
	right := len(line)
	if rightRel >= 0 {
		right = end + rightRel
	}
	seg := doubleTokenRe.ReplaceAllString(line[left:right], "")
	seg = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(seg), "-*0123456789.、)）"))
	return seg
}

func lookupFact(ref string, byID map[int64]GroundedFactSource, byCode map[string]GroundedFactSource) (GroundedFactSource, bool) {
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		if f, ok := byID[id]; ok {
			return f, true
		}
	}
	if f, ok := byCode[strings.ToUpper(strings.TrimSpace(ref))]; ok {
		return f, true
	}
	return GroundedFactSource{}, false
}

func lookupCommit(ref string, byID map[int64]GroundedCommitmentSource, byCode map[string]GroundedCommitmentSource) (GroundedCommitmentSource, bool) {
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		if c, ok := byID[id]; ok {
			return c, true
		}
	}
	if c, ok := byCode[strings.ToLower(strings.TrimSpace(ref))]; ok {
		return c, true
	}
	return GroundedCommitmentSource{}, false
}

// ---------------------------------------------------------------------------
// 句子级断言抽取、证据/承诺核验与 E 级指标计算
// ---------------------------------------------------------------------------

// SentenceClaim 是章节正文中的单句断言及核验结论。
type SentenceClaim struct {
	SentenceIndex        int      `json:"sentence_index"`
	ParagraphAnchor      string   `json:"paragraph_anchor"`
	Text                 string   `json:"text"`
	CleanText            string   `json:"clean_text"`
	ClaimType            string   `json:"claim_type"` // bidder_fact | commitment | tender_restatement | general_narrative
	Status               string   `json:"status"`     // grounded | ungrounded | exempt
	AdmissibilityItemIDs []int64  `json:"admissibility_item_ids,omitempty"`
	RetrievalEvidenceIDs []int64  `json:"retrieval_evidence_ids,omitempty"`
	CommitmentIDs        []int64  `json:"commitment_ids,omitempty"`
	ExtractedMetrics     []string `json:"extracted_metrics,omitempty"`
	ExtractedEntities    []string `json:"extracted_entities,omitempty"`
	Reasons              []string `json:"reasons,omitempty"`
}

// WritingVerificationReason 是章节级写作核验失败原因。
type WritingVerificationReason struct {
	Code          string `json:"code"`
	SentenceIndex int    `json:"sentence_index,omitempty"`
	Message       string `json:"message"`
}

// WritingVerificationMetrics 是章节级写作核验的确定性 E 级指标。
type WritingVerificationMetrics struct {
	TotalSentences                int      `json:"total_sentences"`
	BidderFactSentences           int      `json:"bidder_fact_sentences"`
	GroundedFactSentences         int      `json:"grounded_fact_sentences"`
	UngroundedFactSentences       int      `json:"ungrounded_fact_sentences"`
	CommitmentSentences           int      `json:"commitment_sentences"`
	GroundedCommitmentSentences   int      `json:"grounded_commitment_sentences"`
	UngroundedCommitmentSentences int      `json:"ungrounded_commitment_sentences"`
	TenderRestatementSentences    int      `json:"tender_restatement_sentences"`
	GeneralNarrativeSentences     int      `json:"general_narrative_sentences"`
	UngroundedClaimCount          int      `json:"ungrounded_claim_count"`
	FactBindingRate               float64  `json:"fact_binding_rate"`
	CommitmentBindingRate         float64  `json:"commitment_binding_rate"`
	TenderFactHits                int      `json:"tender_fact_hits"`
	TenderFactTotal               int      `json:"tender_fact_total"`
	SpecificityRatio              float64  `json:"specificity_ratio"`
	KBSimilarityMax               float64  `json:"kb_similarity_max"`
	BoilerplateSimilarityMax      float64  `json:"boilerplate_similarity_max"`
	MatchedTenderTerms            []string `json:"matched_tender_terms,omitempty"`
	MissingTenderTerms            []string `json:"missing_tender_terms,omitempty"`
}

// WritingVerificationResult 是章节正文的完整 D3 写作核验输出。
type WritingVerificationResult struct {
	RuleVersion string                      `json:"rule_version"`
	Verdict     string                      `json:"verdict"` // passed | failed
	Metrics     WritingVerificationMetrics  `json:"metrics"`
	Claims      []SentenceClaim             `json:"claims"`
	Reasons     []WritingVerificationReason `json:"reasons,omitempty"`
}

// VerifyClaimsInput 是章节写作核验的输入。
type VerifyClaimsInput struct {
	SectionID                   int64                      `json:"section_id"`
	SectionCode                 string                     `json:"section_code"`
	SectionTitle                string                     `json:"section_title"`
	ContentMD                   string                     `json:"content_md"`
	Requirements                []Requirement              `json:"requirements,omitempty"`
	TenderFacts                 []string                   `json:"tender_facts,omitempty"`
	AdmissibleFacts             []GroundedFactSource       `json:"admissible_facts,omitempty"`
	VerifiedKB                  []GroundedKBSource         `json:"verified_kb,omitempty"`
	Commitments                 []GroundedCommitmentSource `json:"commitments,omitempty"`
	AllowedAdmissibilityItemIDs []int64                    `json:"allowed_admissibility_item_ids,omitempty"`
	AllowedEvidenceIDs          []int64                    `json:"allowed_evidence_ids,omitempty"`
	AllowedCommitmentIDs        []int64                    `json:"allowed_commitment_ids,omitempty"`
	KBChunks                    []string                   `json:"kb_chunks,omitempty"`
	BoilerplateCorpus           []string                   `json:"boilerplate_corpus,omitempty"`
	MinSpecificityRatio         float64                    `json:"min_specificity_ratio,omitempty"`
	MaxBoilerplateSimilarity    float64                    `json:"max_boilerplate_similarity,omitempty"`
	MaxKBSimilarity             float64                    `json:"max_kb_similarity,omitempty"`
}

var (
	// 投标人自我指称。
	bidderSelfCueRe     = regexp.MustCompile(`我方|我公司|本公司|我司|我单位|本单位|投标人(?:已|具备|持有|拥有|完成|承建|投入|拟派)`)
	firstPersonBidderRe = regexp.MustCompile(`我方|我公司|本公司|我司|我单位|本单位`)
	tenderRestatementRe = regexp.MustCompile(`^响应要求\s+|招标文件|招标要求|采购需求|采购文件|招标人要求|采购人要求|根据要求|要求为|要求[:：]`)

	// 严格客观企业事实特征（资质证书、认证、同类业绩、合同金额、验收、人员证书、社保、专利软著等）。
	strictObjectiveFactRe = regexp.MustCompile(
		`证书编号|资质证书|认证证书|ISO\s*\d{4,5}|CMMI\s*[1-5L级]?|ITSS|CCRC|[一二三甲乙丙特]级资质|` +
			`同类项目|类似项目|同类业绩|类似业绩|典型业绩|承建过|承担过|已完成.*项目|合同金额|合同额|竣工验收|验收报告|验收合格|` +
			`连续缴纳.*社保|近\s*\d+\s*个月社保|社保证明|注册建造师|高级工程师|PMP|CISP|CISSP|软著登字|专利号|检测报告|销售许可证`)

	// 一般投标人事实特征（含设备选型、产品参数、团队人数配置、自有平台等）。
	generalBidderFactRe = regexp.MustCompile(
		`持有|具备.*(?:资质|证书|认证|业绩)|获得.*(?:认证|奖项|证书)|通过.*认证|` +
			`业主[:：]|客户[:：]|(?:项目经理|技术负责人).{0,8}(?:具备|持有|具有|获得|拥有)|工程师\s*\d+\s*[人名]|` +
			`自主研发|自有知识产权|原件页码`)

	// 明确承诺句式。
	commitmentCueRe = regexp.MustCompile(
		`(?:我方|我公司|本公司|我司|我单位)?\s*(?:郑重)?(?:承诺|保证).*(?:工期|周期|质保|保修|响应|到场|修复|恢复|可用性|可用率|在线率|团队|驻场|人数|型号|偏离)|` +
			`(?:总工期|建设工期|实施周期|交付周期|免费质保期|质保期|保修期|故障响应时间|响应时限|到场时限|到达现场时间|故障修复时间|修复时限|系统可用性|可用率|项目团队人数|驻场人数)\s*(?:为|不少于|不低于|不超过|不高于|控制在|达到|承诺为)`)

	// 证书编号抽取。
	certNoExtractRe = regexp.MustCompile(`(?:证书编号|编号|证号|登记号|专利号)[:：]?\s*([A-Za-z0-9][A-Za-z0-9\-_/.]{3,32})`)

	// 默认通用废话/套话模板库（用于检测毫无针对性的八股套话段落）。
	defaultBoilerplateTemplates = []string{
		"我公司高度重视本项目，本着科学管理、精心组织、质量第一、用户至上的原则，选派经验丰富的项目经理和技术骨干组建项目团队，确保按期高质量完成本项目全部建设内容。",
		"本项目建设将严格遵循国家及行业相关标准规范，采用先进成熟、安全可靠、开放可扩展的技术架构，实现系统高可用、高性能、高安全与易维护的目标，全面满足采购人各项业务需求。",
		"我方将建立健全项目管理体系与质量保障机制，实行项目经理负责制，对项目进度、质量、安全、成本进行全过程精细化管控，确保工程顺利实施并通过最终验收。",
		"在售后服务阶段，我公司将秉持客户至上、快速响应、终身维护的服务理念，提供全方位的技术支持与运维保障服务，定期巡检回访，保障系统长期稳定运行。",
	}
)

// VerifySectionClaims 对章节正文执行句子级断言抽取、证据/承诺绑定核验与 E 级指标计算。
func VerifySectionClaims(in VerifyClaimsInput) WritingVerificationResult {
	minSpec := in.MinSpecificityRatio
	if minSpec <= 0 {
		minSpec = DefaultMinSpecificityRatio
	}
	maxBoiler := in.MaxBoilerplateSimilarity
	if maxBoiler <= 0 {
		maxBoiler = DefaultMaxBoilerplateSimilarity
	}
	maxKB := in.MaxKBSimilarity
	if maxKB <= 0 {
		maxKB = DefaultMaxKBSimilarity
	}

	factByID := make(map[int64]GroundedFactSource, len(in.AdmissibleFacts))
	factByCode := make(map[string]GroundedFactSource, len(in.AdmissibleFacts))
	for _, f := range in.AdmissibleFacts {
		factByID[f.AdmissibilityItemID] = f
		if f.Code != "" {
			factByCode[strings.ToUpper(strings.TrimSpace(f.Code))] = f
		}
	}
	kbByID := make(map[int64]GroundedKBSource, len(in.VerifiedKB))
	for _, k := range in.VerifiedKB {
		kbByID[k.EvidenceID] = k
	}
	commitByID := make(map[int64]GroundedCommitmentSource, len(in.Commitments))
	commitByCode := make(map[string]GroundedCommitmentSource, len(in.Commitments))
	for _, c := range in.Commitments {
		commitByID[c.ID] = c
		if c.CommitmentCode != "" {
			commitByCode[strings.ToLower(strings.TrimSpace(c.CommitmentCode))] = c
		}
	}

	allowedFact := toInt64Set(in.AllowedAdmissibilityItemIDs)
	allowedKB := toInt64Set(in.AllowedEvidenceIDs)
	allowedCommit := toInt64Set(in.AllowedCommitmentIDs)

	var (
		claims     []SentenceClaim
		reasons    []WritingVerificationReason
		metrics    WritingVerificationMetrics
		cleanParts []string
	)

	// 检查是否存在未展开的双括号占位符 [[EVID:...]] / [[KB:...]] / [[COMMIT:...]]
	for _, m := range doubleTokenRe.FindAllStringSubmatch(in.ContentMD, -1) {
		reasons = append(reasons, WritingVerificationReason{
			Code:    "unresolved_citation_token",
			Message: fmt.Sprintf("正文存在未展开或无法核验的引用标记 %s", m[0]),
		})
	}

	paras := Paragraphs(in.ContentMD)
	sentIdx := 0
	for _, p := range paras {
		if p.Heading {
			continue
		}
		for _, rawSent := range splitSentences(p.Text) {
			rawSent = strings.TrimSpace(rawSent)
			if hanAlnumCount(rawSent) < 3 {
				continue
			}
			sentIdx++
			claim := evaluateSentenceClaim(
				sentIdx, p.Anchor, rawSent,
				factByID, factByCode,
				kbByID,
				commitByID, commitByCode,
				allowedFact, allowedKB, allowedCommit,
			)
			claims = append(claims, claim)
			cleanParts = append(cleanParts, claim.CleanText)

			metrics.TotalSentences++
			switch claim.ClaimType {
			case ClaimTypeBidderFact:
				metrics.BidderFactSentences++
				if claim.Status == ClaimStatusGrounded {
					metrics.GroundedFactSentences++
				} else {
					metrics.UngroundedFactSentences++
					metrics.UngroundedClaimCount++
					for _, r := range claim.Reasons {
						reasons = append(reasons, WritingVerificationReason{
							Code:          "ungrounded_fact_claim",
							SentenceIndex: claim.SentenceIndex,
							Message:       fmt.Sprintf("第 %d 句（%s）事实断言未通过证据绑定：%s", claim.SentenceIndex, claim.ParagraphAnchor, r),
						})
					}
				}
			case ClaimTypeCommitment:
				metrics.CommitmentSentences++
				if claim.Status == ClaimStatusGrounded {
					metrics.GroundedCommitmentSentences++
				} else {
					metrics.UngroundedCommitmentSentences++
					metrics.UngroundedClaimCount++
					for _, r := range claim.Reasons {
						reasons = append(reasons, WritingVerificationReason{
							Code:          "ungrounded_commitment_claim",
							SentenceIndex: claim.SentenceIndex,
							Message:       fmt.Sprintf("第 %d 句（%s）承诺断言未通过台账绑定：%s", claim.SentenceIndex, claim.ParagraphAnchor, r),
						})
					}
				}
			case ClaimTypeTenderRestatement:
				metrics.TenderRestatementSentences++
			default:
				metrics.GeneralNarrativeSentences++
			}
		}
	}

	if metrics.BidderFactSentences > 0 {
		metrics.FactBindingRate = round4(float64(metrics.GroundedFactSentences) / float64(metrics.BidderFactSentences))
	} else {
		metrics.FactBindingRate = 1.0
	}
	if metrics.CommitmentSentences > 0 {
		metrics.CommitmentBindingRate = round4(float64(metrics.GroundedCommitmentSentences) / float64(metrics.CommitmentSentences))
	} else {
		metrics.CommitmentBindingRate = 1.0
	}

	fullCleanText := strings.Join(cleanParts, "\n")

	// 计算针对性指标（招标专属事实覆盖率）
	tenderTerms := ExtractTenderSpecificTerms(in.Requirements, in.TenderFacts)
	metrics.TenderFactTotal = len(tenderTerms)
	if len(tenderTerms) > 0 {
		lowerClean := strings.ToLower(fullCleanText)
		for _, term := range tenderTerms {
			if strings.Contains(lowerClean, strings.ToLower(term)) {
				metrics.TenderFactHits++
				metrics.MatchedTenderTerms = append(metrics.MatchedTenderTerms, term)
			} else {
				metrics.MissingTenderTerms = append(metrics.MissingTenderTerms, term)
			}
		}
		metrics.SpecificityRatio = round4(float64(metrics.TenderFactHits) / float64(metrics.TenderFactTotal))
		if metrics.SpecificityRatio < minSpec {
			reasons = append(reasons, WritingVerificationReason{
				Code:    "specificity_below_threshold",
				Message: fmt.Sprintf("章节针对性指标 %.1f%% 低于门槛 %.1f%%（命中 %d/%d 项招标专属要素）", metrics.SpecificityRatio*100, minSpec*100, metrics.TenderFactHits, metrics.TenderFactTotal),
			})
		}
	} else {
		metrics.SpecificityRatio = 1.0
	}

	// 计算与知识库分块的最大相似度
	if len(in.KBChunks) > 0 && fullCleanText != "" {
		maxSim := 0.0
		for _, chunk := range in.KBChunks {
			for _, p := range cleanParts {
				if utf8.RuneCountInString(p) < 20 {
					continue
				}
				if sim := ngramSimilarity(p, chunk, 3); sim > maxSim {
					maxSim = sim
				}
			}
		}
		metrics.KBSimilarityMax = round4(maxSim)
		if metrics.KBSimilarityMax > maxKB {
			reasons = append(reasons, WritingVerificationReason{
				Code:    "kb_similarity_exceeded",
				Message: fmt.Sprintf("正文段落与知识库原文最大相似度 %.1f%% 超过阈值 %.1f%%，涉嫌未改写直接拼接", metrics.KBSimilarityMax*100, maxKB*100),
			})
		}
	}

	// 计算与通用八股套话模板的最大相似度
	boilerTemplates := append([]string{}, defaultBoilerplateTemplates...)
	boilerTemplates = append(boilerTemplates, in.BoilerplateCorpus...)
	if fullCleanText != "" && len(boilerTemplates) > 0 {
		maxBoilerSim := 0.0
		for _, tmpl := range boilerTemplates {
			for _, p := range cleanParts {
				if utf8.RuneCountInString(p) < 16 {
					continue
				}
				if sim := ngramSimilarity(p, tmpl, 3); sim > maxBoilerSim {
					maxBoilerSim = sim
				}
			}
		}
		metrics.BoilerplateSimilarityMax = round4(maxBoilerSim)
		if metrics.BoilerplateSimilarityMax > maxBoiler {
			reasons = append(reasons, WritingVerificationReason{
				Code:    "boilerplate_similarity_exceeded",
				Message: fmt.Sprintf("正文段落与通用套话模板最大相似度 %.1f%% 超过阈值 %.1f%%，缺乏项目针对性", metrics.BoilerplateSimilarityMax*100, maxBoiler*100),
			})
		}
	}

	if metrics.TotalSentences == 0 {
		reasons = append(reasons, WritingVerificationReason{
			Code:    "empty_section_body",
			Message: "章节正文无有效句子",
		})
	}

	verdict := WritingVerdictPassed
	if len(reasons) > 0 || metrics.UngroundedClaimCount > 0 {
		verdict = WritingVerdictFailed
	}

	return WritingVerificationResult{
		RuleVersion: WritingVerificationRuleVersion,
		Verdict:     verdict,
		Metrics:     metrics,
		Claims:      claims,
		Reasons:     reasons,
	}
}

func splitSentences(para string) []string {
	// 先保护小数点（如 99.95%、120.50万元）与条款号（如 4.2.1），仅按中文句号/分号/感叹号/问号与换行切分。
	var out []string
	var buf strings.Builder
	for _, r := range para {
		buf.WriteRune(r)
		if r == '。' || r == '；' || r == ';' || r == '！' || r == '？' || r == '\n' {
			if s := strings.TrimSpace(buf.String()); s != "" {
				out = append(out, s)
			}
			buf.Reset()
		}
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		out = append(out, s)
	}
	return out
}

func evaluateSentenceClaim(
	idx int,
	anchor string,
	rawSent string,
	factByID map[int64]GroundedFactSource,
	factByCode map[string]GroundedFactSource,
	kbByID map[int64]GroundedKBSource,
	commitByID map[int64]GroundedCommitmentSource,
	commitByCode map[string]GroundedCommitmentSource,
	allowedFact map[int64]bool,
	allowedKB map[int64]bool,
	allowedCommit map[int64]bool,
) SentenceClaim {
	clean := doubleTokenRe.ReplaceAllString(rawSent, "")
	clean = singleTokenRe.ReplaceAllString(clean, "")
	clean = strings.TrimSpace(clean)

	var (
		evidRefs   []string
		kbRefs     []string
		commitRefs []string
	)
	for _, m := range singleTokenRe.FindAllStringSubmatch(rawSent, -1) {
		switch m[1] {
		case "EVID":
			evidRefs = append(evidRefs, strings.TrimSpace(m[2]))
		case "KB":
			kbRefs = append(kbRefs, strings.TrimSpace(m[2]))
		case "COMMIT":
			commitRefs = append(commitRefs, strings.TrimSpace(m[2]))
		}
	}
	for _, m := range doubleTokenRe.FindAllStringSubmatch(rawSent, -1) {
		switch m[1] {
		case "EVID":
			evidRefs = append(evidRefs, strings.TrimSpace(m[2]))
		case "KB":
			kbRefs = append(kbRefs, strings.TrimSpace(m[2]))
		case "COMMIT":
			commitRefs = append(commitRefs, strings.TrimSpace(m[2]))
		}
	}

	// 抽取本句中的承诺指标与设备选型。
	mentions := Extract(SectionText{SectionID: 1, RevNo: 1, Seq: 1, ContentMD: clean})
	var metricKeys []string
	var nonModelMentions []Mention
	var modelMentions []Mention
	for _, m := range mentions {
		metricKeys = append(metricKeys, m.Metric)
		if strings.HasPrefix(m.Metric, MetricModel+":") {
			modelMentions = append(modelMentions, m)
		} else {
			nonModelMentions = append(nonModelMentions, m)
		}
	}

	var entities []string
	if cm := certNoExtractRe.FindStringSubmatch(clean); len(cm) > 1 {
		entities = append(entities, "cert:"+cm[1])
	}
	for _, mm := range modelMentions {
		entities = append(entities, mm.Metric+"="+mm.Text)
	}

	// 判断句子类型：
	// 1. 若引述招标文件要求且无我方第一人称事实/承诺标记，归为 tender_restatement；
	isQuotedReq := tenderRestatementRe.MatchString(clean) && !firstPersonBidderRe.MatchString(clean) &&
		len(evidRefs) == 0 && len(kbRefs) == 0 && len(commitRefs) == 0
	if isQuotedReq {
		return SentenceClaim{
			SentenceIndex:   idx,
			ParagraphAnchor: anchor,
			Text:            rawSent,
			CleanText:       clean,
			ClaimType:       ClaimTypeTenderRestatement,
			Status:          ClaimStatusExempt,
		}
	}

	hasStrictFact := strictObjectiveFactRe.MatchString(clean)
	hasGeneralFact := bidderSelfCueRe.MatchString(clean) && generalBidderFactRe.MatchString(clean)
	hasModelFact := len(modelMentions) > 0
	hasCommitCue := len(commitRefs) > 0 || len(nonModelMentions) > 0 || commitmentCueRe.MatchString(clean)

	// 若型号断言带了 [EVID:...] 且未带 [COMMIT:...]，按 bidder_fact 校验；若带了 [COMMIT:...] 或无 [EVID:...] 且有承诺词，按 commitment 或 bidder_fact 校验。
	claimType := ClaimTypeGeneralNarrative
	switch {
	case len(commitRefs) > 0 || (hasCommitCue && !hasStrictFact && len(evidRefs) == 0):
		claimType = ClaimTypeCommitment
	case len(evidRefs) > 0 || len(kbRefs) > 0 || hasStrictFact || hasGeneralFact || hasModelFact:
		claimType = ClaimTypeBidderFact
	case hasCommitCue:
		claimType = ClaimTypeCommitment
	}

	claim := SentenceClaim{
		SentenceIndex:    idx,
		ParagraphAnchor:  anchor,
		Text:             rawSent,
		CleanText:        clean,
		ClaimType:        claimType,
		ExtractedMetrics: metricKeys,
		ExtractedEntities: entities,
	}

	switch claimType {
	case ClaimTypeGeneralNarrative:
		claim.Status = ClaimStatusExempt
		return claim

	case ClaimTypeCommitment:
		verifyCommitmentClaim(&claim, commitRefs, evidRefs, mentions, commitByID, commitByCode, factByID, factByCode, allowedCommit, allowedFact)
		return claim

	case ClaimTypeBidderFact:
		verifyBidderFactClaim(&claim, evidRefs, kbRefs, commitRefs, hasStrictFact, modelMentions, factByID, factByCode, kbByID, commitByID, commitByCode, allowedFact, allowedKB)
		return claim
	}

	claim.Status = ClaimStatusExempt
	return claim
}

func verifyCommitmentClaim(
	claim *SentenceClaim,
	commitRefs []string,
	evidRefs []string,
	mentions []Mention,
	commitByID map[int64]GroundedCommitmentSource,
	commitByCode map[string]GroundedCommitmentSource,
	factByID map[int64]GroundedFactSource,
	factByCode map[string]GroundedFactSource,
	allowedCommit map[int64]bool,
	allowedFact map[int64]bool,
) {
	if len(commitRefs) == 0 {
		// 特例：若只是设备型号承诺且绑定了可采设备证据 [EVID:...]，允许转由证据核验
		if len(evidRefs) > 0 && len(mentions) > 0 && allModelMentions(mentions) {
			claim.ClaimType = ClaimTypeBidderFact
			verifyBidderFactClaim(claim, evidRefs, nil, nil, false, mentions, factByID, factByCode, nil, commitByID, commitByCode, allowedFact, nil)
			return
		}
		claim.Status = ClaimStatusUngrounded
		claim.Reasons = append(claim.Reasons, "missing_commitment_binding: 承诺句缺少 [COMMIT:<id>] 台账绑定标记")
		return
	}

	var bound []GroundedCommitmentSource
	for _, ref := range commitRefs {
		c, ok := lookupCommit(ref, commitByID, commitByCode)
		if !ok {
			claim.Reasons = append(claim.Reasons, fmt.Sprintf("commitment_not_found: 未找到承诺台账项 %s", ref))
			continue
		}
		claim.CommitmentIDs = appendUniqueInt64(claim.CommitmentIDs, c.ID)
		if c.Status != "confirmed" && c.Status != "signed_off" {
			claim.Reasons = append(claim.Reasons, fmt.Sprintf("commitment_unconfirmed: 承诺台账项 %s 状态为 %s（须为 confirmed 或 signed_off）", c.CommitmentCode, c.Status))
			continue
		}
		if len(allowedCommit) > 0 && !allowedCommit[c.ID] {
			claim.Reasons = append(claim.Reasons, fmt.Sprintf("commitment_not_in_storyboard: 承诺台账项 %s 未在章节已批准故事板中声明", c.CommitmentCode))
			continue
		}
		bound = append(bound, c)
	}

	if len(bound) == 0 {
		claim.Status = ClaimStatusUngrounded
		return
	}

	// 核对每个已绑定台账项的承诺值与句中表述是否一致
	sentQs := Quantities(claim.CleanText)
	for _, c := range bound {
		if !commitmentMatchesSentence(c, claim.CleanText, mentions, sentQs) {
			claim.Reasons = append(claim.Reasons, fmt.Sprintf(
				"commitment_value_mismatch: 句中承诺内容与台账项 %s（%s=%s）不一致",
				c.CommitmentCode, c.Label, c.CommittedRaw,
			))
		}
	}

	// 若句子同时抽取出了多个具体承诺指标（如同时写了工期与质保期），每个非型号指标都应被已绑定台账覆盖
	for _, m := range mentions {
		if strings.HasPrefix(m.Metric, MetricModel+":") {
			continue
		}
		matched := false
		for _, c := range bound {
			cVal, cDim, hasVal := normalizedCommitmentValue(c)
			if metricMatchesCommitment(m.Metric, c) && (!hasVal || math.Abs(cVal-m.Value) <= 1e-6) {
				matched = true
				break
			}
			if hasVal && cDim == m.Dim && math.Abs(cVal-m.Value) <= 1e-6 {
				matched = true
				break
			}
		}
		if !matched {
			claim.Reasons = append(claim.Reasons, fmt.Sprintf(
				"unmatched_sentence_metric: 句中指标 %s（%s）未被绑定的承诺台账覆盖或数值不符",
				m.Label, m.Text,
			))
		}
	}

	if len(claim.Reasons) > 0 {
		claim.Status = ClaimStatusUngrounded
		return
	}
	claim.Status = ClaimStatusGrounded
}

func normalizedCommitmentValue(c GroundedCommitmentSource) (float64, string, bool) {
	if c.CommittedValue != nil {
		u := strings.TrimSpace(c.CommittedUnit)
		if u != "" {
			uDim, factor := unitDim(u)
			dim := c.Dimension
			if dim == "" {
				dim = uDim
			}
			return *c.CommittedValue * factor, dim, true
		}
		if qs := Quantities(c.CommittedRaw); len(qs) > 0 {
			dim := c.Dimension
			if dim == "" {
				dim = qs[0].Dim
			}
			return qs[0].Value, dim, true
		}
		return *c.CommittedValue, c.Dimension, true
	}
	if qs := Quantities(c.CommittedRaw); len(qs) > 0 {
		dim := c.Dimension
		if dim == "" {
			dim = qs[0].Dim
		}
		return qs[0].Value, dim, true
	}
	return 0, c.Dimension, false
}

func metricMatchesCommitment(mentionMetric string, c GroundedCommitmentSource) bool {
	if strings.EqualFold(c.CommitmentCode, mentionMetric) || strings.EqualFold(c.Category, mentionMetric) {
		return true
	}
	switch mentionMetric {
	case MetricDuration:
		return c.Category == "duration"
	case MetricAvailability:
		return c.Category == "availability"
	}
	return false
}

func commitmentMatchesSentence(c GroundedCommitmentSource, cleanText string, mentions []Mention, sentQs []Quantity) bool {
	cVal, cDim, hasVal := normalizedCommitmentValue(c)
	raw := strings.TrimSpace(c.CommittedRaw)
	if raw != "" && strings.Contains(strings.ToLower(cleanText), strings.ToLower(raw)) {
		// 即使原文包含字符串，若句中出现了同指标但不同数值的断言，也须拦截
		if hasVal {
			for _, m := range mentions {
				if metricMatchesCommitment(m.Metric, c) && math.Abs(m.Value-cVal) > 1e-6 {
					return false
				}
			}
		}
		return true
	}
	if hasVal {
		for _, m := range mentions {
			if (metricMatchesCommitment(m.Metric, c) || m.Dim == cDim) && math.Abs(m.Value-cVal) <= 1e-6 {
				return true
			}
		}
		for _, q := range sentQs {
			if (cDim == "" || q.Dim == cDim) && math.Abs(q.Value-cVal) <= 1e-6 {
				return true
			}
		}
		return false
	}
	return raw != "" && strings.Contains(strings.ToLower(cleanText), strings.ToLower(raw))
}

func verifyBidderFactClaim(
	claim *SentenceClaim,
	evidRefs []string,
	kbRefs []string,
	commitRefs []string,
	hasStrictFact bool,
	modelMentions []Mention,
	factByID map[int64]GroundedFactSource,
	factByCode map[string]GroundedFactSource,
	kbByID map[int64]GroundedKBSource,
	commitByID map[int64]GroundedCommitmentSource,
	commitByCode map[string]GroundedCommitmentSource,
	allowedFact map[int64]bool,
	allowedKB map[int64]bool,
) {
	// 若只有型号断言且绑定了 [COMMIT:...]，允许按承诺台账校验
	if len(evidRefs) == 0 && len(kbRefs) == 0 && len(commitRefs) > 0 && !hasStrictFact && len(modelMentions) > 0 {
		claim.ClaimType = ClaimTypeCommitment
		verifyCommitmentClaim(claim, commitRefs, evidRefs, modelMentions, commitByID, commitByCode, factByID, factByCode, nil, allowedFact)
		return
	}

	if len(evidRefs) == 0 && len(kbRefs) == 0 {
		claim.Status = ClaimStatusUngrounded
		claim.Reasons = append(claim.Reasons, "missing_evidence_binding: 投标人事实句缺少 [EVID:<id>] 或 [KB:<id>] 证据绑定标记")
		return
	}

	// 严格客观企业事实（资质证书、同类业绩、合同金额、社保证明等）必须绑定企业可采性证据 [EVID:<id>]，不能仅用通用知识库 [KB:<id>]。
	if hasStrictFact && len(evidRefs) == 0 {
		claim.Status = ClaimStatusUngrounded
		claim.Reasons = append(claim.Reasons, "strict_fact_requires_admissible_evidence: 资质/证书/业绩/合同/社保等客观事实必须绑定可采企业证据 [EVID:<id>]，不得仅绑定知识库 [KB:<id>]")
		return
	}

	var boundFacts []GroundedFactSource
	for _, ref := range evidRefs {
		f, ok := lookupFact(ref, factByID, factByCode)
		if !ok {
			claim.Reasons = append(claim.Reasons, fmt.Sprintf("evidence_not_found: 未找到企业证据 %s", ref))
			continue
		}
		claim.AdmissibilityItemIDs = appendUniqueInt64(claim.AdmissibilityItemIDs, f.AdmissibilityItemID)
		if f.Verdict != Admissible {
			claim.Reasons = append(claim.Reasons, fmt.Sprintf("evidence_not_admissible: 企业证据 %s（item=%d）可采性状态为 %s（须为 admissible）", f.Code, f.AdmissibilityItemID, f.Verdict))
			continue
		}
		if len(allowedFact) > 0 && !allowedFact[f.AdmissibilityItemID] {
			claim.Reasons = append(claim.Reasons, fmt.Sprintf("evidence_not_in_storyboard: 企业证据 %s（item=%d）未在章节已批准故事板中声明", f.Code, f.AdmissibilityItemID))
			continue
		}
		boundFacts = append(boundFacts, f)
	}

	var boundKBs []GroundedKBSource
	for _, ref := range kbRefs {
		id, err := strconv.ParseInt(ref, 10, 64)
		k, ok := kbByID[id]
		if err != nil || !ok {
			claim.Reasons = append(claim.Reasons, fmt.Sprintf("kb_evidence_not_found: 未找到知识库证据 %s", ref))
			continue
		}
		claim.RetrievalEvidenceIDs = appendUniqueInt64(claim.RetrievalEvidenceIDs, k.EvidenceID)
		if k.Status != "verified" || k.Relation != "supports" {
			claim.Reasons = append(claim.Reasons, fmt.Sprintf("kb_evidence_unverified: 知识库证据 %d 状态为 %s/%s（须为 verified/supports）", k.EvidenceID, k.Status, k.Relation))
			continue
		}
		if len(allowedKB) > 0 && !allowedKB[k.EvidenceID] {
			claim.Reasons = append(claim.Reasons, fmt.Sprintf("kb_evidence_not_in_storyboard: 知识库证据 %d 未在章节已批准故事板中声明", k.EvidenceID))
			continue
		}
		boundKBs = append(boundKBs, k)
	}

	if len(boundFacts) == 0 && len(boundKBs) == 0 {
		claim.Status = ClaimStatusUngrounded
		return
	}

	// 校验句中断言的具体证书号、型号、合同金额、社保月数与锚定关键词是否与绑定证据一致
	if len(boundFacts) > 0 {
		verifySentenceAgainstFacts(claim, boundFacts, modelMentions)
	}

	if len(claim.Reasons) > 0 {
		claim.Status = ClaimStatusUngrounded
		return
	}
	claim.Status = ClaimStatusGrounded
}

var contractAmtInSentRe = regexp.MustCompile(`(?:合同金额|合同额|项目金额|中标金额)[:：为]?\s*(\d+(?:\.\d+)?)\s*万元`)
var socialMonthsInSentRe = regexp.MustCompile(`(?:社保|养老保险)[^。；;0-9]{0,8}?(\d+)\s*个月|(\d+)\s*个月(?:以上)?(?:连续)?社保`)

func verifySentenceAgainstFacts(claim *SentenceClaim, facts []GroundedFactSource, modelMentions []Mention) {
	sentLower := strings.ToLower(claim.CleanText)

	// 1. 若句中声明了明确的证书编号，必须与至少一条绑定证据的 CertificateNo 一致
	if cm := certNoExtractRe.FindStringSubmatch(claim.CleanText); len(cm) > 1 {
		certNo := strings.ToUpper(strings.TrimSpace(cm[1]))
		matched := false
		for _, f := range facts {
			if strings.EqualFold(strings.TrimSpace(f.CertificateNo), certNo) {
				matched = true
				break
			}
		}
		if !matched {
			claim.Reasons = append(claim.Reasons, fmt.Sprintf("certificate_no_mismatch: 句中证书编号 %s 与绑定证据不一致", certNo))
		}
	}

	// 2. 若句中声明了设备型号，必须与至少一条绑定证据的 ModelSpec / Title / Scope 一致
	for _, mm := range modelMentions {
		modelLower := strings.ToLower(strings.TrimSpace(mm.Text))
		matched := false
		for _, f := range facts {
			blob := strings.ToLower(f.ModelSpec + " " + f.Title + " " + f.Scope)
			if strings.Contains(blob, modelLower) {
				matched = true
				break
			}
		}
		if !matched {
			claim.Reasons = append(claim.Reasons, fmt.Sprintf("model_spec_mismatch: 句中型号 %s 未在绑定证据中证实", mm.Text))
		}
	}

	// 3. 若句中声明了合同金额（万元），必须与至少一条绑定业绩证据的 ContractAmount 一致（误差 <= 0.01）
	if am := contractAmtInSentRe.FindStringSubmatch(claim.CleanText); len(am) > 1 {
		if val, err := strconv.ParseFloat(am[1], 64); err == nil {
			matched := false
			for _, f := range facts {
				if f.ContractAmount > 0 && (math.Abs(f.ContractAmount-val) <= 0.05 || math.Abs(f.ContractAmount/10000.0-val) <= 0.05) {
					matched = true
					break
				}
			}
			if !matched {
				claim.Reasons = append(claim.Reasons, fmt.Sprintf("contract_amount_mismatch: 句中合同金额 %s 万元与绑定证据不一致", am[1]))
			}
		}
	}

	// 4. 若句中声明了连续社保月数，不能超过绑定证据的 SocialSecurityMonths
	if sm := socialMonthsInSentRe.FindStringSubmatch(claim.CleanText); len(sm) > 0 {
		numStr := sm[1]
		if numStr == "" {
			numStr = sm[2]
		}
		if months, err := strconv.Atoi(numStr); err == nil && months > 0 {
			matched := false
			for _, f := range facts {
				if f.SocialSecurityMonths >= months {
					matched = true
					break
				}
			}
			if !matched {
				claim.Reasons = append(claim.Reasons, fmt.Sprintf("social_security_months_mismatch: 句中社保月数 %d 个月未获绑定证据支持", months))
			}
		}
	}

	// 5. 校验句中是否至少命中每条绑定证据的核心锚定词（防止挂羊头卖狗肉，如挂 ISO9001 证据却声称拥有 CMMI5）
	anyAnchorHit := false
	for _, f := range facts {
		if factAnchorMatchesSentence(f, sentLower) {
			anyAnchorHit = true
			break
		}
	}
	if !anyAnchorHit {
		claim.Reasons = append(claim.Reasons, "fact_content_unmatched: 句中事实表述与所绑定企业证据的名称/项目/范围/编号无交集")
	}
}

func factAnchorMatchesSentence(f GroundedFactSource, sentLower string) bool {
	candidates := []string{
		f.Title,
		f.ProjectName,
		f.CertificateNo,
		f.ModelSpec,
		f.QualificationLevel,
		f.ClientName,
		f.Code,
	}
	for _, c := range candidates {
		c = strings.ToLower(strings.TrimSpace(c))
		if c != "" && utf8.RuneCountInString(c) >= 2 && strings.Contains(sentLower, c) {
			return true
		}
	}
	// 退一步检查 Title / ProjectName / Scope 的关键词二元组命中率
	terms := keyTerms(Requirement{
		Title:       f.Title + " " + f.ProjectName,
		Description: f.Scope + " " + f.QualificationLevel + " " + f.ModelSpec,
	})
	if len(terms) == 0 {
		return true
	}
	hits := 0
	for _, t := range terms {
		if strings.Contains(sentLower, strings.ToLower(t)) {
			hits++
		}
	}
	return hits >= 1 && float64(hits)/float64(len(terms)) >= 0.25
}

func allModelMentions(ms []Mention) bool {
	for _, m := range ms {
		if !strings.HasPrefix(m.Metric, MetricModel+":") {
			return false
		}
	}
	return len(ms) > 0
}

// ExtractTenderSpecificTerms 从章节关联的招标要求与显式项目事实中抽取去重后的项目专属词元/数值。
func ExtractTenderSpecificTerms(reqs []Requirement, customFacts []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if utf8.RuneCountInString(s) < 2 {
			return
		}
		key := strings.ToLower(s)
		if !seen[key] {
			seen[key] = true
			out = append(out, s)
		}
	}
	for _, f := range customFacts {
		add(f)
	}
	for _, r := range reqs {
		for _, t := range keyTerms(r) {
			add(t)
		}
		for _, q := range Quantities(r.Title + " " + r.Description) {
			// 提取数值+单位部分（去掉比较词）
			if m := regexp.MustCompile(numPat + `\s*(个日历天|日历天|个工作日|工作日|个自然日|自然日|天|日|个星期|星期|周|个月|月|年|个小时|小时|h\b|H\b|分钟|min\b|%|％|人|名|台|套|块|个|路|节点|组)`).FindString(q.Raw); m != "" {
				add(strings.ReplaceAll(m, " ", ""))
			}
		}
	}
	return out
}

// ngramSimilarity 计算两段文本的字符 n-gram Jaccard 与包含度混合相似度（0~1）。
func ngramSimilarity(a, b string, n int) float64 {
	sa := runeNGrams(squash(strings.ToLower(a)), n)
	sb := runeNGrams(squash(strings.ToLower(b)), n)
	if len(sa) == 0 || len(sb) == 0 {
		return 0
	}
	inter := 0
	for k := range sa {
		if sb[k] {
			inter++
		}
	}
	union := len(sa) + len(sb) - inter
	if union <= 0 {
		return 0
	}
	jaccard := float64(inter) / float64(union)
	minLen := len(sa)
	if len(sb) < minLen {
		minLen = len(sb)
	}
	containment := float64(inter) / float64(minLen)
	// 取 Jaccard 与 0.85 * Containment 的较大值，既能识别整段照搬也能识别长段中大比例嵌入模板
	if containment*0.85 > jaccard {
		return containment * 0.85
	}
	return jaccard
}

func runeNGrams(s string, n int) map[string]bool {
	r := []rune(s)
	out := map[string]bool{}
	if len(r) < n {
		if len(r) > 0 {
			out[string(r)] = true
		}
		return out
	}
	for i := 0; i+n <= len(r); i++ {
		out[string(r[i:i+n])] = true
	}
	return out
}

func toInt64Set(ids []int64) map[int64]bool {
	if len(ids) == 0 {
		return nil
	}
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id > 0 {
			m[id] = true
		}
	}
	return m
}

func sortedKeys(m map[int64]bool) []int64 {
	if len(m) == 0 {
		return nil
	}
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func appendUniqueInt64(slice []int64, v int64) []int64 {
	for _, x := range slice {
		if x == v {
			return slice
		}
	}
	return append(slice, v)
}

func nonEmpty(a, fallback string) string {
	if strings.TrimSpace(a) != "" {
		return strings.TrimSpace(a)
	}
	return fallback
}

func round4(v float64) float64 {
	return math.Round(v*10000) / 10000
}

// StoryboardRequirementCoverageCheck 校验故事板声明的得分点/要求是否完整覆盖章节绑定的全部要求。
func StoryboardRequirementCoverageCheck(mappedReqCodes []string, storyboardReqCodes []string) (missing []string) {
	covered := make(map[string]bool, len(storyboardReqCodes))
	for _, c := range storyboardReqCodes {
		c = strings.TrimSpace(c)
		if c != "" {
			covered[c] = true
		}
	}
	for _, req := range mappedReqCodes {
		req = strings.TrimSpace(req)
		if req != "" && !covered[req] {
			missing = append(missing, req)
		}
	}
	return missing
}

// ValidateThemeSentence 校验故事板核心论点句（Theme Sentence）是否非空且具备实质长度（>= 12 个有效中英数字字符）。
func ValidateThemeSentence(theme string) bool {
	count := 0
	for _, r := range theme {
		if unicode.Is(unicode.Han, r) || unicode.IsLetter(r) || unicode.IsDigit(r) {
			count++
		}
	}
	return count >= 12
}
