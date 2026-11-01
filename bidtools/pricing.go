package bidtools

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/tool"
)

// 金额一律用 big.Rat 计算，避免浮点误差；输出保留两位小数的十进制文本。

type quoteInput struct {
	AssetID   int64       `json:"asset_id"`
	Code      string      `json:"code"`
	UnitPrice json.Number `json:"unit_price"`
	Quantity  json.Number `json:"quantity"`
	LineTotal json.Number `json:"line_total"`
}

type pricingInput struct {
	LotNo             string       `json:"lot_no"`
	CeilingPrice      json.Number  `json:"ceiling_price"`
	Quotes            []quoteInput `json:"quotes"`
	DeclaredTotal     json.Number  `json:"declared_total"`
	CostEstimate      json.Number  `json:"cost_estimate"`
	MinProfitMargin   *float64     `json:"min_profit_margin"`
	AbnormalLowRatio  *float64     `json:"abnormal_low_ratio"`
	ToleranceAbsolute json.Number  `json:"tolerance"`
}

type pricingIssue struct {
	Severity string `json:"severity"`
	Kind     string `json:"kind"`
	AssetID  int64  `json:"asset_id,omitempty"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message"`
}

type pricedLine struct {
	AssetID     int64  `json:"asset_id,omitempty"`
	Code        string `json:"code"`
	Title       string `json:"title,omitempty"`
	UnitPrice   string `json:"unit_price"`
	Quantity    string `json:"quantity"`
	Computed    string `json:"computed_total"`
	Declared    string `json:"declared_total,omitempty"`
	ItemCeiling string `json:"item_ceiling,omitempty"`
	Consistent  bool   `json:"consistent"`
}

type pricingResult struct {
	Verdict         string         `json:"verdict"` // pass | warn | fail
	Lines           []pricedLine   `json:"lines"`
	ComputedTotal   string         `json:"computed_total"`
	DeclaredTotal   string         `json:"declared_total,omitempty"`
	CeilingPrice    string         `json:"ceiling_price,omitempty"`
	CeilingSource   string         `json:"ceiling_source,omitempty"`
	CeilingRatio    string         `json:"ceiling_ratio,omitempty"`
	ProfitMargin    string         `json:"profit_margin,omitempty"`
	MissingItems    []string       `json:"missing_items"`
	Issues          []pricingIssue `json:"issues"`
	CommercialItems int            `json:"commercial_items"`
	QuotedItems     int            `json:"quoted_items"`
	DefaultsApplied map[string]any `json:"defaults_applied,omitempty"`
}

const (
	defaultMinProfitMargin  = 0.03
	defaultAbnormalLowRatio = 0.7
)

func parseRat(n json.Number) (*big.Rat, bool, error) {
	s := strings.TrimSpace(n.String())
	if s == "" {
		return nil, false, nil
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, false, fmt.Errorf("invalid number %q", s)
	}
	return r, true, nil
}

func ratFromString(s *string) *big.Rat {
	if s == nil {
		return nil
	}
	r, ok := new(big.Rat).SetString(strings.TrimSpace(*s))
	if !ok {
		return nil
	}
	return r
}

func money(r *big.Rat) string { return r.FloatString(2) }

// verifyCommercial 对报价做勾稽与限价验算；ceiling 为 nil 时跳过最高限价检查。
func verifyCommercial(items []*db.Asset, ceiling *big.Rat, ceilingSource string, in pricingInput) (pricingResult, error) {
	res := pricingResult{Lines: []pricedLine{}, MissingItems: []string{}, Issues: []pricingIssue{}, CommercialItems: len(items)}
	tol := new(big.Rat).SetFrac64(1, 100)
	if t, ok, err := parseRat(in.ToleranceAbsolute); err != nil {
		return res, fmt.Errorf("tolerance: %w", err)
	} else if ok {
		tol = t
	}
	minMargin, lowRatio := defaultMinProfitMargin, defaultAbnormalLowRatio
	res.DefaultsApplied = map[string]any{}
	if in.MinProfitMargin != nil {
		minMargin = *in.MinProfitMargin
	} else {
		res.DefaultsApplied["min_profit_margin"] = minMargin
	}
	if in.AbnormalLowRatio != nil {
		lowRatio = *in.AbnormalLowRatio
	} else {
		res.DefaultsApplied["abnormal_low_ratio"] = lowRatio
	}
	issue := func(sev, kind string, a *db.Asset, code, format string, args ...any) {
		is := pricingIssue{Severity: sev, Kind: kind, Code: code, Message: fmt.Sprintf(format, args...)}
		if a != nil {
			is.AssetID, is.Code = a.ID, a.Code
		}
		res.Issues = append(res.Issues, is)
	}
	byID, byCode := map[int64]*db.Asset{}, map[string]*db.Asset{}
	for _, a := range items {
		byID[a.ID] = a
		byCode[a.Code] = a
	}
	quoted := map[int64]bool{}
	total := new(big.Rat)
	for i, q := range in.Quotes {
		a := byID[q.AssetID]
		if a == nil && q.Code != "" {
			a = byCode[q.Code]
		}
		code := q.Code
		if a != nil {
			code = a.Code
		}
		if code == "" {
			code = fmt.Sprintf("#%d", i)
		}
		if a == nil {
			issue(db.SeverityMedium, "unknown_item", nil, code, "报价项 %s 不在报价清单要素中，请核对是否多报或清单未入图", code)
		} else if quoted[a.ID] {
			issue(db.SeverityHigh, "duplicate_quote", a, "", "清单项 %s 重复报价", a.Code)
			continue
		}
		unit, hasUnit, err := parseRat(q.UnitPrice)
		if err != nil {
			return res, fmt.Errorf("quotes[%d].unit_price: %w", i, err)
		}
		qty, hasQty, err := parseRat(q.Quantity)
		if err != nil {
			return res, fmt.Errorf("quotes[%d].quantity: %w", i, err)
		}
		if !hasQty && a != nil {
			qty = ratFromString(a.Quantity)
			hasQty = qty != nil
		}
		declared, hasDeclared, err := parseRat(q.LineTotal)
		if err != nil {
			return res, fmt.Errorf("quotes[%d].line_total: %w", i, err)
		}
		line := pricedLine{Code: code, Consistent: true}
		if a != nil {
			line.AssetID, line.Title = a.ID, truncate(a.Title, 60)
			quoted[a.ID] = true
		}
		var computed *big.Rat
		switch {
		case hasUnit && hasQty:
			computed = new(big.Rat).Mul(unit, qty)
			line.UnitPrice, line.Quantity = money(unit), qty.FloatString(4)
		case hasDeclared:
			computed = declared
			issue(db.SeverityLow, "incomplete_quote", a, code, "清单项 %s 缺少单价或数量，按分项合价计入", code)
		default:
			issue(db.SeverityHigh, "incomplete_quote", a, code, "清单项 %s 缺少单价/数量且无分项合价，无法计价", code)
			res.Lines = append(res.Lines, line)
			continue
		}
		line.Computed = money(computed)
		if hasDeclared {
			line.Declared = money(declared)
			if diff := new(big.Rat).Sub(computed, declared); diff.Abs(diff).Cmp(tol) > 0 {
				line.Consistent = false
				issue(db.SeverityHigh, "line_mismatch", a, code, "清单项 %s 单价×数量=%s，与填报合价 %s 不一致", code, money(computed), money(declared))
			}
		}
		if a != nil {
			if itemCeiling := ratFromString(a.Amount); itemCeiling != nil {
				line.ItemCeiling = money(itemCeiling)
				if computed.Cmp(itemCeiling) > 0 {
					issue(db.SeverityHigh, "item_over_ceiling", a, "", "清单项 %s 报价 %s 超过分项限价 %s", a.Code, money(computed), money(itemCeiling))
				}
			}
		}
		total.Add(total, computed)
		res.Lines = append(res.Lines, line)
	}
	res.QuotedItems = len(quoted)
	for _, a := range items {
		if !quoted[a.ID] {
			res.MissingItems = append(res.MissingItems, a.Code)
			issue(db.SeverityHigh, "missing_item", a, "", "清单项 %s（%s）未报价（漏项）", a.Code, truncate(a.Title, 40))
		}
	}
	res.ComputedTotal = money(total)
	if declared, ok, err := parseRat(in.DeclaredTotal); err != nil {
		return res, fmt.Errorf("declared_total: %w", err)
	} else if ok {
		res.DeclaredTotal = money(declared)
		if diff := new(big.Rat).Sub(total, declared); diff.Abs(diff).Cmp(tol) > 0 {
			issue(db.SeverityHigh, "total_mismatch", nil, "", "分项合计 %s 与投标总价 %s 不一致", money(total), money(declared))
		}
		total = declared // 限价与利润按投标人填报的总价判定
	}
	if ceiling != nil && ceiling.Sign() > 0 {
		res.CeilingPrice, res.CeilingSource = money(ceiling), ceilingSource
		ratio := new(big.Rat).Quo(total, ceiling)
		res.CeilingRatio = ratio.FloatString(4)
		if total.Cmp(ceiling) > 0 {
			issue(db.SeverityCritical, "over_ceiling", nil, "", "投标总价 %s 超过最高限价 %s，将被否决", money(total), money(ceiling))
		} else if f, _ := ratio.Float64(); f < lowRatio {
			issue(db.SeverityMedium, "abnormally_low", nil, "", "投标总价仅为最高限价的 %.1f%%，低于 %.0f%%，可能被认定为异常低价，需准备成本说明", f*100, lowRatio*100)
		}
	} else {
		issue(db.SeverityLow, "no_ceiling", nil, "", "未找到最高限价（ceiling_price 未提供，tender/lot 要素 amount 为空），未做限价检查")
	}
	if cost, ok, err := parseRat(in.CostEstimate); err != nil {
		return res, fmt.Errorf("cost_estimate: %w", err)
	} else if ok && total.Sign() > 0 {
		margin := new(big.Rat).Quo(new(big.Rat).Sub(total, cost), total)
		res.ProfitMargin = margin.FloatString(4)
		f, _ := margin.Float64()
		switch {
		case f < 0:
			issue(db.SeverityCritical, "below_cost", nil, "", "投标总价 %s 低于成本测算 %s，存在低于成本报价被否决与亏损风险", money(total), money(cost))
		case f < minMargin:
			issue(db.SeverityMedium, "thin_margin", nil, "", "风险利润率 %.2f%% 低于要求 %.2f%%", f*100, minMargin*100)
		}
	}
	res.Verdict = "pass"
	for _, is := range res.Issues {
		switch is.Severity {
		case db.SeverityCritical, db.SeverityHigh:
			res.Verdict = "fail"
		case db.SeverityMedium:
			if res.Verdict == "pass" {
				res.Verdict = "warn"
			}
		}
	}
	return res, nil
}

func (k *Kit) verifyCommercialTool() tool.CoreTool {
	return readTool(tool.Spec{
		Name: ToolVerifyCommercial,
		Description: "对照当前项目的商务报价清单要素（commercial_item）使用精确有理数运算全面验算拟投标报价：逐行核对“单价 × 数量 = 合价”、" +
			"单项最高限价、漏项与重复报价、分项合计与投标总价一致性、项目/标段最高限价（超限直接否决）、异常低价风险线及预期成本利润率。" +
			"返回分级问题列表与总体判定结论 verdict（pass 通过 | warn 预警 | fail 不通过）。",
		Schema: obj(map[string]any{
			"lot_no":        str("仅核验指定标段编号的商务清单项（共用项自动包含；默认以该标段限价作为最高限价）"),
			"ceiling_price": number("最高投标限价（不填时优先取标段要素限价，其次取招标项目总限价）"),
			"quotes": map[string]any{"type": "array", "description": "拟填报的分项报价明细列表",
				"items": obj(map[string]any{
					"asset_id":   integer("商务清单要素 ID（与 code 二选一）"),
					"code":       str("商务清单项编码（与 asset_id 二选一）"),
					"unit_price": number("填报含税单价"),
					"quantity":   number("报价数量（不填则默认取招标清单要素数量）"),
					"line_total": number("报价表填报的单项合价"),
				})},
			"declared_total":     number("开标一览表/报价总表填报的投标总价"),
			"cost_estimate":      number("内部测算总成本（用于核验是否低于成本报价及测算利润率）"),
			"min_profit_margin":  number("最低要求利润率（0~1 之间小数，默认 0.03 即 3%）"),
			"abnormal_low_ratio": number("异常低价预警占最高限价比例阈值（默认 0.70 即 70%）"),
			"tolerance":          number("金额勾稽允许的最大绝对尾差（默认 0.01 元）"),
		}, "quotes"),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		dec := json.NewDecoder(strings.NewReader(string(in)))
		dec.UseNumber()
		var req pricingInput
		if err := dec.Decode(&req); err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		assets, err := k.as.ListScopedAssets(ctx, []string{db.AssetCommercialItem, db.AssetTender, db.AssetLot}, 5000)
		if err != nil {
			return tool.Result{}, err
		}
		var items []*db.Asset
		var tenderCeiling, lotCeiling *big.Rat
		for _, a := range assets {
			switch a.Type {
			case db.AssetCommercialItem:
				if req.LotNo == "" || a.LotNo == "" || a.LotNo == req.LotNo {
					items = append(items, a)
				}
			case db.AssetTender:
				if c := ratFromString(a.Amount); c != nil && tenderCeiling == nil {
					tenderCeiling = c
				}
			case db.AssetLot:
				if req.LotNo != "" && a.LotNo == req.LotNo {
					lotCeiling = ratFromString(a.Amount)
				}
			}
		}
		ceiling, source := (*big.Rat)(nil), ""
		if c, ok, err := parseRat(req.CeilingPrice); err != nil {
			return tool.Errorf("ceiling_price: %v", err), nil
		} else if ok {
			ceiling, source = c, "input"
		} else if lotCeiling != nil {
			ceiling, source = lotCeiling, "lot_asset"
		} else if tenderCeiling != nil {
			ceiling, source = tenderCeiling, "tender_asset"
		}
		res, err := verifyCommercial(items, ceiling, source, req)
		if err != nil {
			return tool.Errorf("%v", err), nil
		}
		return tool.JSON(res), nil
	})
}
