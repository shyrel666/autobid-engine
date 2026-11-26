package db

import (
	"reflect"
	"strings"
	"testing"
)

func TestComplexScoringWithSpanningCriteriaHeader(t *testing.T) {
	table := buildScoreTable([][]string{
		{"序号", "评审因素", "评审标准", "评审标准"},
		{"分值构成（100分）", "分值构成（100分）", "投标报价：35分 商务部分：25分 技术部分：40分", "投标报价：35分 商务部分：25分 技术部分：40分"},
		{"1", "投标报价分值（35分）", "最低报价与本报价之比乘以35", "最低报价与本报价之比乘以35"},
		{"2", "商务部分（25分）", "同类业绩（20分）", "最高得20分。"},
		{"", "", "仓储能力（5分）", "满足的得5分。"},
		{"3", "技术部分（40分）", "供货方案（40分）", "优良得40分。"},
	})
	items, _, handled := parseComplexScoringTable(table, nil)
	if !handled {
		t.Fatal("missed grouped rubric")
	}
	checkItems(t, items, []wantItem{
		{"投标报价分值", ScoreGroupPrice, 35}, {"同类业绩", ScoreGroupCommercial, 20},
		{"仓储能力", ScoreGroupCommercial, 5}, {"供货方案", ScoreGroupTechnical, 40},
	})
}

func TestComplexScoringCountsMergedHeadingOnceAndJoinsSplitWeight(t *testing.T) {
	table := buildScoreTable([][]string{
		{"评分内容", "评分因素", "评分标准"},
		{"一、投标报价（10分）", "投标报价（10分）", "低价优先。"},
		{"二、技术部分（76分）", "配送能力（70分）", "仓储设施。"},
		{"", "", "专用车辆。"},
		{"", "", "服务团队最多得8分。"},
		{"", "本地化服务（6", "本地化方案。"},
		{"", "分）", "措施完整得6分。"},
		{"三、商务部分（14分）", "售后服务方案（14分）", "方案完善的得14分。"},
	}, cellSpec{row: 2, col: 1, rowSpan: 2}, cellSpec{row: 6, page: 2})
	items, _, handled := parseComplexScoringTable(table, nil)
	if !handled {
		t.Fatal("missed grouped rubric")
	}
	checkItems(t, items, []wantItem{
		{"投标报价", ScoreGroupPrice, 10}, {"配送能力", ScoreGroupTechnical, 70},
		{"本地化服务", ScoreGroupTechnical, 6}, {"售后服务方案", ScoreGroupCommercial, 14},
	})
	if !reflect.DeepEqual(items[1].Rows, []int{2, 3, 4}) || !strings.Contains(items[2].Criteria, "措施完整得6分") {
		t.Fatalf("continuations missing: %+v; %+v", items[1], items[2])
	}
}

func TestComplexScoringWithConflictingPriceAndClauseAnnexContinuation(t *testing.T) {
	start := buildScoreTable([][]string{
		{"条款号", "条款号", "条款号", "评审因素", "评审标准"},
		{"2.1", "2.1", "符合性审查", "报价唯一", "只能有一个有效报价。"},
		{"2.2.1", "2.2.1", "2.2.1", "分值构成（总分100分）", "报价得分：30分 技术部分：40分 商务部分：40分"},
		{"条款号", "条款号", "条款号", "评分因素", "评分标准"},
		{"2.2.2（1）", "报价得分（20分）", "报价得分（20分）", "投标报价评分标准", "价格扣减说明。"},
	})
	start.PageStart = 1
	price, next, handled := parseComplexScoringTable(start, nil)
	if !handled || next == nil {
		t.Fatal("missed evaluation annex")
	}
	checkItems(t, price, []wantItem{{"报价得分", ScoreGroupPrice, 0}})
	if !reflect.DeepEqual(price[0].ScoreConflict, []float64{20, 30}) {
		t.Fatalf("unresolved source values missing: %+v", price[0])
	}
	continued := buildScoreTable([][]string{
		{"", "", "", "报价得分=(评标基准价／评标报价)×20"},
		{"2.2.2（2）", "技术部分（40分）", "服务流程：25分", "流程完整得25分。"},
		{"", "", "工作沟通机制：15分", "机制健全得15分。"},
		{"2.2.2（3）", "商务部分（40分）", "类似项目业绩：40分", "最高得40分。"},
	}, cellSpec{row: 0, page: 2}, cellSpec{row: 1, page: 2}, cellSpec{row: 2, page: 2}, cellSpec{row: 3, page: 2})
	continued.PageStart = 2
	details, _, handled := parseComplexScoringTable(continued, next)
	if !handled || !strings.Contains(price[0].Criteria, "×20") || !reflect.DeepEqual(price[0].Rows, []int{4}) {
		t.Fatalf("bad continuation evidence: %+v", price[0])
	}
	checkItems(t, details, []wantItem{
		{"服务流程", ScoreGroupTechnical, 25}, {"工作沟通机制", ScoreGroupTechnical, 15},
		{"类似项目业绩", ScoreGroupCommercial, 40},
	})
}
