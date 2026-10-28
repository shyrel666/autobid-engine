package ingest

import (
	"strings"
	"testing"
)

const sampleTender = `XX市政务云平台采购项目招标文件
招标编号：ZB-2026-001

第一章 招标公告
1 项目概况
本项目采购政务云平台服务，预算 3.50 万元/月。
1.1 资金来源
财政资金。
- 1 -
第二章 投标人须知
2.1 投标人资格要求
2.1.1 投标人须具有独立法人资格。
★2.1.2 投标人须具备信息系统集成一级资质，否则投标无效。
2.1.3 近三年完成过 2 个类似项目。
★ 项目经理须具备高级职称。
★ 投标有效期不少于 90 天。
第 2 页 共 3 页
第三章 评标办法
一、技术评分
3.1 技术方案（30 分）
二、商务评分
3.2 业绩（10 分）
1、每提供一个类似业绩得 2 分；
2、最高得 10 分。
`

func TestSplitHeadingsAndPages(t *testing.T) {
	secs := Split(sampleTender)
	byCode := map[string]Section{}
	var codes []string
	for _, s := range secs {
		byCode[s.Code] = s
		codes = append(codes, s.Code)
	}
	want := []string{"", "第一章", "1", "1.1", "第二章", "2.1", "2.1.1", "2.1.2", "2.1.3", "2.1★1", "2.1★2",
		"第三章", "一", "3.1", "二", "3.2"}
	if strings.Join(codes, "|") != strings.Join(want, "|") {
		t.Fatalf("codes\n got %q\nwant %q", codes, want)
	}
	if s := byCode[""]; s.Level != 0 || !strings.Contains(s.Content, "ZB-2026-001") {
		t.Fatalf("preamble: %+v", s)
	}
	if s := byCode["1"]; !strings.Contains(s.Content, "3.50 万元") || s.Level != 3 {
		t.Fatalf("amount must stay in body of section 1: %+v", s)
	}
	if s := byCode["第一章"]; s.Title != "招标公告" || s.Level != 1 || s.PageNo != 1 {
		t.Fatalf("chapter: %+v", s)
	}
	if s := byCode["第二章"]; s.PageNo != 2 {
		t.Fatalf("page after footer marker: %+v", s)
	}
	if s := byCode["第三章"]; s.PageNo != 3 {
		t.Fatalf("page after 第 2 页 marker: %+v", s)
	}
	if s := byCode["2.1.1"]; s.Mandatory || s.Title != "投标人须具有独立法人资格" || s.Level != 5 {
		t.Fatalf("2.1.1: %+v", s)
	}
	if s := byCode["2.1.2"]; !s.Mandatory || s.Level != 5 || !strings.HasPrefix(s.Content, "★2.1.2") {
		t.Fatalf("numbered star clause: %+v", s)
	}
	for _, c := range []string{"2.1★1", "2.1★2"} {
		if s := byCode[c]; !s.Mandatory || s.Level != 5 {
			t.Fatalf("unnumbered star clause %s: %+v", c, s)
		}
	}
	if s := byCode["3.2"]; !strings.Contains(s.Content, "1、每提供") || !strings.Contains(s.Content, "2、最高") {
		t.Fatalf("list items must stay in 3.2 body: %+v", s)
	}
	if strings.Contains(byCode["2.1★2"].Content, "第 2 页") {
		t.Fatal("page markers must be dropped from content")
	}
}

func TestSplitFormFeedPages(t *testing.T) {
	secs := Split("第一章 总则\n正文\n- 1 -\n\f第二章 资格\n\f\f第三章 评分\n")
	if len(secs) != 3 {
		t.Fatalf("sections: %+v", secs)
	}
	if secs[0].PageNo != 1 || secs[1].PageNo != 2 || secs[2].PageNo != 4 {
		t.Fatalf("pages: %d %d %d", secs[0].PageNo, secs[1].PageNo, secs[2].PageNo)
	}
	if strings.Contains(secs[0].Content, "- 1 -") {
		t.Fatal("page marker kept in content")
	}
}

func TestSplitMandatoryKeywords(t *testing.T) {
	secs := Split("1.1 报价\n报价超过最高限价的，作废标处理。\n1.2 付款\n按月付款。")
	if !secs[0].Mandatory || secs[1].Mandatory {
		t.Fatalf("mandatory flags: %+v", secs)
	}
}

func TestSplitRejectsNumbersThatAreNotClauses(t *testing.T) {
	for _, line := range []string{"3.5万元以内", "2024.06.30 截止", "1.5倍", "10 个工作日内完成", "0.5 分/项"} {
		if h, ok := classify(line, 0, false); ok {
			t.Errorf("%q classified as heading %+v", line, h)
		}
	}
	for _, line := range []string{"1.1投标人资格", "3．2．1 技术要求", "第十二章 附件", "第3节 评分", "四、合同条款"} {
		if _, ok := classify(line, 0, false); !ok {
			t.Errorf("%q should be a heading", line)
		}
	}
}

func TestSplitLongSection(t *testing.T) {
	var b strings.Builder
	b.WriteString("1.1 技术要求\n")
	for i := 0; i < 50; i++ {
		b.WriteString(strings.Repeat("服务器配置要求", 10) + "\n")
	}
	secs := Split(b.String())
	if len(secs) < 2 {
		t.Fatalf("expected long section to be split, got %d", len(secs))
	}
	for i, s := range secs {
		if s.Code != "1.1" || len([]rune(s.Content)) > MaxSectionRunes {
			t.Fatalf("part %d: code=%q runes=%d", i, s.Code, len([]rune(s.Content)))
		}
	}
	if !strings.Contains(secs[1].Title, "续1") {
		t.Fatalf("continuation title: %q", secs[1].Title)
	}
}

func TestEstimateTokens(t *testing.T) {
	if n := EstimateTokens("投标人资格 abcd efgh"); n != 5+2 {
		t.Fatalf("EstimateTokens = %d", n)
	}
}
