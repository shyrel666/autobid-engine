package db

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTaskReportMarkdown(t *testing.T) {
	pct, score := 0.5, 30.0
	rep := &TaskReport{
		Task: &Task{ID: 7, Name: "政务云审查", Status: "done"}, Goal: "完成资格审查", GeneratedAt: time.Now(),
		Coverage: Coverage{Target: 4, Tested: 2, Pct: &pct},
		Summary: ReportSummary{Documents: 1, Elements: 3, MandatoryElements: 1, MandatoryVerified: 1, Findings: 1,
			ElementsByType: map[string]int{AssetQualification: 1, AssetScoringItem: 1, AssetTender: 1},
			BySeverity:     map[string]int{SeverityHigh: 1}, ByCategory: map[string]int{FindingCompliance: 1}},
		Documents: []*TenderDocument{{ID: 3, Title: "招标文件", TenderNo: "ZB-1", ChunkCount: 7}},
		Elements: []ReportElement{
			{ReportAnchor: ReportAnchor{AssetID: 1, Type: AssetQualification, Code: "1.1.2", Title: "集成|一级", IsMandatory: true,
				SourceRef: json.RawMessage(`{"chunk_id":5}`)}, Verified: true, FactIDs: []int64{11}},
			{ReportAnchor: ReportAnchor{AssetID: 2, Type: AssetScoringItem, Code: "2.1", Title: "技术方案"}, ScoreMax: &score},
		},
		Findings: []ReportFinding{{Finding: &Finding{ID: 1, Category: FindingCompliance, Severity: SeverityHigh, Title: "资质满足",
			Summary: "具备一级资质"}, Evidence: []ReportEvidence{{NodeID: 11, Kind: KindFact, State: "confirmed", Summary: "核验通过",
			Evidence: "证书编号 X"}}}},
	}
	md := rep.Markdown()
	for _, want := range []string{"# 标书合规审查报告：政务云审查", "| 要素覆盖率 | 50.0%（2 / 4） |", "| 强制性条款核验 | 1 / 1 |",
		`集成\|一级`, "✅ 已核验", "| 2.1 | 技术方案 |  | 30 | ⚠️ 待核验 | — |", "### 合规性（1）", "#### [高] 资质满足",
		"- 证据 #11（fact/confirmed）：核验通过", "  - 依据：证书编号 X", "结论严重度分布：高 1"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}
}
