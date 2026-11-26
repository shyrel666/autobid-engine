package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
)

const schemaVersion = 1

type reviewPlan struct {
	Version    int       `json:"version"`
	ExerciseID string    `json:"exercise_id"`
	MarginPP   *float64  `json:"noninferiority_margin_pp"`
	Reviewers  []string  `json:"reviewers"`
	Projects   []project `json:"projects"`
}

type project struct {
	ID       string      `json:"id"`
	Family   string      `json:"family"`
	DraftIDs []string    `json:"draft_ids"`
	Rubric   []criterion `json:"rubric"`
}

type criterion struct {
	ID        string  `json:"id"`
	Label     string  `json:"label"`
	MaxPoints float64 `json:"max_points"`
}

type unblindKey struct {
	Version    int         `json:"version"`
	ExerciseID string      `json:"exercise_id"`
	Pairs      []draftPair `json:"pairs"`
}

type draftPair struct {
	ProjectID             string   `json:"project_id"`
	AssistedDraftID       string   `json:"assisted_draft_id"`
	ManualDraftID         string   `json:"manual_draft_id"`
	ReferenceTechnicalPct *float64 `json:"reference_technical_pct"`
}

func validatePlan(plan reviewPlan) (map[string]project, error) {
	if plan.Version != schemaVersion || strings.TrimSpace(plan.ExerciseID) == "" {
		return nil, errors.New("需要支持的 version 和非空 exercise_id")
	}
	if plan.MarginPP == nil || !inRange(*plan.MarginPP, 0, 100) {
		return nil, errors.New("noninferiority_margin_pp 必须在 0–100 百分点内")
	}
	if len(plan.Reviewers) < 3 || len(plan.Projects) < 2 {
		return nil, errors.New("至少需要 3 个评审编号和 2 个独立项目族")
	}

	reviewers := make(map[string]bool)
	for _, id := range plan.Reviewers {
		if !validID(id) || reviewers[id] {
			return nil, fmt.Errorf("无效或重复的评审编号 %q", id)
		}
		reviewers[id] = true
	}

	drafts := make(map[string]project)
	projectIDs := make(map[string]bool)
	families := make(map[string]bool)
	for _, p := range plan.Projects {
		if !validID(p.ID) || projectIDs[p.ID] || !validID(p.Family) || families[p.Family] {
			return nil, fmt.Errorf("无效或重复的项目编号或项目族 %q / %q", p.ID, p.Family)
		}
		projectIDs[p.ID], families[p.Family] = true, true
		if len(p.DraftIDs) != 2 || len(p.Rubric) == 0 {
			return nil, fmt.Errorf("项目 %s 必须有两份稿件和非空评分表", p.ID)
		}
		var maxTotal float64
		criteria := make(map[string]bool)
		for _, item := range p.Rubric {
			if !validID(item.ID) || criteria[item.ID] || strings.TrimSpace(item.Label) == "" ||
				!inRange(item.MaxPoints, 0, math.MaxFloat64) || item.MaxPoints == 0 {
				return nil, fmt.Errorf("项目 %s 的评分项 %q 无效或重复", p.ID, item.ID)
			}
			criteria[item.ID] = true
			maxTotal += item.MaxPoints
		}
		if math.IsInf(maxTotal, 0) {
			return nil, fmt.Errorf("项目 %s 的评分表满分溢出", p.ID)
		}
		for _, id := range p.DraftIDs {
			if !validID(id) || drafts[id].ID != "" {
				return nil, fmt.Errorf("无效或重复的匿名稿件编号 %q", id)
			}
			drafts[id] = p
		}
	}
	return drafts, nil
}

type scoreKey struct {
	reviewer, draft, criterion string
}

func readPanel(input io.Reader, plan reviewPlan, drafts map[string]project) (map[string][]float64, error) {
	reader := csv.NewReader(input)
	reader.FieldsPerRecord = 4
	header, err := reader.Read()
	if err != nil || !slices.Equal(header, []string{"reviewer_id", "draft_id", "criterion_id", "points"}) {
		return nil, errors.New("CSV 表头须为 reviewer_id,draft_id,criterion_id,points")
	}
	scores := make(map[scoreKey]float64)
	for {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		line, _ := reader.FieldPos(0)
		key := scoreKey{reviewer: row[0], draft: row[1], criterion: row[2]}
		p, ok := drafts[key.draft]
		if !ok || !slices.Contains(plan.Reviewers, key.reviewer) {
			return nil, fmt.Errorf("CSV 第 %d 行的稿件或评审编号未知", line)
		}
		i := slices.IndexFunc(p.Rubric, func(item criterion) bool { return item.ID == key.criterion })
		if i < 0 {
			return nil, fmt.Errorf("CSV 第 %d 行的评分项 %q 不属于稿件 %s", line, key.criterion, key.draft)
		}
		if _, exists := scores[key]; exists {
			return nil, fmt.Errorf("CSV 第 %d 行重复记录同一评审、稿件和评分项", line)
		}
		points, err := strconv.ParseFloat(row[3], 64)
		if err != nil || !inRange(points, 0, p.Rubric[i].MaxPoints) {
			return nil, fmt.Errorf("CSV 第 %d 行分数须在 0–%g 内", line, p.Rubric[i].MaxPoints)
		}
		scores[key] = points
	}

	panel := make(map[string][]float64, len(drafts))
	for _, p := range plan.Projects {
		var maxTotal float64
		for _, item := range p.Rubric {
			maxTotal += item.MaxPoints
		}
		for _, draft := range p.DraftIDs {
			totals := make([]float64, 0, len(plan.Reviewers))
			for _, reviewer := range plan.Reviewers {
				var total float64
				for _, item := range p.Rubric {
					points, ok := scores[scoreKey{reviewer: reviewer, draft: draft, criterion: item.ID}]
					if !ok {
						return nil, fmt.Errorf("缺少评审 %s、稿件 %s、评分项 %s 的分数", reviewer, draft, item.ID)
					}
					total += points
				}
				totals = append(totals, 100*(total/maxTotal))
			}
			panel[draft] = totals
		}
	}
	return panel, nil
}

func validateKey(plan reviewPlan, key unblindKey) error {
	if key.Version != schemaVersion || key.ExerciseID != plan.ExerciseID {
		return errors.New("version 或 exercise_id 与评审方案不一致")
	}
	byProject := make(map[string]draftPair)
	for _, pair := range key.Pairs {
		if _, exists := byProject[pair.ProjectID]; exists {
			return fmt.Errorf("项目 %q 揭盲映射重复", pair.ProjectID)
		}
		byProject[pair.ProjectID] = pair
	}
	if len(byProject) != len(plan.Projects) {
		return errors.New("揭盲映射须覆盖评审方案的每个项目")
	}
	for _, p := range plan.Projects {
		pair, ok := byProject[p.ID]
		if !ok || pair.AssistedDraftID == pair.ManualDraftID ||
			!slices.Contains(p.DraftIDs, pair.AssistedDraftID) ||
			!slices.Contains(p.DraftIDs, pair.ManualDraftID) {
			return fmt.Errorf("项目 %s 的揭盲映射与匿名稿件不一致", p.ID)
		}
		if pair.ReferenceTechnicalPct != nil && !inRange(*pair.ReferenceTechnicalPct, 0, 100) {
			return fmt.Errorf("项目 %s 的模拟参考得分须在 0–100 内", p.ID)
		}
	}
	return nil
}

func validID(id string) bool {
	return id != "" && id == strings.TrimSpace(id) && !strings.ContainsAny(id, "\r\n\t")
}

func inRange(value, lower, upper float64) bool {
	return value >= lower && value <= upper
}
