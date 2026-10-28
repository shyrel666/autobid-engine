package ingest

// Phase 5 真实语料集成测试：对 ccgp_2026-10-06/ 下的真实政府采购招标文件（.docx / .pdf）逐个执行
// 解析入库 -> 要求清单提取（评分项 / ★ 强制项 / 投标格式要求）-> 幂等重提 -> 清单确认 -> 默认大纲生成 -> 提交 / 批准 ->
// 首章节派发门禁，验证真实文件上解析与状态机全链路无报错。目录不存在（语料未下载）或未设置 AUTOBID_TEST_DATABASE_URL 时跳过。
// 本测试只证明链路可运行与清单结构完整，不代表提取的召回率或业务正确性已验收（清单仍须人工复核）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
)

const ccgpCorpusDir = "../ccgp_2026-10-06"

func ccgpTenderFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(ccgpCorpusDir)
	if os.IsNotExist(err) {
		t.Skipf("%s not present; real-tender corpus test skipped", ccgpCorpusDir)
	}
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if !e.IsDir() && (ext == ".docx" || ext == ".pdf") {
			files = append(files, filepath.Join(ccgpCorpusDir, e.Name()))
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		t.Skipf("%s contains no .docx/.pdf files", ccgpCorpusDir)
	}
	return files
}

func TestCCGPRealTendersRequirementsAndOutline(t *testing.T) {
	files := ccgpTenderFiles(t)
	d := testDB(t)
	totals := map[string]int{}
	for i, path := range files {
		name := filepath.Base(path)
		t.Run(fmt.Sprintf("%02d_%s", i+1, strings.TrimSuffix(name, filepath.Ext(name))), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			task, err := d.CreateTask(ctx, db.CreateTaskInput{Name: "ccgp-" + name, Goal: "真实招标文件要求清单与大纲"})
			if err != nil {
				t.Fatal(err)
			}
			defer d.ExecContext(context.Background(), `DELETE FROM tasks WHERE id = $1`, task.ID) //nolint:errcheck

			res, err := Ingest(ctx, d, db.NewAssetStore(d, task.ID), Input{TenderNo: fmt.Sprintf("CCGP-%d-%d", i+1, time.Now().UnixNano()),
				File: &File{Name: name, Data: data}, CreatedBy: "ccgp-test"})
			if err != nil {
				t.Fatalf("ingest %s: %v", name, err)
			}
			if res.Chunks == 0 {
				t.Fatalf("ingest %s produced no chunks: %+v", name, res)
			}

			ext, err := d.ExtractRequirements(ctx, db.DefaultTenant, task.ID, "ccgp-test")
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			view, err := d.ListRequirements(ctx, db.DefaultTenant, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			by := view.Stats.ByCategory
			t.Logf("%s: format=%s pages=%d chunks=%d mandatory_chunks=%d tables=%d -> requirements=%d (scoring=%d mandatory=%d required=%d format=%d)",
				name, res.Format, res.PageCount, res.Chunks, res.Mandatory, res.Tables, view.Stats.Total,
				by[db.ReqScoringItem], by[db.ReqMandatoryClause], by[db.ReqNonScoringRequired], by[db.ReqFormatAttachment])
			for k, v := range by {
				totals[k] += v
			}
			if ext.Scanned != res.Chunks || ext.Created != view.Stats.Total {
				t.Fatalf("extract must traverse every chunk: %+v vs ingest %+v / total %d", ext, res, view.Stats.Total)
			}
			if by[db.ReqScoringItem] == 0 || by[db.ReqFormatAttachment] == 0 {
				t.Fatalf("real tender must yield scoring and format requirements: %v", by)
			}
			if res.Mandatory > 0 && by[db.ReqMandatoryClause] == 0 {
				t.Fatalf("%d mandatory chunks but no mandatory_clause requirement", res.Mandatory)
			}
			for _, it := range view.Items {
				if it.SourceChunkID == nil || it.SourceHash == "" || it.Origin != "extracted" {
					t.Fatalf("requirement without versioned source: %+v", it)
				}
				if it.Category == db.ReqScoringItem && strings.Contains(it.Description, "序号") && strings.Contains(it.Description, "分值") {
					t.Fatalf("scoring table header row extracted as a requirement: %q", it.Description)
				}
			}
			// 幂等：原文未变时重提不新增、不递增 epoch。
			again, err := d.ExtractRequirements(ctx, db.DefaultTenant, task.ID, "ccgp-test")
			if err != nil || again.Created != 0 || again.Updated != 0 || again.Unchanged != view.Stats.Total {
				t.Fatalf("re-extract not idempotent: %+v %v", again, err)
			}

			if _, err := d.ConfirmRequirements(ctx, db.DefaultTenant, task.ID, "ccgp-test", view.Manifest.Epoch); err != nil {
				t.Fatalf("confirm: %v", err)
			}
			outline, err := d.CreateOutline(ctx, db.DefaultTenant, task.ID, "ccgp-test", db.OutlineInput{Note: "按要求清单生成"})
			if err != nil {
				t.Fatalf("create outline: %v", err)
			}
			if outline.Coverage.Total != view.Stats.Total || len(outline.Coverage.Unmapped) != 0 || len(outline.Sections) < 2 {
				t.Fatalf("default outline must map every requirement: sections=%d coverage=%+v", len(outline.Sections), outline.Coverage)
			}
			if _, err := d.SubmitOutline(ctx, db.DefaultTenant, task.ID, outline.Outline.Version, "ccgp-test"); err != nil {
				t.Fatalf("submit: %v", err)
			}
			approved, err := d.ApproveOutline(ctx, db.DefaultTenant, task.ID, outline.Outline.Version, "ccgp-approver")
			if err != nil {
				t.Fatalf("approve: %v", err)
			}
			ready, pending := 0, 0
			var first, dependent *db.ProposalSection
			for _, s := range approved.Sections {
				switch s.Status {
				case db.SectionReady:
					ready++
				case db.SectionPending:
					pending++
				default:
					t.Fatalf("unexpected section status after approval: %+v", s)
				}
				if s.Code == "1" {
					first = s
				} else if dependent == nil && len(s.DependsOn) > 0 {
					dependent = s
				}
			}
			if first == nil || first.Status != db.SectionReady || dependent == nil || ready != 1 || pending != len(approved.Sections)-1 {
				t.Fatalf("readiness: ready=%d pending=%d first=%+v", ready, pending, first)
			}
			if _, err := d.ClaimSectionWrite(ctx, db.DefaultTenant, task.ID, dependent.ID, "ccgp-test", 0); err == nil {
				t.Fatal("dependent section dispatched before its prerequisite")
			}
			claim, err := d.ClaimSectionWrite(ctx, db.DefaultTenant, task.ID, first.ID, "ccgp-test", 0)
			if err != nil {
				t.Fatalf("claim first section: %v", err)
			}
			if err := d.ReleaseSectionClaim(ctx, db.DefaultTenant, first.ID, claim.LeaseToken); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: outline v%d sections=%d approved; first section ready, %d gated by dependencies", name, approved.Outline.Version, len(approved.Sections), pending)
		})
	}
	t.Logf("corpus totals: %v", totals)
}
