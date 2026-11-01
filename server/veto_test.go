package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
)

// 否决项核对与投标范围的 HTTP 契约：上传携带文件角色与发布时间、提取产出法定否决项与截止时间、
// 核对须写依据（400）、非否决项 404、未知分包 400、人工截止时间写回清单。
func TestHTTPVetoAndBidScope(t *testing.T) {
	c := newProposalAPI(t, newGatedWriter())
	var task struct {
		ID int64 `json:"id"`
	}
	if code := c.raw("POST", "/api/tasks", map[string]any{"name": "d1", "goal": "否决项核对"}, &task); code != 201 {
		t.Fatalf("create task: %d", code)
	}
	base := fmt.Sprintf("/api/tasks/%d", task.ID)
	text := strings.Join([]string{"第一章 投标邀请", "（七）投标截止时间：2026年8月26日北京时间10时00分；",
		"第四章 投标文件格式", "投标文件正本一份、副本四份，须逐页签字盖章。"}, "\n")
	upload := map[string]any{"tender_no": fmt.Sprintf("ZB-D1-%d", time.Now().UnixNano()), "title": "招标文件", "text": text}
	for field, bad := range map[string]string{"doc_role": "memo", "published_at": "08/01/2026"} {
		req := map[string]any{field: bad}
		for k, v := range upload {
			req[k] = v
		}
		if code := c.raw("POST", base+"/documents", req, nil); code != 400 {
			t.Fatalf("upload with invalid %s: %d", field, code)
		}
	}
	upload["doc_role"], upload["published_at"] = "tender", "2026-08-05"
	if code := c.raw("POST", base+"/documents", upload, nil); code != 201 {
		t.Fatalf("upload: %d", code)
	}
	var docs []db.TenderDocument
	if code := c.raw("GET", base+"/documents", nil, &docs); code != 200 || len(docs) != 1 || docs[0].DocRole != "tender" || docs[0].PublishedAt == nil {
		t.Fatalf("documents: %d %+v", code, docs)
	}

	var extracted struct {
		Result       db.ExtractResult    `json:"result"`
		Requirements db.RequirementsView `json:"requirements"`
	}
	if code := c.raw("POST", base+"/requirements/extract", nil, &extracted); code != 200 || extracted.Result.VetoItems != 6 {
		t.Fatalf("extract: %d %+v", code, extracted.Result)
	}
	want := time.Date(2026, 8, 26, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if m := extracted.Requirements.Manifest; m.BidDeadline == nil || !m.BidDeadline.Equal(want) || extracted.Requirements.Stats.Veto.Pending != 6 {
		t.Fatalf("manifest = %+v stats = %+v", m, extracted.Requirements.Stats)
	}
	var veto, other *db.RequirementItem
	for _, it := range extracted.Requirements.Items {
		if it.Category == db.ReqVetoItem && veto == nil {
			veto = it
		} else if it.Category != db.ReqVetoItem && other == nil {
			other = it
		}
	}
	var e apiErr
	if code := c.raw("PUT", fmt.Sprintf("%s/requirements/%d/veto", base, veto.ID), map[string]any{"status": "passed"}, &e); code != 400 || e.Code != "invalid_requirement" {
		t.Fatalf("pass without note: %d %+v", code, e)
	}
	if code := c.raw("PUT", fmt.Sprintf("%s/requirements/%d/veto", base, other.ID), map[string]any{"status": "passed", "note": "x"}, nil); code != 404 {
		t.Fatalf("veto on non-veto requirement: %d", code)
	}
	var checked db.RequirementItem
	if code := c.raw("PUT", fmt.Sprintf("%s/requirements/%d/veto", base, veto.ID), map[string]any{"status": "passed", "note": "保证金回单"}, &checked); code != 200 ||
		checked.VetoStatus != db.VetoPassed || checked.VetoCheckedBy != "http-reviewer" {
		t.Fatalf("veto check: %d %+v", code, checked)
	}

	if code := c.raw("PUT", base+"/requirements/bid-scope", map[string]any{"bid_lots": []string{"9"}}, &e); code != 400 {
		t.Fatalf("unknown lot: %d %+v", code, e)
	}
	var view db.RequirementsView
	if code := c.raw("PUT", base+"/requirements/bid-scope", map[string]any{"bid_deadline": "2026-08-27 09:30"}, &view); code != 200 ||
		view.Manifest.BidDeadline == nil || !strings.Contains(string(view.Manifest.BidDeadlineSource), `"manual"`) || view.Stats.Veto.Passed != 1 {
		t.Fatalf("bid scope: %d %+v", code, view.Manifest)
	}

	// Bid / No-Bid 决策路由与导出阻断门禁。
	if code := c.raw("PUT", base+"/requirements/bid-decision", map[string]any{"bid_decision": "no_bid"}, &e); code != 400 {
		t.Fatalf("no_bid without note: %d %+v", code, e)
	}
	if code := c.raw("PUT", base+"/requirements/bid-decision", map[string]any{"bid_decision": "no_bid", "bid_decision_note": "关键资质暂缺"}, &view); code != 200 ||
		view.Manifest.BidDecision != db.BidDecisionNoBid || view.BidEvaluation.Decision != db.BidDecisionNoBid {
		t.Fatalf("bid decision no_bid: %d %+v", code, view.Manifest)
	}
	var readiness db.ExportReadiness
	if code := c.raw("GET", base+"/export-readiness", nil, &readiness); code != 200 || readiness.Counts[db.BlockNoBid] != 1 {
		t.Fatalf("export readiness should include no_bid_decision blocker: %d %+v", code, readiness)
	}
}
