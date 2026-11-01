package bidtools

import (
	"context"
	"encoding/json"
	"math"
	"sort"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/tool"
)

type scoreAssumption struct {
	AssetID       int64   `json:"asset_id"`
	ExpectedScore float64 `json:"expected_score"`
	FactIDs       []int64 `json:"fact_ids"`
	Note          string  `json:"note"`
}

// 评分依据。
const (
	BasisAssumption = "assumption"        // 调用方给出的预计得分
	BasisFact       = "fact"              // 锚定事实 payload 中的 expected_score / score
	BasisUnscored   = "evidence_unscored" // 有锚定事实但未给出分值，按 0 计
	BasisNone       = "none"              // 无任何事实支撑，按 0 计
	BasisNoMax      = "missing_score_max" // 评分项缺少满分，不计入合计
)

type scoredItem struct {
	AssetID  int64    `json:"asset_id"`
	Code     string   `json:"code"`
	Title    string   `json:"title"`
	LotNo    string   `json:"lot_no,omitempty"`
	Category string   `json:"category"`
	ScoreMax *float64 `json:"score_max"`
	Expected float64  `json:"expected"`
	Loss     float64  `json:"loss"`
	Ratio    *float64 `json:"ratio,omitempty"`
	Basis    string   `json:"basis"`
	FactIDs  []int64  `json:"fact_ids,omitempty"`
	Note     string   `json:"note,omitempty"`
}

type categoryTotal struct {
	Category string  `json:"category"`
	Max      float64 `json:"max"`
	Expected float64 `json:"expected"`
}

type scoringResult struct {
	Items         []scoredItem    `json:"items"`
	MaxTotal      float64         `json:"max_total"`
	ExpectedTotal float64         `json:"expected_total"`
	Ratio         *float64        `json:"ratio"`
	ByCategory    []categoryTotal `json:"by_category"`
	TopLosses     []scoredItem    `json:"top_losses"`
	Unevidenced   []int64         `json:"unevidenced"`
	Warnings      []string        `json:"warnings,omitempty"`
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

func ratioPtr(a, b float64) *float64 {
	if b <= 0 {
		return nil
	}
	r := round2(a / b)
	return &r
}

// factScore 取事实 payload 中的 expected_score（回落 score）。
func factScore(n *db.Node) (float64, bool) {
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) != nil {
		return 0, false
	}
	for _, key := range []string{"expected_score", "score"} {
		if v, ok := numeric(p[key]); ok {
			return v, true
		}
	}
	return 0, false
}

// simulateScoring 计算每个评分项的预计得分：assumption 优先，其次取最新锚定事实中的分值；
// 预计分被截断到 [0, score_max]。
func simulateScoring(items []*db.Asset, facts map[int64][]*db.Node, assumptions []scoreAssumption) scoringResult {
	byAsset := map[int64]scoreAssumption{}
	for _, a := range assumptions {
		byAsset[a.AssetID] = a
	}
	res := scoringResult{Items: []scoredItem{}, ByCategory: []categoryTotal{}, TopLosses: []scoredItem{}, Unevidenced: []int64{}}
	cats := map[string]*categoryTotal{}
	var catOrder []string
	known := map[int64]bool{}
	for _, a := range items {
		known[a.ID] = true
		it := scoredItem{AssetID: a.ID, Code: a.Code, Title: truncate(a.Title, 60), LotNo: a.LotNo, Category: a.Category, ScoreMax: a.ScoreMax, Basis: BasisNone}
		if it.Category == "" {
			it.Category = "未分类"
		}
		for _, f := range facts[a.ID] {
			it.FactIDs = append(it.FactIDs, f.ID)
		}
		if as, ok := byAsset[a.ID]; ok {
			it.Expected, it.Basis, it.Note = as.ExpectedScore, BasisAssumption, as.Note
			it.FactIDs = append(it.FactIDs, as.FactIDs...)
		} else if fs := facts[a.ID]; len(fs) > 0 {
			it.Basis = BasisUnscored
			for i := len(fs) - 1; i >= 0; i-- {
				if v, ok := factScore(fs[i]); ok {
					it.Expected, it.Basis = v, BasisFact
					break
				}
			}
		}
		if len(it.FactIDs) == 0 {
			res.Unevidenced = append(res.Unevidenced, a.ID)
		}
		if a.ScoreMax == nil {
			it.Basis = BasisNoMax
			res.Warnings = append(res.Warnings, "评分项 "+a.Code+" 缺少 score_max，未计入合计")
			res.Items = append(res.Items, it)
			continue
		}
		max := *a.ScoreMax
		if it.Expected > max || it.Expected < 0 {
			res.Warnings = append(res.Warnings, "评分项 "+a.Code+" 的预计得分超出 [0, score_max]，已截断")
			it.Expected = math.Min(math.Max(it.Expected, 0), max)
		}
		it.Expected = round2(it.Expected)
		it.Loss = round2(max - it.Expected)
		it.Ratio = ratioPtr(it.Expected, max)
		res.MaxTotal += max
		res.ExpectedTotal += it.Expected
		c, ok := cats[it.Category]
		if !ok {
			c = &categoryTotal{Category: it.Category}
			cats[it.Category] = c
			catOrder = append(catOrder, it.Category)
		}
		c.Max += max
		c.Expected += it.Expected
		res.Items = append(res.Items, it)
	}
	for _, a := range assumptions {
		if !known[a.AssetID] {
			res.Warnings = append(res.Warnings, "assumption 指向的 asset 不是范围内的评分项，已忽略")
		}
	}
	for _, name := range catOrder {
		c := cats[name]
		res.ByCategory = append(res.ByCategory, categoryTotal{c.Category, round2(c.Max), round2(c.Expected)})
	}
	res.MaxTotal, res.ExpectedTotal = round2(res.MaxTotal), round2(res.ExpectedTotal)
	res.Ratio = ratioPtr(res.ExpectedTotal, res.MaxTotal)
	losses := make([]scoredItem, 0, len(res.Items))
	for _, it := range res.Items {
		if it.ScoreMax != nil && it.Loss > 0 {
			losses = append(losses, it)
		}
	}
	sort.SliceStable(losses, func(i, j int) bool { return losses[i].Loss > losses[j].Loss })
	res.TopLosses = losses[:min(len(losses), 5)]
	return res
}

func (k *Kit) simulateScoringTool() tool.CoreTool {
	return readTool(tool.Spec{
		Name: ToolSimulateScoring,
		Description: "基于当前项目已提取的评分项要素（scoring_item）及锚定其上的已确认事实节点（fact），模拟测算技术与商务评标得分。" +
			"返回逐项预期得分、总分、得分率、按类别小计、前五大失分项（top_losses）及尚无证据支撑的评分项（unevidenced_assets）。" +
			"已确认事实节点提供默认 expected_score/score，也可通过 assumptions 覆盖测算。",
		Schema: obj(map[string]any{
			"lot_no": str("仅模拟指定标段编号的评分项（所有标段共用的评分项始终包含；不填则测算全部）"),
			"assumptions": map[string]any{"type": "array", "description": "人工或推演确定的评分项预期得分覆盖列表",
				"items": obj(map[string]any{
					"asset_id":       integer("评分项要素 ID（scoring_item asset_id）"),
					"expected_score": number("该评分项的预期得分（自动限制在 0 到满分之间）"),
					"fact_ids":       map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "支撑该得分的已确认事实节点 ID 列表"},
					"note":           str("得分测算依据说明"),
				}, "asset_id", "expected_score")},
		}),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			LotNo       string            `json:"lot_no"`
			Assumptions []scoreAssumption `json:"assumptions"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		assets, err := k.as.ListScopedAssets(ctx, []string{db.AssetScoringItem}, 2000)
		if err != nil {
			return tool.Result{}, err
		}
		var items []*db.Asset
		var ids []int64
		for _, a := range assets {
			if req.LotNo == "" || a.LotNo == "" || a.LotNo == req.LotNo {
				items = append(items, a)
				ids = append(ids, a.ID)
			}
		}
		if len(items) == 0 {
			return tool.Errorf("no scoring_item elements in this task's scope; extract the scoring table with insert_assets first"), nil
		}
		facts, err := k.es.ConfirmedFactsByAsset(ctx, ids)
		if err != nil {
			return tool.Result{}, err
		}
		return tool.JSON(simulateScoring(items, facts, req.Assumptions)), nil
	})
}
