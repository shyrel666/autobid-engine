package db

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func table(rows ...[]string) *scoreTable { return &scoreTable{Grid: rows, Rows: len(rows)} }

func TestParseReviewTableLayouts(t *testing.T) {
	// 重庆版式：序号纵向合并，两个「检查因素」列（类别 + 因素）。
	cq := parseReviewTable(table(
		[]string{"序号", "检查因素", "检查因素", "检查内容"},
		[]string{"（一）", "《中华人民共和国政府采购法》第二十二条规定", "1.具有独立承担民事责任的能力", "1.投标人法人营业执照（副本）"},
		[]string{"（一）", "《中华人民共和国政府采购法》第二十二条规定", "6.法律、行政法规规定的其他条件", "/"},
		[]string{"（三）", "投标保证金", "投标保证金", "按照招标文件要求足额交纳所投包的投标保证金。"},
	), "（一）资格审查 资格审查资料表如下：")
	if len(cq) != 3 || cq[0].Kind != VetoQualification || cq[0].Group == "" || stripNumbering(cq[0].Factor) != "具有独立承担民事责任的能力" {
		t.Fatalf("chongqing qualification: %+v", cq)
	}
	if d := cq[1].description(); strings.Contains(d, "审查内容") {
		t.Fatalf("a bare \"/\" is not review content: %q", d)
	}
	// 中央版式：层级序号，只引用其他章节的父行跳过；无子项的引用行保留。
	cen := parseReviewTable(table(
		[]string{"序号", "审查因素", "审查内容", "格式要求"},
		[]string{"1", "满足《中华人民共和国政府采购法》第二十二条规定", "具体规定见第一章《投标邀请》", ""},
		[]string{"1-1", "营业执照等证明文件", "投标人为企业的，应提供有效的营业执照", "提供证明文件复印件"},
		[]string{"1-2", "投标人资格声明书", "提供了符合要求的《投标人资格声明书》。", "格式见《投标文件格式》"},
		[]string{"2", "本项目的特定资格要求", "如有，见第一章《投标邀请》", ""},
	), "二、资格审查要求")
	if len(cen) != 3 || cen[0].Seq != "1-1" || cen[2].Seq != "2" || !strings.Contains(cen[0].description(), "格式要求：提供证明文件复印件") {
		t.Fatalf("central hierarchical: %+v", cen)
	}
	// 符合性审查：表头「评审因素 / 评审标准」，前文同时提到资格时以符合性为准。
	conf := parseReviewTable(table(
		[]string{"序号", "评审因素", "评审因素", "评审标准"},
		[]string{"1", "有效性审查", "投标文件签署或盖章", "投标文件上法定代表人或其授权代表的签署齐全"},
		[]string{"1", "有效性审查", "报价唯一", "只能在预算金额和最高限价内报价"},
	), "评标委员会应当对符合资格的投标人的投标文件进行符合性审查")
	if len(conf) != 2 || conf[0].Kind != VetoConformity || conf[1].Factor != "报价唯一" {
		t.Fatalf("conformity: %+v", conf)
	}
	// 表头本身写明「资格审查因素」；序号与因素都为空的续行并入上一项。
	cont := parseReviewTable(table(
		[]string{"序号", "资格审查因素", "说明与要求"},
		[]string{"7", "投标人身份证明及授权", "（1）法定代表人身份证明"},
		[]string{"", "", "（2）单位负责人身份证明"},
	), "否则将承担其投标被视为无效投标的风险。")
	if len(cont) != 1 || cont[0].Kind != VetoQualification || !strings.Contains(cont[0].Criteria, "（2）单位负责人") || len(cont[0].Rows) != 2 {
		t.Fatalf("continuation: %+v", cont)
	}
	// 不是审查标准的表格：投标文件格式中的承诺表、函件模板、评分表。
	for name, tb := range map[string]*scoreTable{
		"response form": table([]string{"序号", "项     目", "投标人承诺"}, []string{"1", "具有有效营业执照", "是"}),
		"letter":        table([]string{"具体情形", "□低于全部通过符合性审查供应商报价平均值的50%"}, []string{"中央国家机关政府采购中心：", "说明"}),
		"scoring":       table([]string{"序号", "评审因素", "分值", "评分标准"}, []string{"1", "资格证书", "5", "提供得5分"}),
	} {
		if items := parseReviewTable(tb, "资格审查 符合性审查"); len(items) != 0 {
			t.Fatalf("%s parsed as review table: %+v", name, items)
		}
	}
}

// 审查表一般对全部分包适用：只有紧邻表格的文字唯一点名一个分包时才归入该分包（真实语料中曾把全项目审查表误归入包1）。
func TestReviewLotAndRepeatedFactors(t *testing.T) {
	for text, want := range map[string]string{"包1保证金汇至以下账户": "1", "包1、包2 均适用本审查表": "", "（一）资格审查": "", "包2资格审查表": "2"} {
		if got := singleLotMarker(text); got != want {
			t.Fatalf("singleLotMarker(%q) = %q, want %q", text, got, want)
		}
	}
	items := parseReviewTable(table(
		[]string{"序号", "评审因素", "评审因素", "评审标准"},
		[]string{"3", "技术部分", "投标文件内容", "本招标文件第二篇全部内容作响应或承诺"},
		[]string{"4", "商务部分", "投标文件内容", "本招标文件第三篇全部内容作响应或承诺"},
		[]string{"5", "有效性审查", "报价唯一", "只能有一个有效报价"},
	), "（二）符合性审查")
	if len(items) != 3 || items[0].title(true) != "【符合性】技术部分·投标文件内容" || items[1].title(true) != "【符合性】商务部分·投标文件内容" ||
		items[2].title(false) != "【符合性】报价唯一" {
		t.Fatalf("titles: %+v", items)
	}
}

func TestParseBidDeadlineRealFormats(t *testing.T) {
	at := func(y, mo, d, h, mi int) time.Time { return time.Date(y, time.Month(mo), d, h, mi, 0, 0, beijing) }
	for line, want := range map[string]time.Time{
		"（七）投标截止时间：2025年3月25日北京时间10时00分；":              at(2025, 3, 25, 10, 0),
		"（六）投标截止时间：2026年5月20日北京时间10:00":                 at(2026, 5, 20, 10, 0),
		"投标截止及开标时间为2026-05-29 09:00:00。":                  at(2026, 5, 29, 9, 0),
		"投标截止时间、开标时间：2026年8月20日9点30分（北京时间）。":            at(2026, 8, 20, 9, 30),
		"1.投标截止及开标时间：2026年9月1日8时30分（北京时间），逾期提交或不符合规定的": at(2026, 9, 1, 8, 30),
		"提交投标文件截止时间：2026年7月8日09点00分（北京时间）":              at(2026, 7, 8, 9, 0),
		"投标文件递交截止时间：2026年6月3日下午2时30分":                  at(2026, 6, 3, 14, 30),
	} {
		got, ok := ParseBidDeadline(line)
		if !ok || !got.Equal(want) {
			t.Fatalf("%s: got %v %v want %v", line, got, ok, want)
		}
	}
	for _, line := range []string{
		"2.3投标保证金保函单独密封，随投标文件一并递交，投标保证金保函递交截止时间同投标截止时间。",
		"（四）超过投标截止时间递交的投标文件，恕不接收。",
		"七、接受投标时间、投标截止时间及开标时间",
		"澄清或者修改的内容可能影响投标文件编制的，应当在投标截止时间至少15日前",
		"获取招标文件截止时间：2026年8月10日17时00分",
		"（八）开标时间：2026年8月26日北京时间10时00分；",
	} {
		if got, ok := ParseBidDeadline(line); ok {
			t.Fatalf("%s: unexpected deadline %v", line, got)
		}
	}
}

func TestFindBidDeadlineInHeadingAndSchedule(t *testing.T) {
	want := time.Date(2026, 3, 12, 9, 30, 0, 0, beijing)
	check := func(cs, next *chunkSource, sourceID int64, page int) {
		t.Helper()
		got, ok := findChunkBidDeadline(cs, next)
		if !ok || !got.at.Equal(want) || got.chunk.ChunkID != sourceID || got.page != page {
			t.Fatalf("deadline = %+v, %t, want %s from chunk %d page %d", got, ok, want, sourceID, page)
		}
	}
	check(&chunkSource{ChunkID: 1, Loc: map[string]any{"page_start": 4},
		Content: "四、投标文件提交\n截止时间：2026年03月12日09时30分（北京时间）"}, nil, 1, 4)
	heading := &chunkSource{DocID: 2, Content: "4. 售价：0元\n四、投标截止时间及地点："}
	value := &chunkSource{DocID: 2, ChunkID: 3, Loc: map[string]any{"page_start": 6},
		Content: "4.1 时间：2026-03-12 09:30\n地点：政务服务中心"}
	check(heading, value, 3, 6)
	schedule := &chunkSource{ChunkID: 4, Content: "| 步骤名称 | 开始时间 | 截止时间 |",
		Tables: []byte(`[{"page_start":2,"cells":[{"row":2,"col":2,"page":3}],"grid":[
			["步骤名称","开始时间","截止时间"],
			["获取招标文件","2026年3月1日09:00","2026年3月5日09:00"],
			["网上应答（上传电子投标文件）","2026年3月6日09:00","2026年3月12日09:30"],
			["开标解密","2026年3月12日09:30","2026年3月12日10:30"]]}]`)}
	check(schedule, nil, 4, 3)
	for _, content := range []string{
		"三、获取招标文件\n时间：2026年03月12日09:30",
		"五、开标时间及地点\n时间：2026年03月12日09:30",
		"四、投标截止时间及地点\n时间：详见公告\n发布时间：2026年03月12日09:30",
		"四、投标截止时间及地点\n2026年 月 日 时 分00秒（北京时间）",
		"四、投标截止时间及地点\n时间：2026年02月31日09:30",
	} {
		if got, ok := findChunkBidDeadline(&chunkSource{Content: content}, nil); ok {
			t.Errorf("unexpected deadline %s from %q", got.at, content)
		}
	}
	value.DocID = 3
	if _, ok := findChunkBidDeadline(heading, value); ok {
		t.Fatal("deadline heading must not borrow a value from another document")
	}
}

func TestDocPrecedenceAndLots(t *testing.T) {
	t1 := sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, beijing), Valid: true}
	t2 := sql.NullTime{Time: time.Date(2026, 1, 8, 0, 0, 0, 0, beijing), Valid: true}
	tender := deadlineDoc{id: 1, role: DocRoleTender, published: t1}
	correction := deadlineDoc{id: 2, role: DocRoleCorrection, published: t2}
	if !laterDoc(correction, tender) || laterDoc(tender, correction) {
		t.Fatal("later publication must win")
	}
	undated := deadlineDoc{id: 3, role: DocRoleClarification}
	if !laterDoc(undated, deadlineDoc{id: 4, role: DocRoleTender}) {
		t.Fatal("a clarification without a date still follows the tender")
	}
	if got := normalizeLots([]string{"包2", "第一包", " A包 ", "2"}); strings.Join(got, ",") != "1,2,A" {
		t.Fatalf("normalizeLots = %v", got)
	}
	if outOfBid("", []string{"1"}) || outOfBid("2", nil) || outOfBid("1", []string{"1"}) || !outOfBid("2", []string{"1"}) {
		t.Fatal("outOfBid")
	}
	if GuessDocRole("XX项目更正公告") != DocRoleCorrection || GuessDocRole("招标文件（定稿）") != DocRoleTender {
		t.Fatal("GuessDocRole")
	}
}

func TestStatutoryVetoCoverage(t *testing.T) {
	rows := []reqCandidate{
		{code: "VQ-1", vetoKind: VetoQualification, title: "【资格】投标保证金", description: "审查内容：足额交纳投标保证金"},
		{code: "VQ-2", vetoKind: VetoQualification, title: "【资格】营业执照"},
		{code: "VC-1", vetoKind: VetoConformity, title: "【符合性】投标文件签署或盖章"},
	}
	got := map[string][]string{}
	for _, c := range statutoryCandidates(rows) {
		if c.category != ReqVetoItem || c.vetoKind != VetoStatutory || !c.mandatory {
			t.Fatalf("statutory candidate %+v", c)
		}
		got[c.code] = c.loc["covered_by"].([]string)
	}
	if len(got) != 6 || strings.Join(got["VS-63-1"], ",") != "VQ-1" || strings.Join(got["VS-63-3"], ",") != "VQ-1,VQ-2" ||
		strings.Join(got["VS-63-2"], ",") != "VC-1" || len(got["VS-37"]) != 0 {
		t.Fatalf("covered_by = %v", got)
	}
}

func TestParseScoringTiersAndProofRules(t *testing.T) {
	// 1. 多档次主观评分项
	tiers, kind, minScore := ParseScoringTiers(
		"优：总体架构设计完整、高可用方案科学合理，得[12,15]分；良：方案基本完整，得[8,12)分；差：方案存在缺漏，得[0,8)分。",
		15, ScoreGroupTechnical,
	)
	if kind != EvalSubjective || minScore != 0 || len(tiers) < 3 {
		t.Fatalf("subjective tiers: kind=%q min=%v tiers=%+v", kind, minScore, tiers)
	}
	if tiers[0].Label != "优" || tiers[0].ScoreMax != 15 || tiers[0].ScoreMin != 12 {
		t.Fatalf("first tier: %+v", tiers[0])
	}

	// 2. 客观计件评分项 + 证明规则抽取
	objText := "投标人自2023年1月1日以来每提供一个同类政务云平台项目业绩得3分，最高得9分；须提供合同首页、金额页、签字盖章页及竣工验收报告复印件，且须附发票，不接受母公司或关联公司业绩。"
	objTiers, objKind, _ := ParseScoringTiers(objText, 9, ScoreGroupCommercial)
	if objKind != EvalObjective || len(objTiers) == 0 {
		t.Fatalf("objective tiers: kind=%q tiers=%+v", objKind, objTiers)
	}
	pr := ParseProofRules(objText, ReqScoringItem, ScoreGroupCommercial)
	if pr.IsZero() || !pr.NeedAcceptance || !pr.BidderOnly || len(pr.ContractPages) < 3 || pr.RawSummary == "" {
		t.Fatalf("proof rules: %+v", pr)
	}

	// 3. 人员社保月数与截止日有效
	staffText := "拟派项目经理具有PMP证书，须提供近6个月投标人为其缴纳的社保证明，证书须在投标截止日有效。"
	staffPR := ParseProofRules(staffText, ReqScoringItem, ScoreGroupTechnical)
	if staffPR.IsZero() || staffPR.SocialSecurityMonths != 6 || !staffPR.ValidOnBidDeadline {
		t.Fatalf("staff proof rules: %+v", staffPR)
	}

	// 4. 价格分公式
	priceTiers, priceKind, _ := ParseScoringTiers("投标报价得分=(评标基准价/投标报价)×30×100%", 30, ScoreGroupPrice)
	if priceKind != EvalObjective || len(priceTiers) == 0 || priceTiers[0].Kind != "formula" {
		t.Fatalf("price tiers: kind=%q tiers=%+v", priceKind, priceTiers)
	}
}
