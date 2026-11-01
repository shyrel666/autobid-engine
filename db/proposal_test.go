package db

// Phase 5 集成测试（真实 PostgreSQL，需要 AUTOBID_TEST_DATABASE_URL）：要求清单提取 / 确认、大纲持久化审批、
// 章节派发与写回双端门禁、乐观锁冲突、绑定版本批准与回滚、证据失效联动。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type proposalFixture struct {
	*fixture
	docID int64
}

func newProposalFixture(t *testing.T) *proposalFixture {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()
	chunks := []DocumentChunk{
		{Seq: 0, Level: 1, SectionCode: "第一章", SectionTitle: "投标人须知", Content: "第一章 投标人须知"},
		{Seq: 1, Level: 2, SectionCode: "1.1", SectionTitle: "资格要求", Content: "★1.1 投标人须具备安防工程一级资质，否则投标无效。", IsMandatory: true},
		{Seq: 2, Level: 1, SectionCode: "第三章", SectionTitle: "评分标准", Content: "技术方案评分：总体架构合理、技术路线先进得10分。"},
		{Seq: 3, Level: 2, SectionCode: "4.1", SectionTitle: "投标文件格式", Content: "投标文件正本一份、副本四份，须逐页签字盖章。"},
		{Seq: 4, Level: 2, SectionCode: "5.2", SectionTitle: "售后服务", Content: "投标人须提供不少于三年的售后服务承诺书。"},
		{Seq: 5, Level: 2, SectionCode: "5.3", SectionTitle: "说明", Content: "本项目位于园区东侧。"},
	}
	docID, _, err := f.d.InsertTenderDocument(ctx, &TenderDocument{TaskID: f.task.ID, TenderNo: uniq("ZB-P5"), Title: "园区安防招标文件",
		RawText: uniq("raw")}, chunks)
	if err != nil {
		t.Fatalf("InsertTenderDocument: %v", err)
	}
	return &proposalFixture{fixture: f, docID: docID}
}

func (p *proposalFixture) extractAndConfirm(t *testing.T) *RequirementsView {
	t.Helper()
	ctx := context.Background()
	if _, err := p.d.ExtractRequirements(ctx, DefaultTenant, p.task.ID, "reviewer"); err != nil {
		t.Fatalf("ExtractRequirements: %v", err)
	}
	if _, err := p.d.ConfirmRequirements(ctx, DefaultTenant, p.task.ID, "reviewer", 0); err != nil {
		t.Fatalf("ConfirmRequirements: %v", err)
	}
	v, err := p.d.ListRequirements(ctx, DefaultTenant, p.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// approvedOutline 提取并确认清单、生成默认大纲、提交并批准，返回章节编码 -> ID。
func (p *proposalFixture) approvedOutline(t *testing.T) (*OutlineDetail, map[string]int64) {
	t.Helper()
	ctx := context.Background()
	p.extractAndConfirm(t)
	det, err := p.d.CreateOutline(ctx, DefaultTenant, p.task.ID, "editor", OutlineInput{Note: "默认目录"})
	if err != nil {
		t.Fatalf("CreateOutline: %v", err)
	}
	if _, err := p.d.SubmitOutline(ctx, DefaultTenant, p.task.ID, det.Outline.Version, "editor"); err != nil {
		t.Fatalf("SubmitOutline: %v", err)
	}
	det, err = p.d.ApproveOutline(ctx, DefaultTenant, p.task.ID, det.Outline.Version, "approver")
	if err != nil {
		t.Fatalf("ApproveOutline: %v", err)
	}
	return det, sectionIDs(det)
}

func sectionIDs(det *OutlineDetail) map[string]int64 {
	out := map[string]int64{}
	for _, s := range det.Sections {
		out[s.Code] = s.ID
	}
	return out
}

func (p *proposalFixture) save(t *testing.T, sectionID int64, base, outlineVersion int, source, content string, lease int64, evidence ...int64) *SaveRevisionResult {
	t.Helper()
	res, err := p.d.SaveSectionRevision(context.Background(), DefaultTenant, "writer", SaveRevisionInput{TaskID: p.task.ID, SectionID: sectionID,
		BaseRevNo: base, BasedOnOutlineVersion: outlineVersion, Source: source, ContentMD: content, SummaryNote: "摘要：" + content,
		EvidenceIDs: evidence, LeaseToken: lease})
	if err != nil {
		t.Fatalf("SaveSectionRevision(%s base=%d v=%d): %v", source, base, outlineVersion, err)
	}
	return res
}

func (p *proposalFixture) section(t *testing.T, id int64) *SectionDetail {
	t.Helper()
	det, err := p.d.GetSection(context.Background(), DefaultTenant, p.task.ID, id)
	if err != nil {
		t.Fatalf("GetSection: %v", err)
	}
	return det
}

func TestRequirementExtractionAndManifest(t *testing.T) {
	p := newProposalFixture(t)
	ctx := context.Background()
	res, err := p.d.ExtractRequirements(ctx, DefaultTenant, p.task.ID, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	// 4 条原文要求 + 6 条法定否决项（87 号令第六十三条五项与第三十七条）。
	if res.Created != 10 || res.VetoItems != 6 || res.Scanned != 6 {
		t.Fatalf("extract = %+v", res)
	}
	v, err := p.d.ListRequirements(ctx, DefaultTenant, p.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	cats := map[string]*RequirementItem{}
	for _, it := range v.Items {
		cats[it.Category] = it
		if it.VetoKind == VetoStatutory {
			if it.VetoStatus != VetoPending || !it.IsMandatory || it.InTechnicalScope() {
				t.Fatalf("statutory veto item: %+v", it)
			}
			continue
		}
		if it.SourceChunkID == nil || it.SourceHash == "" || !strings.Contains(string(it.SourceLoc), `"page_start"`) {
			t.Fatalf("requirement without versioned source: %+v", it)
		}
	}
	if len(cats) != 5 || !cats[ReqMandatoryClause].IsMandatory || cats[ReqScoringItem].ScoreWeight != 10 {
		t.Fatalf("categories = %+v", v.Items)
	}
	if v.Stats.Total != 10 || v.Stats.NoEvidence != 4 || len(v.Stats.Unmapped) != 4 || v.Manifest.Status != ManifestDraft ||
		v.Stats.Veto != (VetoStats{Total: 6, Pending: 6}) || v.Stats.OutOfScope != 0 {
		t.Fatalf("stats = %+v manifest = %+v", v.Stats, v.Manifest)
	}
	// 幂等：原文未变不重复写入、不递增 epoch。
	epoch := v.Manifest.Epoch
	again, err := p.d.ExtractRequirements(ctx, DefaultTenant, p.task.ID, "reviewer")
	if err != nil || again.Created != 0 || again.Unchanged != 10 {
		t.Fatalf("re-extract = %+v err=%v", again, err)
	}
	if m, _ := p.d.RequirementManifest(ctx, DefaultTenant, p.task.ID); m.Epoch != epoch {
		t.Fatalf("idempotent extract bumped epoch %d -> %d", epoch, m.Epoch)
	}
	// 人工补充、修订、证据缺口记录、删除。
	code, title, cat := "MAN-1", "提供项目经理社保证明", ReqNonScoringRequired
	manual, err := p.d.CreateRequirement(ctx, DefaultTenant, p.task.ID, "reviewer", RequirementInput{Code: &code, Title: &title, Category: &cat})
	if err != nil || manual.Origin != "manual" {
		t.Fatalf("CreateRequirement: %+v %v", manual, err)
	}
	if _, err := p.d.CreateRequirement(ctx, DefaultTenant, p.task.ID, "reviewer", RequirementInput{Code: &code, Title: &title, Category: &cat}); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("duplicate code: %v", err)
	}
	gap := "知识库无社保证明材料"
	if upd, err := p.d.UpdateRequirement(ctx, DefaultTenant, p.task.ID, manual.ID, "reviewer", RequirementInput{EvidenceGap: &gap}); err != nil || upd.EvidenceGap != gap {
		t.Fatalf("record gap: %+v %v", upd, err)
	}
	bogus := []int64{1 << 50}
	if _, err := p.d.UpdateRequirement(ctx, DefaultTenant, p.task.ID, manual.ID, "reviewer", RequirementInput{EvidenceIDs: &bogus}); !errors.Is(err, ErrEvidenceInvalid) {
		t.Fatalf("foreign evidence accepted: %v", err)
	}
	if err := p.d.DeleteRequirement(ctx, DefaultTenant, p.task.ID, manual.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	m, err := p.d.ConfirmRequirements(ctx, DefaultTenant, p.task.ID, "reviewer", 0)
	if err != nil || m.Status != ManifestConfirmed || m.ApprovedBy != "reviewer" {
		t.Fatalf("confirm: %+v %v", m, err)
	}
	// 原文分块被修改：要求标记 needs_revision，清单变 stale；确认前须先复核。
	if _, err := p.d.ExecContext(ctx, `UPDATE document_chunks SET content = content || '（补遗）' WHERE id = $1`, *cats[ReqMandatoryClause].SourceChunkID); err != nil {
		t.Fatal(err)
	}
	m, _ = p.d.RequirementManifest(ctx, DefaultTenant, p.task.ID)
	it, _ := p.d.GetRequirement(ctx, DefaultTenant, p.task.ID, cats[ReqMandatoryClause].ID)
	if m.Status != ManifestStale || it.ReviewStatus != ReviewNeedsRevision {
		t.Fatalf("source change not propagated: manifest=%+v item=%+v", m, it)
	}
	if _, err := p.d.ConfirmRequirements(ctx, DefaultTenant, p.task.ID, "reviewer", 0); !errors.Is(err, ErrRequirementReview) {
		t.Fatalf("confirm with needs_revision: %v", err)
	}
	// 重新提取同步新原文。
	if again, err := p.d.ExtractRequirements(ctx, DefaultTenant, p.task.ID, "reviewer"); err != nil || again.Updated != 1 {
		t.Fatalf("re-extract changed source = %+v %v", again, err)
	}
	confirmed := ReviewConfirmed
	if _, err := p.d.UpdateRequirement(ctx, DefaultTenant, p.task.ID, cats[ReqMandatoryClause].ID, "reviewer", RequirementInput{ReviewStatus: &confirmed}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.d.ConfirmRequirements(ctx, DefaultTenant, p.task.ID, "reviewer", m.Epoch-1); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("confirm with stale epoch: %v", err)
	}
	if m, err := p.d.ConfirmRequirements(ctx, DefaultTenant, p.task.ID, "reviewer", 0); err != nil || m.Status != ManifestConfirmed {
		t.Fatalf("reconfirm: %+v %v", m, err)
	}
	// 任务级联删除不应被清单触发器阻断。
	if _, err := p.d.ExecContext(ctx, `DELETE FROM tasks WHERE id = $1`, p.task.ID); err != nil {
		t.Fatalf("cascade delete task: %v", err)
	}
}

func TestOutlineValidationAndCoverage(t *testing.T) {
	p := newProposalFixture(t)
	ctx := context.Background()
	v := p.extractAndConfirm(t)
	ids := []int64{}
	for _, it := range v.Items {
		if it.InTechnicalScope() { // 否决项由投标方逐项核对，不进入大纲
			ids = append(ids, it.ID)
		}
	}
	// 依赖环、未知依赖、跨任务要求均被拒绝。
	cycle := []SectionInput{{Code: "1", Title: "A", DependsOn: []string{"2"}}, {Code: "2", Title: "B", DependsOn: []string{"1"}}}
	if _, err := p.d.CreateOutline(ctx, DefaultTenant, p.task.ID, "editor", OutlineInput{Sections: cycle}); !errors.Is(err, ErrInvalidOutline) {
		t.Fatalf("cycle accepted: %v", err)
	}
	other := newProposalFixture(t)
	ov := other.extractAndConfirm(t)
	foreign := []SectionInput{{Code: "1", Title: "A", RequirementIDs: []int64{ov.Items[0].ID}}}
	if _, err := p.d.CreateOutline(ctx, DefaultTenant, p.task.ID, "editor", OutlineInput{Sections: foreign}); !errors.Is(err, ErrInvalidOutline) {
		t.Fatalf("cross-task requirement accepted: %v", err)
	}
	// 漏映射的大纲不能提交审批；补齐后可提交。
	partial := []SectionInput{{Code: "1", Title: "总体方案", RequirementIDs: ids[:1]}, {Code: "2", Title: "其余", DependsOn: []string{"1"}}}
	det, err := p.d.CreateOutline(ctx, DefaultTenant, p.task.ID, "editor", OutlineInput{Sections: partial})
	if err != nil {
		t.Fatal(err)
	}
	if det.Coverage.Total != 4 || det.Coverage.Mapped != 1 || len(det.Coverage.Unmapped) != 3 {
		t.Fatalf("coverage = %+v", det.Coverage)
	}
	if _, err := p.d.CreateOutline(ctx, DefaultTenant, p.task.ID, "editor", OutlineInput{}); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("second open outline: %v", err)
	}
	if _, err := p.d.SubmitOutline(ctx, DefaultTenant, p.task.ID, det.Outline.Version, "editor"); !errors.Is(err, ErrOutlineIncomplete) {
		t.Fatalf("incomplete outline submitted: %v", err)
	}
	partial[1].RequirementIDs = ids[1:]
	if det, err = p.d.UpdateOutline(ctx, DefaultTenant, p.task.ID, det.Outline.Version, "editor", OutlineInput{Sections: partial}); err != nil || det.Coverage.Mapped != 4 {
		t.Fatalf("update draft: %+v %v", det, err)
	}
	if det.Sections[1].DependsOnCodes[0] != "1" {
		t.Fatalf("dependency codes = %+v", det.Sections[1])
	}
	if _, err := p.d.ApproveOutline(ctx, DefaultTenant, p.task.ID, det.Outline.Version, "approver"); !errors.Is(err, ErrOutlineState) {
		t.Fatalf("approve draft directly: %v", err)
	}
	if _, err := p.d.SubmitOutline(ctx, DefaultTenant, p.task.ID, det.Outline.Version, "editor"); err != nil {
		t.Fatal(err)
	}
}

// 场景 1：大纲处于 draft / pending_approval 时，章节派发与写回都被拒绝，且不会留下任何修订。
func TestScenario1UnapprovedOutlineBlocksWriting(t *testing.T) {
	p := newProposalFixture(t)
	ctx := context.Background()
	p.extractAndConfirm(t)
	det, err := p.d.CreateOutline(ctx, DefaultTenant, p.task.ID, "editor", OutlineInput{})
	if err != nil {
		t.Fatal(err)
	}
	ids := sectionIDs(det)
	check := func(stage string) {
		t.Helper()
		if _, err := p.d.ClaimSectionWrite(ctx, DefaultTenant, p.task.ID, ids["1"], "dispatcher", 0); !errors.Is(err, ErrOutlineNotApproved) {
			t.Fatalf("%s: claim = %v", stage, err)
		}
		for _, src := range []string{RevAIInitial, RevHumanEdit} {
			_, err := p.d.SaveSectionRevision(ctx, DefaultTenant, "w", SaveRevisionInput{TaskID: p.task.ID, SectionID: ids["1"], BaseRevNo: 0,
				BasedOnOutlineVersion: det.Outline.Version, Source: src, ContentMD: "正文"})
			if !errors.Is(err, ErrOutlineNotApproved) {
				t.Fatalf("%s: save %s = %v", stage, src, err)
			}
		}
		if s := p.section(t, ids["1"]); len(s.Revisions) != 0 || s.Section.CurrentRevNo != 0 || s.Writable {
			t.Fatalf("%s: section mutated: %+v", stage, s.Section)
		}
	}
	check("draft")
	if _, err := p.d.SubmitOutline(ctx, DefaultTenant, p.task.ID, det.Outline.Version, "editor"); err != nil {
		t.Fatal(err)
	}
	check("pending_approval")
}

// 场景 2：待审批大纲跨「重启」（关闭并重建连接池）后状态完整保留；批准后无依赖章节可派发，
// 有未完成前置依赖的章节被门禁拦截，前置章节产出有效版本后才放行。
func TestScenario2PersistentApprovalAcrossRestart(t *testing.T) {
	p := newProposalFixture(t)
	ctx := context.Background()
	dsn := os.Getenv("AUTOBID_TEST_DATABASE_URL")
	first, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.ExtractRequirements(ctx, DefaultTenant, p.task.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.ConfirmRequirements(ctx, DefaultTenant, p.task.ID, "reviewer", 0); err != nil {
		t.Fatal(err)
	}
	det, err := first.CreateOutline(ctx, DefaultTenant, p.task.ID, "editor", OutlineInput{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.SubmitOutline(ctx, DefaultTenant, p.task.ID, det.Outline.Version, "editor"); err != nil {
		t.Fatal(err)
	}
	first.Close() // 模拟服务进程退出

	second, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	restored, err := second.GetOutline(ctx, DefaultTenant, p.task.ID, det.Outline.Version)
	if err != nil || restored.Outline.Status != OutlinePending || restored.Outline.SubmittedBy != "editor" {
		t.Fatalf("pending outline not restored: %+v %v", restored, err)
	}
	ids := sectionIDs(restored)
	if _, err := second.ClaimSectionWrite(ctx, DefaultTenant, p.task.ID, ids["1"], "dispatcher", 0); !errors.Is(err, ErrOutlineNotApproved) {
		t.Fatalf("claim before approval: %v", err)
	}
	approved, err := second.ApproveOutline(ctx, DefaultTenant, p.task.ID, det.Outline.Version, "approver")
	if err != nil || approved.Outline.Status != OutlineApproved || approved.Outline.ApprovedBy != "approver" {
		t.Fatalf("approve: %+v %v", approved, err)
	}
	states := map[string]string{}
	for _, s := range approved.Sections {
		states[s.Code] = s.Status
	}
	if states["1"] != SectionReady || states["2.1"] != SectionPending {
		t.Fatalf("readiness after approval = %v", states)
	}
	if _, err := second.ClaimSectionWrite(ctx, DefaultTenant, p.task.ID, ids["2.1"], "dispatcher", 0); !errors.Is(err, ErrSectionDependencyUnmet) {
		t.Fatalf("dependent section dispatched early: %v", err)
	}
	if _, err := second.SaveSectionRevision(ctx, DefaultTenant, "w", SaveRevisionInput{TaskID: p.task.ID, SectionID: ids["2.1"], BasedOnOutlineVersion: approved.Outline.Version,
		Source: RevAIInitial, ContentMD: "抢跑"}); !errors.Is(err, ErrSectionDependencyUnmet) {
		t.Fatalf("AI write-back bypassed dependency gate: %v", err)
	}
	claim, err := second.ClaimSectionWrite(ctx, DefaultTenant, p.task.ID, ids["1"], "dispatcher", 30*time.Second)
	if err != nil || claim.BaseRevNo != 0 || claim.BasedOnOutlineVersion != approved.Outline.Version || claim.LeaseToken == 0 {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	if _, err := second.ClaimSectionWrite(ctx, DefaultTenant, p.task.ID, ids["1"], "dispatcher", 0); !errors.Is(err, ErrSectionBusy) {
		t.Fatalf("double dispatch: %v", err)
	}
	if err := second.RenewSectionLease(ctx, DefaultTenant, ids["1"], claim.LeaseToken, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	res := p.save(t, ids["1"], claim.BaseRevNo, claim.BasedOnOutlineVersion, RevAIInitial, "总体方案初稿", claim.LeaseToken)
	if res.Outcome != SaveApplied || res.Section.CurrentRevNo != 1 || res.Section.Status != SectionReviewing || res.Section.Writing {
		t.Fatalf("write-back: %+v", res.Section)
	}
	c2, err := second.ClaimSectionWrite(ctx, DefaultTenant, p.task.ID, ids["2.1"], "dispatcher", 0)
	if err != nil {
		t.Fatalf("dependent section still blocked: %v", err)
	}
	if err := second.ReleaseSectionClaim(ctx, DefaultTenant, ids["2.1"], c2.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if s := p.section(t, ids["2.1"]); s.Section.Writing || s.Section.Status != SectionReady {
		t.Fatalf("release: %+v", s.Section)
	}
}

// 场景 3：v1 批准后 Worker 领取（v1, base 0）；人工修改要求清单或目录使大纲升至 v2，章节 current_rev_no 仍为 0；
// 旧 Worker 写回被归档为 stale_outline，绝不成为当前主版本。
func TestScenario3InflightOutlineInvalidation(t *testing.T) {
	for _, trigger := range []string{"requirement", "outline"} {
		t.Run(trigger, func(t *testing.T) {
			p := newProposalFixture(t)
			ctx := context.Background()
			det, ids := p.approvedOutline(t)
			claim, err := p.d.ClaimSectionWrite(ctx, DefaultTenant, p.task.ID, ids["1"], "dispatcher", 0)
			if err != nil || claim.BasedOnOutlineVersion != 1 || claim.BaseRevNo != 0 {
				t.Fatalf("claim: %+v %v", claim, err)
			}
			if trigger == "requirement" {
				reqs, _ := p.d.ListRequirements(ctx, DefaultTenant, p.task.ID)
				title := reqs.Items[0].Title + "（人工修订）"
				if _, err := p.d.UpdateRequirement(ctx, DefaultTenant, p.task.ID, reqs.Items[0].ID, "reviewer", RequirementInput{Title: &title}); err != nil {
					t.Fatal(err)
				}
			} else {
				secs := []SectionInput{}
				for _, s := range det.Sections {
					secs = append(secs, SectionInput{Code: s.Code, Title: s.Title, RequirementIDs: s.RequirementIDs, DependsOn: s.DependsOnCodes})
				}
				secs[0].Title = "总体技术方案（调整）"
				if _, err := p.d.UpdateOutline(ctx, DefaultTenant, p.task.ID, 1, "editor", OutlineInput{Sections: secs}); err != nil {
					t.Fatal(err)
				}
			}
			list, err := p.d.ListOutlines(ctx, DefaultTenant, p.task.ID)
			if err != nil || len(list.Outlines) != 2 || list.Outlines[0].Version != 2 || list.Outlines[0].Status != OutlineDraft ||
				list.Outlines[1].Status != OutlineSuperseded || list.ApprovedVersion != 0 {
				t.Fatalf("outline versions after change: %+v %v", list, err)
			}
			if s := p.section(t, ids["1"]); s.Section.CurrentRevNo != 0 || s.Section.Status != SectionStaleOutline {
				t.Fatalf("v1 section before write-back: %+v", s.Section)
			}
			res := p.save(t, ids["1"], claim.BaseRevNo, claim.BasedOnOutlineVersion, RevAIInitial, "基于 v1 的在途初稿", claim.LeaseToken)
			if res.Outcome != SaveStale || res.Revision.Status != RevStaleOut || res.Section.CurrentRevNo != 0 {
				t.Fatalf("stale write-back promoted: %+v", res)
			}
			// 新大纲重新确认、批准后，旧 Worker 重试仍是 stale；v2 章节从 rev 0 开始。
			if _, err := p.d.ConfirmRequirements(ctx, DefaultTenant, p.task.ID, "reviewer", 0); err != nil {
				t.Fatal(err)
			}
			if _, err := p.d.SubmitOutline(ctx, DefaultTenant, p.task.ID, 2, "editor"); err != nil {
				t.Fatal(err)
			}
			v2, err := p.d.ApproveOutline(ctx, DefaultTenant, p.task.ID, 2, "approver")
			if err != nil {
				t.Fatal(err)
			}
			if res := p.save(t, ids["1"], 0, 1, RevAIInitial, "重试", claim.LeaseToken); res.Outcome != SaveStale {
				t.Fatalf("retry after v2 approval: %+v", res)
			}
			ids2 := sectionIDs(v2)
			if s := p.section(t, ids2["1"]); s.Section.CurrentRevNo != 0 || s.Section.DerivedFromSectionID == nil || *s.Section.DerivedFromSectionID != ids["1"] {
				t.Fatalf("v2 section: %+v", s.Section)
			}
			if _, err := p.d.ClaimSectionWrite(ctx, DefaultTenant, p.task.ID, ids["1"], "dispatcher", 0); !errors.Is(err, ErrOutlineStale) {
				t.Fatalf("claim on superseded section: %v", err)
			}
		})
	}
}

// 场景 4：章节 rev 2；滞后 AI（base 2）运行期间人工保存 rev 3；AI 写回只能成为 conflict_candidate，
// current_rev_no 与正文保持人工的 rev 3。
func TestScenario4HumanEditNeverOverwrittenByLaggingAI(t *testing.T) {
	p := newProposalFixture(t)
	ctx := context.Background()
	_, ids := p.approvedOutline(t)
	sid := ids["1"]
	p.save(t, sid, 0, 1, RevHumanEdit, "rev1", 0)
	p.save(t, sid, 1, 1, RevHumanEdit, "rev2", 0)
	claim, err := p.d.ClaimSectionWrite(ctx, DefaultTenant, p.task.ID, sid, "dispatcher", 0)
	if err != nil || claim.BaseRevNo != 2 {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	human := p.save(t, sid, 2, 1, RevHumanEdit, "人工修改的 rev3", 0)
	if human.Outcome != SaveApplied || human.Revision.RevNo != 3 || !human.Section.Writing {
		t.Fatalf("human save: %+v", human)
	}
	ai := p.save(t, sid, claim.BaseRevNo, claim.BasedOnOutlineVersion, RevAIRewrite, "滞后 AI 重写", claim.LeaseToken)
	if ai.Outcome != SaveConflict || ai.Revision.Status != RevConflict || ai.Section.CurrentRevNo != 3 || ai.Section.Writing {
		t.Fatalf("lagging AI result: %+v / %+v", ai.Revision, ai.Section)
	}
	det := p.section(t, sid)
	if det.Current == nil || det.Current.RevNo != 3 || det.Current.ContentMD != "人工修改的 rev3" || len(det.Revisions) != 4 {
		t.Fatalf("human content overwritten: %+v", det.Current)
	}
	// 并发写回：同一 base 只有一个能成为主版本，其余全部为冲突候选。
	var wg sync.WaitGroup
	results := make([]*SaveRevisionResult, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = p.d.SaveSectionRevision(ctx, DefaultTenant, fmt.Sprintf("editor-%d", i), SaveRevisionInput{TaskID: p.task.ID,
				SectionID: sid, BaseRevNo: 3, BasedOnOutlineVersion: 1, Source: RevHumanEdit, ContentMD: fmt.Sprintf("并发编辑 %d", i)})
		}(i)
	}
	wg.Wait()
	applied, winner := 0, 0
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("concurrent save %d: %v", i, errs[i])
		}
		if r.Outcome == SaveApplied {
			applied++
			winner = r.Revision.RevNo
		} else if r.Outcome != SaveConflict {
			t.Fatalf("unexpected outcome %+v", r)
		}
	}
	if cur := p.section(t, sid).Section.CurrentRevNo; applied != 1 || cur != winner {
		t.Fatalf("applied=%d current=%d winner=%d", applied, cur, winner)
	}
}

// 场景 5：批准 rev 3 -> 新修订 rev 4 自动撤销批准 -> 回滚至 rev 2 生成 rev 5 且内容恢复；修订不可篡改。
func TestScenario5ApprovalBindingRevocationAndRollback(t *testing.T) {
	p := newProposalFixture(t)
	ctx := context.Background()
	_, ids := p.approvedOutline(t)
	sid := ids["1"]
	p.save(t, sid, 0, 1, RevHumanEdit, "rev1", 0)
	p.save(t, sid, 1, 1, RevHumanEdit, "rev2 内容", 0)
	p.save(t, sid, 2, 1, RevHumanEdit, "rev3 内容", 0)
	if _, err := p.d.ApproveSectionRevision(ctx, DefaultTenant, p.task.ID, sid, 2, "approver"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("approve non-current revision: %v", err)
	}
	sec, err := p.d.ApproveSectionRevision(ctx, DefaultTenant, p.task.ID, sid, 3, "approver")
	if err != nil || sec.ApprovedRevNo == nil || *sec.ApprovedRevNo != 3 || sec.Status != SectionApproved || sec.ApprovedBy != "approver" {
		t.Fatalf("approve rev3: %+v %v", sec, err)
	}
	claim, err := p.d.ClaimSectionWrite(ctx, DefaultTenant, p.task.ID, sid, "dispatcher", 0)
	if err != nil {
		t.Fatal(err)
	}
	rw := p.save(t, sid, claim.BaseRevNo, claim.BasedOnOutlineVersion, RevAIRewrite, "rev4 局部重写", claim.LeaseToken)
	if rw.Revision.RevNo != 4 || rw.Section.ApprovedRevNo != nil || rw.Section.Status != SectionReviewing {
		t.Fatalf("rewrite did not revoke approval: %+v", rw.Section)
	}
	base := 3
	if _, err := p.d.RollbackSectionRevision(ctx, DefaultTenant, p.task.ID, sid, 2, &base, "editor"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("rollback with stale base: %v", err)
	}
	rb, err := p.d.RollbackSectionRevision(ctx, DefaultTenant, p.task.ID, sid, 2, nil, "editor")
	if err != nil {
		t.Fatal(err)
	}
	if rb.Revision.RevNo != 5 || rb.Revision.Source != RevRollback || *rb.Revision.RollbackOfRevNo != 2 || rb.Revision.ContentMD != "rev2 内容" ||
		rb.Section.CurrentRevNo != 5 || rb.Section.ApprovedRevNo != nil {
		t.Fatalf("rollback: %+v / %+v", rb.Revision, rb.Section)
	}
	if _, err := p.d.ExecContext(ctx, `UPDATE section_revisions SET content_md = 'tampered' WHERE tenant_id = $1 AND section_id = $2 AND rev_no = 3`,
		DefaultTenant, sid); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("revision content mutable: %v", err)
	}
	hist := p.section(t, sid).Revisions
	if len(hist) != 5 || hist[2].ContentMD != "rev3 内容" || hist[2].Status != RevApproved {
		t.Fatalf("history = %+v", hist)
	}
	if sec, err := p.d.ApproveSectionRevision(ctx, DefaultTenant, p.task.ID, sid, 5, "approver"); err != nil || *sec.ApprovedRevNo != 5 {
		t.Fatalf("approve rollback rev: %+v %v", sec, err)
	}
}

// 场景 6：章节修订绑定的 retrieval_evidence 因知识库文档删除而变为 stale 时，修订与章节自动降级为 stale_evidence、
// 撤销批准，且不能作为定稿批准；未核验证据自动记为缺口。
func TestScenario6EvidenceStalePropagation(t *testing.T) {
	p := newProposalFixture(t)
	ctx := context.Background()
	_, ids := p.approvedOutline(t)
	sid := ids["1"]
	kdoc := &KnowledgeDocument{TenantID: DefaultTenant, Category: KnowledgeProductSpec, Title: uniq("P5 产品规格"), RawText: uniq("恢复时间 30 秒")}
	kid, _, err := p.d.InsertKnowledgeDocument(ctx, kdoc, []KnowledgeChunk{{Seq: 0, Content: "恢复时间：30秒", PageStart: 1, PageEnd: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := p.d.KnowledgeChunks(ctx, DefaultTenant, kid, false)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("chunks: %v %v", chunks, err)
	}
	taskID := p.task.ID
	run, _, err := p.d.CreateRetrievalRun(ctx, RetrievalRun{TenantID: DefaultTenant, TaskID: &taskID, RequestKey: uniq("p5"), Request: json.RawMessage(`{}`),
		Kind: "search", MaxCalls: 1, MaxTokens: 1, Deadline: time.Now().Add(time.Hour)}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	verified := &RetrievalEvidence{RunID: run.ID, Domain: "knowledge", SourceID: chunks[0].ID, DocID: kid, Hash: "h", Snapshot: json.RawMessage(`{}`),
		Claim: json.RawMessage(`{"text":"恢复时间≤30秒"}`), Relation: "supports", Status: "verified"}
	if err := p.d.SaveRetrievalEvidence(ctx, DefaultTenant, "tester", verified); err != nil {
		t.Fatal(err)
	}
	// 未核验证据：可保存但自动记为缺口。
	other := &RetrievalEvidence{RunID: run.ID, Domain: "knowledge", SourceID: chunks[0].ID + 1<<40, DocID: kid, Hash: "h2", Snapshot: json.RawMessage(`{}`),
		Relation: "insufficient", Status: "unverified"}
	if err := p.d.SaveRetrievalEvidence(ctx, DefaultTenant, "tester", other); err != nil {
		t.Fatal(err)
	}
	reqs, _ := p.d.ListRequirements(ctx, DefaultTenant, p.task.ID)
	evIDs := []int64{verified.ID}
	if _, err := p.d.UpdateRequirement(ctx, DefaultTenant, p.task.ID, reqs.Items[0].ID, "reviewer", RequirementInput{EvidenceRunID: &run.ID, EvidenceIDs: &evIDs}); err != nil {
		t.Fatal(err)
	}
	if o, _ := p.d.ListOutlines(ctx, DefaultTenant, p.task.ID); o.ApprovedVersion != 1 {
		t.Fatalf("evidence binding must not supersede the outline: %+v", o)
	}
	res := p.save(t, sid, 0, 1, RevHumanEdit, "系统恢复时间不超过 30 秒。", 0, verified.ID, other.ID)
	if res.Outcome != SaveApplied || len(res.Revision.Gaps) != 1 || res.Revision.Gaps[0].EvidenceID != other.ID || res.Revision.Gaps[0].Kind != "unverified" {
		t.Fatalf("auto gaps: %+v", res.Revision)
	}
	if _, err := p.d.ApproveSectionRevision(ctx, DefaultTenant, p.task.ID, sid, 1, "approver"); err != nil {
		t.Fatalf("approve with verified evidence: %v", err)
	}
	if err := p.d.DeleteKnowledgeDocument(ctx, DefaultTenant, kid); err != nil {
		t.Fatal(err)
	}
	det := p.section(t, sid)
	if det.Section.Status != SectionStaleEvidence || det.Section.ApprovedRevNo != nil || det.Current.Status != RevStaleEvid || len(det.Warnings) == 0 {
		t.Fatalf("stale evidence not propagated: %+v / %+v / %v", det.Section, det.Current, det.Warnings)
	}
	if it, _ := p.d.GetRequirement(ctx, DefaultTenant, p.task.ID, reqs.Items[0].ID); !strings.Contains(it.EvidenceGap, "stale") {
		t.Fatalf("requirement gap not recorded: %q", it.EvidenceGap)
	}
	if _, err := p.d.ApproveSectionRevision(ctx, DefaultTenant, p.task.ID, sid, 1, "approver"); !errors.Is(err, ErrEvidenceInvalid) {
		t.Fatalf("stale evidence approved: %v", err)
	}
	// 保存新修订时引用失效证据：直接降级为 stale_evidence；去掉失效证据后才可批准。
	if r := p.save(t, sid, 1, 1, RevHumanEdit, "仍引用失效证据", 0, verified.ID); r.Revision.Status != RevStaleEvid {
		t.Fatalf("save with stale evidence: %+v", r.Revision)
	}
	fixed := p.save(t, sid, 2, 1, RevHumanEdit, "恢复时间指标待补充证据。", 0)
	if fixed.Section.Status != SectionDraft {
		t.Fatalf("fixed: %+v", fixed.Section)
	}
	if _, err := p.d.ApproveSectionRevision(ctx, DefaultTenant, p.task.ID, sid, 3, "approver"); err != nil {
		t.Fatal(err)
	}
}
