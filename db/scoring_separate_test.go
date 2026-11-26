package db

import (
	"strings"
	"testing"
)

func TestSeparateScoringDetailedHeader(t *testing.T) {
	table := buildScoreTable([][]string{
		{"评审因素", "评审因素", "评审标准", "评审标准"},
		{"分值构成", "分值构成", "技术部分75.00分商务部分10.00分报价得分15.00分", ""},
		{"评审因素分类", "评审项", "详细描述", "分值"},
		{"技术评审", "实施方案", "方案充分得45分", "45.00"},
		{"", "", "方案欠缺得零分", ""},
		{"", "人员配置", "提供人员方案得30分", "30.00"},
		{"商务评审", "项目业绩", "每项2分，最多10分", "10.00"},
		{"价格分", "投标价格得分", "按低价优先法计分", "15.00"},
	})
	items, _, handled := parseSeparateScoringTable(table, scoringCaption{}, "详细评审")
	if !handled {
		t.Fatal("did not recognize detailed score table")
	}
	checkItems(t, items, []wantItem{
		{"实施方案", ScoreGroupTechnical, 45}, {"人员配置", ScoreGroupTechnical, 30},
		{"项目业绩", ScoreGroupCommercial, 10}, {"投标价格得分", ScoreGroupPrice, 15},
	})
	if items[0].Rows[0] != 3 || !strings.Contains(items[0].Criteria, "方案欠缺得零分") || items[1].GroupWeight != 75 || items[2].GroupWeight != 10 || items[3].GroupWeight != 15 {
		t.Fatalf("declarations or source rows lost: %+v", items)
	}
}

func TestSeparateScoringDistinguishesMonthlyDeductions(t *testing.T) {
	table := buildScoreTable([][]string{
		{"类别", "项目", "规定分值", "评分标准", "扣分"},
		{"履约服务", "工作巡查", "20分", "每发现一次扣2分", ""},
	})
	if items, carry, handled := parseSeparateScoringTable(table, scoringCaption{}, "验收标准与要求（续一）"); !handled || len(items) != 0 || carry != nil {
		t.Fatalf("monthly deductions must stay outside bid scoring: %v %v %v", items, carry, handled)
	}
	if _, _, handled := parseSeparateScoringTable(table, scoringCaption{}, "评标细则"); handled {
		t.Fatal("deduction column alone must not exclude a bid scoring table")
	}
}

func TestSeparateScoringUseExplicitCaptionAndFormula(t *testing.T) {
	commercial := separateScoringCaption("注：以上审查通过后计分。\n附表三：商务评分细则表（30分）")
	items := []*ScoredItem{{Name: "培训方案", ScoreGroup: ScoreGroupTechnical, Score: 5}}
	applyScoringCaption(items, commercial)
	if commercial.group != ScoreGroupCommercial || items[0].ScoreGroup != ScoreGroupCommercial || items[0].GroupWeight != 30 {
		t.Fatalf("caption not applied: %+v %+v", commercial, items[0])
	}
	price := separateScoringCaption("附表五价格评分表（价格分值：20分）")
	formula := "最低有效报价为评标基准价。\n投标人价格得分＝（评标基准价/投标报价）×价格分值"
	table := buildScoreTable([][]string{{"评分因素", "评分标准", "分值"}, {"价格", formula, "20"}})
	if items, _, ok := parseSeparateScoringTable(table, price, "价格评审"); !ok || len(items) != 1 || items[0].Score != 20 || items[0].ScoreGroup != ScoreGroupPrice {
		t.Fatalf("independent price table lost: %v %v", items, ok)
	}
	source := &chunkSource{Content: "附表五价格评分表（价格分值：20分）\n" + formula}
	if item := separatePriceFormula(source, nil); item == nil || item.Score != 20 || item.ScoreGroup != ScoreGroupPrice {
		t.Fatalf("standalone price formula lost: %+v", item)
	}
	source.Content = "附表五价格评分表（价格分值：20分）\n只有价格权重说明。"
	if item := separatePriceFormula(source, nil); item != nil {
		t.Fatalf("generated an item from declaration alone: %+v", item)
	}
	source.SectionTitle, source.Content = "价格评价：（精确到小数点后两位）", formula
	weights := buildScoreTable([][]string{{"评分项目", "技术评分", "商务评分", "价格评分"}, {"分 值", "60分", "30分", "10分"}})
	if item := separatePriceFormula(source, []scoreTable{*weights}); item == nil || item.Score != 10 {
		t.Fatalf("did not read horizontal price weight: %+v", item)
	}
}
