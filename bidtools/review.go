package bidtools

// Phase 6A 标书正文审查工具（经 proposal-review 技能解锁）：
//   compliance_verify            对当前批准大纲（或单个章节）的「要求 × 章节」逐条核验正文实质响应，写入 compliance_matrix；
//   fulltext_consistency_check   比对全部章节有效修订中的总工期、质保期、SLA、团队人数、核心型号与数量，并列出 ★/▲ 漏项。
// 两个工具直接调用 db 层（加锁、快照与失效门禁都在 db 层），结论属于标书正文域，不写 explorations/findings。

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/autobid/autobid-engine/compliance"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/tool"
)

const (
	ToolComplianceVerify   = "compliance_verify"
	ToolFulltextConsistent = "fulltext_consistency_check"
)

func reviewErr(err error) (tool.Result, error) {
	switch {
	case errors.Is(err, db.ErrNotFound):
		return tool.Errorf("section not found in this task"), nil
	case errors.Is(err, db.ErrOutlineNotApproved):
		return tool.Errorf("no approved outline: %v; the outline must be approved before review", err), nil
	}
	return tool.Errorf("%v", err), nil
}

func (k *Kit) complianceVerifyTool() tool.CoreTool {
	spec := tool.Spec{Name: ToolComplianceVerify,
		Description: "对当前批准大纲（或单个指定章节）绑定的全部招标要求逐条核验正文是否给出实质性响应（拒绝仅复述标题、仅贴引用或简单照搬要求原文），" +
			"将核验结果与段落锚点写入响应矩阵（compliance_matrix），并返回每条要求的核验状态、偏离类型及不通过原因。",
		Schema: obj(map[string]any{"section_id": integer("仅核验指定章节 ID（可选，不填则核验全部章节）"),
			"section_code": str("仅核验指定章节编号（可选，如 \"3.2\"）")})}
	return tool.New(spec, func(ctx context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		req, err := decode[struct {
			SectionID int64  `json:"section_id"`
			Code      string `json:"section_code"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		sid := req.SectionID
		if sid == 0 && req.Code != "" {
			if sid, err = k.pg.SectionByCode(ctx, k.tenant, k.taskID, req.Code); err != nil {
				return reviewErr(err)
			}
		}
		res, err := k.pg.VerifyComplianceMatrix(ctx, k.tenant, k.taskID, sid, compliance.RuleVerifier{}, "worker")
		if err != nil {
			return reviewErr(err)
		}
		rows := make([]map[string]any, 0, len(res.Rows))
		for _, r := range res.Rows {
			row := map[string]any{"requirement": r.RequirementCode, "kind": r.Kind, "section": r.SectionCode, "status": r.VerifyStatus,
				"deviation": r.Deviation, "anchor": r.Anchor}
			if r.RevNo != nil {
				row["rev_no"] = *r.RevNo
			}
			if r.FailureReason != "" {
				row["reason"] = r.FailureReason
			}
			if r.StaleReason != "" {
				row["stale_reason"] = r.StaleReason
			}
			rows = append(rows, row)
		}
		return tool.JSON(map[string]any{"outline_version": res.OutlineVersion, "checked": res.Checked, "verified": res.Verified,
			"failed": res.Failed, "pending": res.Pending, "stale": res.Stale, "rows": rows,
			"note": "rule-based verification; failed / pending rows must be fixed in the section text, not by editing this result"}), nil
	})
}

func (k *Kit) fulltextConsistencyTool() tool.CoreTool {
	spec := tool.Spec{Name: ToolFulltextConsistent,
		Description: "横向比对当前批准大纲下所有章节有效修订稿中的关键定量承诺（总工期、免费质保期、SLA 响应/到场/修复时限与可用性、项目团队人数、核心设备型号与数量），" +
			"检出跨章节前后矛盾项（contradictions），并汇总所有尚未通过实质性响应核验的 ★/▲ 强制条款漏项（omissions）。",
		Schema: obj(map[string]any{"reverify": boolean("检查前是否先对全部章节重新运行响应矩阵核验（默认 true）")})}
	return tool.New(spec, func(ctx context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		req, err := decode[struct {
			Reverify *bool `json:"reverify"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		if req.Reverify == nil || *req.Reverify {
			if _, err := k.pg.VerifyComplianceMatrix(ctx, k.tenant, k.taskID, 0, compliance.RuleVerifier{}, "worker"); err != nil {
				return reviewErr(err)
			}
		}
		det, err := k.pg.RunConsistencyCheck(ctx, k.tenant, k.taskID, "worker")
		if err != nil {
			return reviewErr(err)
		}
		var contradictions, omissions []map[string]any
		for _, f := range det.Findings {
			if f.Kind == db.FindingContradiction {
				contradictions = append(contradictions, map[string]any{"finding_id": f.ID, "metric": f.MetricLabel,
					"a": map[string]any{"section": f.ASectionCode, "rev_no": f.ARevNo, "value": f.AValue, "excerpt": f.AExcerpt},
					"b": map[string]any{"section": f.BSectionCode, "rev_no": f.BRevNo, "value": f.BValue, "excerpt": f.BExcerpt}})
			} else {
				omissions = append(omissions, map[string]any{"finding_id": f.ID, "requirement": f.RequirementCode, "title": f.RequirementTitle,
					"kind": f.RequirementKind, "detail": f.Detail})
			}
		}
		return tool.JSON(map[string]any{"run_id": det.Run.ID, "outline_version": det.Run.OutlineVersion, "snapshot": det.Run.Snapshot,
			"contradictions": contradictions, "omissions": omissions,
			"note": "rule-based extraction (recall/precision not evaluated); report every contradiction and omission to the human reviewer"}), nil
	})
}

func (k *Kit) reviewTools() []tool.CoreTool {
	return []tool.CoreTool{k.complianceVerifyTool(), k.fulltextConsistencyTool()}
}
