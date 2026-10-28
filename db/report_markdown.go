package db

// 推演报告的 Markdown 渲染（GET /api/tasks/{id}/report?format=markdown）。

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

var (
	assetTypeLabels = map[string]string{AssetTender: "招标项目", AssetLot: "标段", AssetClause: "条款",
		AssetQualification: "资格要求", AssetScoringItem: "评分项", AssetCommercialItem: "商务项"}
	findingCategoryLabels = map[string]string{FindingCompliance: "合规性", FindingRisk: "风险",
		FindingScoring: "评分策略", FindingPricing: "报价", FindingStrategy: "投标策略"}
	severityLabels = map[string]string{SeverityCritical: "致命", SeverityHigh: "高", SeverityMedium: "中",
		SeverityLow: "低", SeverityInfo: "提示"}
	findingCategoryOrder = []string{FindingCompliance, FindingRisk, FindingScoring, FindingPricing, FindingStrategy}
)

func labelOf(m map[string]string, k string) string {
	if v, ok := m[k]; ok {
		return v
	}
	return k
}

// mdCell 转义 Markdown 表格单元格中的竖线与换行。
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\r\n", " ")
	return strings.ReplaceAll(s, "\n", " ")
}

func idList(ids []int64) string {
	if len(ids) == 0 {
		return "—"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprintf("#%d", id)
	}
	return strings.Join(parts, ", ")
}

func anchorLabel(a ReportAnchor) string {
	s := labelOf(assetTypeLabels, a.Type)
	if a.Code != "" {
		s += " " + a.Code
	}
	if a.Title != "" {
		s += " " + a.Title
	}
	if a.IsMandatory {
		s = "★ " + s
	}
	return s
}

func refText(raw []byte) string {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		return "—"
	}
	return "`" + strings.ReplaceAll(string(raw), "`", "'") + "`"
}

// Markdown 把报告渲染为面向投标人员的 Markdown 审查报告。
func (r *TaskReport) Markdown() string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	s := r.Summary

	w("# 标书合规审查报告：%s\n\n", r.Task.Name)
	w("- 任务编号：#%d\n- 任务状态：%s\n- 审查目标：%s\n- 生成时间：%s\n\n", r.Task.ID, r.Task.Status, r.Goal,
		r.GeneratedAt.In(time.Local).Format("2006-01-02 15:04:05"))

	w("## 一、总体指标\n\n| 指标 | 数值 |\n| --- | --- |\n")
	cov := "—"
	if r.Coverage.Pct != nil {
		cov = fmt.Sprintf("%.1f%%", *r.Coverage.Pct*100)
	}
	w("| 要素覆盖率 | %s（%d / %d） |\n", cov, r.Coverage.Tested, r.Coverage.Target)
	w("| 招标文档 | %d |\n| 招标要素 | %d |\n", s.Documents, s.Elements)
	w("| 强制性条款核验 | %d / %d |\n", s.MandatoryVerified, s.MandatoryElements)
	w("| 已确认事实 | %d |\n| 审查结论 | %d |\n| 目标达成 | %d / %d |\n\n", s.Facts, s.Findings, s.GoalsMet, s.GoalsTotal)
	if len(s.BySeverity) > 0 {
		w("结论严重度分布：")
		var parts []string
		for _, k := range []string{SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow, SeverityInfo} {
			if n := s.BySeverity[k]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", severityLabels[k], n))
			}
		}
		w("%s\n\n", strings.Join(parts, "，"))
	}

	w("## 二、招标文档\n\n")
	if len(r.Documents) == 0 {
		w("暂无招标文档。\n\n")
	} else {
		w("| 文档 | 招标编号 | 标段 | 章节数 |\n| --- | --- | --- | --- |\n")
		for _, d := range r.Documents {
			w("| #%d %s | %s | %s | %d |\n", d.ID, mdCell(d.Title), mdCell(d.TenderNo), mdCell(d.LotNo), d.ChunkCount)
		}
		w("\n")
	}

	verified := func(ok bool) string {
		if ok {
			return "✅ 已核验"
		}
		return "⚠️ 待核验"
	}
	w("## 三、强制性条款核验清单\n\n")
	var mandatory, scoring []ReportElement
	for _, el := range r.Elements {
		if el.IsMandatory {
			mandatory = append(mandatory, el)
		}
		if el.Type == AssetScoringItem {
			scoring = append(scoring, el)
		}
	}
	if len(mandatory) == 0 {
		w("范围内暂无强制性条款要素。\n\n")
	} else {
		w("| 编号 | 条款 | 类型 | 状态 | 事实证据 | 原文出处 |\n| --- | --- | --- | --- | --- | --- |\n")
		for _, el := range mandatory {
			w("| %s | %s | %s | %s | %s | %s |\n", mdCell(el.Code), mdCell(el.Title), labelOf(assetTypeLabels, el.Type),
				verified(el.Verified), idList(el.FactIDs), mdCell(refText(el.SourceRef)))
		}
		w("\n")
	}

	w("## 四、评分项\n\n")
	if len(scoring) == 0 {
		w("范围内暂无评分项要素。\n\n")
	} else {
		w("| 编号 | 评分项 | 分类 | 分值 | 状态 | 关联结论 |\n| --- | --- | --- | --- | --- | --- |\n")
		for _, el := range scoring {
			score := "—"
			if el.ScoreMax != nil {
				score = fmt.Sprintf("%g", *el.ScoreMax)
			}
			w("| %s | %s | %s | %s | %s | %s |\n", mdCell(el.Code), mdCell(el.Title), mdCell(el.Category), score,
				verified(el.Verified), idList(el.FindingIDs))
		}
		w("\n")
	}

	w("## 五、要素分布\n\n")
	if len(s.ElementsByType) == 0 {
		w("暂无要素。\n\n")
	} else {
		types := make([]string, 0, len(s.ElementsByType))
		for k := range s.ElementsByType {
			types = append(types, k)
		}
		sort.Strings(types)
		w("| 类型 | 数量 |\n| --- | --- |\n")
		for _, k := range types {
			w("| %s | %d |\n", labelOf(assetTypeLabels, k), s.ElementsByType[k])
		}
		w("\n")
	}

	w("## 六、审查结论与证据链\n\n")
	if len(r.Findings) == 0 {
		w("暂无审查结论。\n")
		return b.String()
	}
	groups := map[string][]ReportFinding{}
	cats := append([]string{}, findingCategoryOrder...)
	for _, f := range r.Findings {
		if _, known := findingCategoryLabels[f.Category]; !known && len(groups[f.Category]) == 0 {
			cats = append(cats, f.Category)
		}
		groups[f.Category] = append(groups[f.Category], f)
	}
	for _, c := range cats {
		fs := groups[c]
		if len(fs) == 0 {
			continue
		}
		w("### %s（%d）\n\n", labelOf(findingCategoryLabels, c), len(fs))
		for _, f := range fs {
			w("#### [%s] %s\n\n", labelOf(severityLabels, f.Severity), f.Title)
			if f.Summary != "" {
				w("%s\n\n", f.Summary)
			}
			for _, a := range f.Anchors {
				w("- 关联要素：%s\n", anchorLabel(a))
			}
			for _, ref := range f.SourceRefs {
				w("- 原文出处：%s\n", refText(ref))
			}
			if len(f.Evidence) == 0 {
				w("- 证据：无\n")
			}
			for _, ev := range f.Evidence {
				line := fmt.Sprintf("- 证据 #%d（%s/%s）：%s", ev.NodeID, ev.Kind, ev.State, ev.Summary)
				if ev.Inherited {
					line += "（继承自来源任务）"
				}
				w("%s\n", line)
				if ev.Evidence != "" {
					w("  - 依据：%s\n", strings.ReplaceAll(ev.Evidence, "\n", " "))
				}
				if ref := refText(ev.SourceRef); ref != "—" {
					w("  - 出处：%s\n", ref)
				}
			}
			w("\n")
		}
	}
	return b.String()
}
