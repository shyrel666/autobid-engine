package ingest

// Phase 5 真实语料集成测试：对 eval/bid/corpus.json 登记的公开开发集逐个校验 SHA-256 并执行
// 解析入库 -> 要求清单提取（评分项 / ★ 强制项 / 投标格式要求）-> 幂等重提 -> 清单确认 -> 默认大纲生成 -> 提交 / 批准 ->
// 首章节派发门禁，验证真实文件上解析与状态机全链路无报错。目录不存在（语料未下载）或未设置 AUTOBID_TEST_DATABASE_URL 时跳过。
// 本测试只证明链路可运行与清单结构完整，不代表提取的召回率或业务正确性已验收（清单仍须人工复核）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
)

const ccgpCorpusDir = "ccgp_2026-10-06"

type ccgpTender struct {
	Key                string    `json:"key"`
	ProjectNo          string    `json:"project_no"`
	Family             string    `json:"family"`
	LocalFile          string    `json:"local_file"`
	SHA256             string    `json:"sha256"`
	Lots               []string  `json:"lots"`
	Deadline           time.Time `json:"bid_deadline"`
	DeadlineInOriginal *bool     `json:"deadline_in_original"`
	ReviewTable        *bool     `json:"has_review_table"`
	Profile            string    `json:"regression_profile"`
}

func ccgpTenders(t *testing.T) (*os.Root, []ccgpTender) {
	t.Helper()
	root, err := os.OpenRoot(filepath.Join("..", ccgpCorpusDir))
	if os.IsNotExist(err) {
		t.Skipf("%s not present; real-tender corpus test skipped", ccgpCorpusDir)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	data, err := os.ReadFile("../eval/bid/corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Version  string       `json:"version"`
		Projects []ccgpTender `json:"projects"`
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	if corpus.Version == "" || len(corpus.Projects) < 30 {
		t.Fatalf("CCGP development corpus needs a version and at least 30 projects; got %d", len(corpus.Projects))
	}
	keys := make(map[string]bool, len(corpus.Projects))
	numbers := make(map[string]bool, len(corpus.Projects))
	files := make(map[string]bool, len(corpus.Projects))
	hashes := make(map[string]bool, len(corpus.Projects))
	for i := range corpus.Projects {
		project := &corpus.Projects[i]
		file := filepath.Clean(project.LocalFile)
		ext := strings.ToLower(filepath.Ext(file))
		relative, relErr := filepath.Rel(ccgpCorpusDir, file)
		hash, hashErr := hex.DecodeString(project.SHA256)
		if project.Key == "" || keys[project.Key] || project.ProjectNo == "" || numbers[project.ProjectNo] ||
			project.Family == "" || files[file] || hashes[project.SHA256] ||
			relErr != nil || !filepath.IsLocal(relative) || (ext != ".docx" && ext != ".pdf") ||
			hashErr != nil || len(hash) != sha256.Size {
			t.Fatalf("invalid or duplicate development corpus entry: %+v", project)
		}
		switch project.Profile {
		case "baseline":
			if project.ReviewTable == nil {
				t.Fatalf("baseline review table expectation missing for %s", project.Key)
			}
		case "expanded":
		default:
			t.Fatalf("unknown regression profile %q for %s", project.Profile, project.Key)
		}
		if project.Deadline.IsZero() && (project.DeadlineInOriginal == nil || *project.DeadlineInOriginal) {
			t.Fatalf("deadline expectation missing for %s", project.Key)
		}
		keys[project.Key], files[file] = true, true
		numbers[project.ProjectNo], hashes[project.SHA256] = true, true
		project.LocalFile = relative
	}
	t.Logf("development corpus %s: %d registered projects, manifest SHA-256 %x", corpus.Version, len(corpus.Projects), sha256.Sum256(data))
	return root, corpus.Projects
}

func TestCCGPRealTendersRequirementsAndOutline(t *testing.T) {
	root, projects := ccgpTenders(t)
	d := testDB(t)
	scoreExpectations := loadAuditScores(t)
	proofExpectations := loadCCGPProofExpectations(t, projects)
	totals := map[string]int{}
	for _, project := range projects {
		name := filepath.Base(project.LocalFile)
		t.Run(project.Key, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			file, err := root.Open(project.LocalFile)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			data, err := io.ReadAll(file)
			if err != nil {
				t.Fatal(err)
			}
			if actual := fmt.Sprintf("%x", sha256.Sum256(data)); actual != project.SHA256 {
				t.Fatalf("%s SHA-256 mismatch: got %s, want %s", name, actual, project.SHA256)
			}
			// 本用例验证解析、要求与大纲，写章节一步不建故事板，显式沿用旧写作门禁。
			task, err := d.CreateTask(ctx, db.CreateTaskInput{Name: "ccgp-" + name, Goal: "真实招标文件要求清单与大纲", LegacyWriting: true})
			if err != nil {
				t.Fatal(err)
			}
			defer d.ExecContext(context.Background(), `DELETE FROM tasks WHERE id = $1`, task.ID) //nolint:errcheck

			res, err := Ingest(ctx, d, db.NewAssetStore(d, task.ID), Input{TenderNo: fmt.Sprintf("CCGP-%s-%d", project.Key, time.Now().UnixNano()),
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
			checkAuditScores(t, project.Key, view, scoreExpectations[project.Key])
			checkCCGPProofExpectations(t, ctx, d, task.ID, res, view, proofExpectations[project.Key])
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
			if project.Profile == "baseline" && (by[db.ReqScoringItem] == 0 || by[db.ReqFormatAttachment] == 0) {
				t.Errorf("baseline tender must yield scoring and format requirements: %v", by)
			}
			if res.Mandatory > 0 && by[db.ReqMandatoryClause] == 0 {
				t.Errorf("%d mandatory chunks but no mandatory_clause requirement", res.Mandatory)
			}
			price, statutory, reviewRows := 0, 0, 0
			for _, it := range view.Items {
				if it.VetoKind == db.VetoStatutory {
					statutory++
					continue
				}
				if it.SourceChunkID == nil || it.SourceHash == "" || it.Origin != "extracted" {
					t.Fatalf("requirement without versioned source: %+v", it)
				}
				if it.Category == db.ReqVetoItem {
					reviewRows++
					// 旧基线的审查表全包适用，扩充语料可有分包限定的审查表。
					if (it.VetoKind == db.VetoQualification) != strings.HasPrefix(it.Code, "VQ-") || it.InTechnicalScope() ||
						it.VetoStatus != db.VetoPending || (project.Profile == "baseline" && it.Lot != "") {
						t.Errorf("unexpected review veto item: %s %s lot=%q", it.Code, it.VetoKind, it.Lot)
					}
				}
				if it.Category == db.ReqScoringItem && project.Profile == "baseline" {
					if strings.Contains(it.Description, "序号") && strings.Contains(it.Description, "分值") || it.ScoreGroup == "" || it.ScoreWeight <= 0 {
						t.Errorf("unexpected scoring item: %s %s score=%g", it.Code, it.ScoreGroup, it.ScoreWeight)
					}
				}
				if it.Category == db.ReqScoringItem && it.ScoreGroup == db.ScoreGroupPrice {
					price++
				}
			}
			if statutory != 6 || project.ReviewTable != nil && (reviewRows > 0) != *project.ReviewTable {
				t.Errorf("D1: statutory=%d, review rows=%d", statutory, reviewRows)
			}
			if project.DeadlineInOriginal != nil && !*project.DeadlineInOriginal {
				if view.Manifest.BidDeadline != nil {
					t.Errorf("D1 deadline: original has no explicit deadline, got %v (%s)", view.Manifest.BidDeadline, view.Manifest.BidDeadlineSource)
				}
			} else if view.Manifest.BidDeadline == nil || !view.Manifest.BidDeadline.Equal(project.Deadline) {
				t.Errorf("D1 deadline: got %v (%s), want %s", view.Manifest.BidDeadline, view.Manifest.BidDeadlineSource, project.Deadline.Format(time.RFC3339))
			}
			t.Logf("%s: veto items %d (review rows %d) deadline %v", name, statutory+reviewRows, reviewRows, view.Manifest.BidDeadline)
			if project.Profile == "baseline" && (len(view.Scoring) != max(1, len(project.Lots)) || price != len(view.Scoring)) {
				t.Errorf("expected one price item per lot: price=%d, scoring lots=%d", price, len(view.Scoring))
			}
			for _, s := range view.Scoring {
				if project.Profile == "baseline" && (s.Total != 100 || s.Price <= 0 || s.Technical <= 0 || s.Other != 0) {
					t.Errorf("lot %q scoring total %+v", s.Lot, s)
				}
				t.Logf("%s: lot %q items=%d technical=%.4g commercial=%.4g price=%.4g warnings=%v", name, s.Lot, s.Items, s.Technical, s.Commercial, s.Price, s.Warnings)
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
			// 价格评分项由报价文件响应、否决项由投标方逐项核对，都不进入技术标目录；其余要求全部映射。
			if outline.Coverage.Total+outline.Coverage.OutOfScope+view.Stats.Veto.Total != view.Stats.Total || outline.Coverage.OutOfScope != price ||
				len(outline.Coverage.Unmapped) != 0 || len(outline.Sections) < 2 {
				t.Fatalf("default outline must map every in-scope requirement: sections=%d coverage=%+v", len(outline.Sections), outline.Coverage)
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
				t.Fatalf("readiness: ready=%d pending=%d first present=%t", ready, pending, first != nil)
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

type auditScore struct {
	Name       string    `json:"name"`
	Points     float64   `json:"points"`
	Group      string    `json:"group"`
	ScoreGroup string    `json:"score_group"`
	EvalKind   string    `json:"eval_kind"`
	Conflict   []float64 `json:"conflict"`
}

type auditScoreScope struct {
	Lots  []string     `json:"lots"`
	Items []auditScore `json:"items"`
}

func loadAuditScores(t *testing.T) map[string][]auditScoreScope {
	t.Helper()
	raw, err := os.ReadFile("../eval/bid/fixtures/d1-scoring.json")
	if err != nil {
		t.Fatal(err)
	}
	var scores map[string][]auditScoreScope
	if err := json.Unmarshal(raw, &scores); err != nil {
		t.Fatal(err)
	}
	if len(scores) != 18 {
		t.Fatalf("want 18 audited score projects, got %d", len(scores))
	}
	return scores
}

func checkAuditScores(t *testing.T, project string, view *db.RequirementsView, scopes []auditScoreScope) {
	t.Helper()
	if len(scopes) == 0 {
		if project == "P10" {
			checkScoreWarnings(t, view.Scoring, "包C", "声明 60 分", "已提取 50 分")
		}
		return
	}
	expected := make(map[string]map[string]auditScore)
	for _, scope := range scopes {
		for _, lot := range scope.Lots {
			expected[lot] = make(map[string]auditScore)
			for _, item := range scope.Items {
				expected[lot][item.Name] = item
			}
		}
	}
	for _, item := range view.Items {
		if item.Category != db.ReqScoringItem || item.IsSuperseded() || item.ParentID != nil {
			continue
		}
		name := strings.TrimPrefix(item.Title, "【"+db.LotLabel(item.Lot)+"】")
		want, ok := expected[item.Lot][name]
		if !ok {
			t.Errorf("unexpected audited score item %q in package %q", name, item.Lot)
			continue
		}
		delete(expected[item.Lot], name)
		var loc struct {
			Lot      string    `json:"lot"`
			Group    string    `json:"group"`
			Conflict []float64 `json:"score_conflict"`
		}
		if err := json.Unmarshal(item.SourceLoc, &loc); err != nil {
			t.Fatal(err)
		}
		if item.SourceChunkID == nil || loc.Lot != item.Lot || item.ScoreWeight != want.Points ||
			item.ScoreGroup != want.ScoreGroup || loc.Group != want.Group || !slices.Equal(loc.Conflict, want.Conflict) ||
			(want.EvalKind != "" && item.EvalKind != want.EvalKind) {
			t.Errorf("score %q in package %q = %g / %q / %q / %q / %v, want %+v",
				name, item.Lot, item.ScoreWeight, item.ScoreGroup, loc.Group, item.EvalKind, loc.Conflict, want)
		}
	}
	for lot, missing := range expected {
		if len(missing) > 0 {
			t.Errorf("missing audited score items in package %q: %v", lot, missing)
		}
	}
	if len(view.Scoring) != len(expected) {
		t.Errorf("scoring summaries = %d, want %d packages", len(view.Scoring), len(expected))
	}
	if project == "P35" {
		checkScoreWarnings(t, view.Scoring, "原件分值冲突", "20 分 / 30 分", "未计入合计")
		if len(view.Scoring) == 1 && (view.Scoring[0].Total != 80 || view.Scoring[0].Price != 0) {
			t.Errorf("unresolved price counted in P35 summary: %+v", view.Scoring[0])
		}
	} else {
		for _, lot := range view.Scoring {
			if _, ok := expected[lot.Lot]; !ok || lot.Total != 100 || len(lot.Warnings) != 0 {
				t.Errorf("audited score package: %+v", lot)
			}
		}
	}
}

func checkScoreWarnings(t *testing.T, summaries []db.ScoringSummary, phrases ...string) {
	t.Helper()
	var warnings []string
	for _, summary := range summaries {
		warnings = append(warnings, summary.Warnings...)
	}
	if len(warnings) != 1 {
		t.Errorf("expected exactly one source conflict warning, got %v", warnings)
		return
	}
	for _, phrase := range phrases {
		if !strings.Contains(warnings[0], phrase) {
			t.Errorf("expected %q in source conflict warning %q", phrase, warnings[0])
		}
	}
}
