package main

// D3 检查项、门禁结果、黄金期望与两轮归一化比较模型。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/autobid/autobid-engine/db"
)

const (
	statusPass    = "pass"
	statusFail    = "fail"
	statusBlocked = "blocked"
	actor         = "d3-author"
	pinkReviewer  = "d3-pink-reviewer"
	redReviewer   = "d3-red-reviewer"
	signer        = "d3-delivery-lead"
)

type Check struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type Stage struct {
	ID     string  `json:"id"`
	Title  string  `json:"title"`
	Status string  `json:"status"`
	Checks []Check `json:"checks"`
}

type Gate struct {
	ID         string  `json:"id"`
	Gate       string  `json:"gate"`
	SignalType string  `json:"signal_type"` // write_rejected | readiness_blocker
	Sentinel   string  `json:"sentinel,omitempty"`
	Trigger    string  `json:"trigger"`
	Observed   string  `json:"observed"`
	Remediated bool    `json:"remediated"`
	FullReady  bool    `json:"full_ready,omitempty"`
	Checks     []Check `json:"checks,omitempty"`
	Error      string  `json:"error,omitempty"`
}

type Artifact struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Note   string `json:"note,omitempty"`
}

type Deferred struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type Determinism struct {
	Enabled      bool     `json:"enabled"`
	Consistent   bool     `json:"consistent"`
	Differences  []string `json:"differences"`
	SecondFailed int      `json:"second_failed"`
}

type CleanupInfo struct {
	Mode   string `json:"mode"`
	Tasks  int    `json:"tasks"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type goldenSpec struct {
	Status                 string             `json:"_status"`
	FixtureVersion         int                `json:"fixture_version"`
	TenderNo               string             `json:"tender_no"`
	BidDeadline            string             `json:"bid_deadline"`
	Company                string             `json:"company"`
	Evidence               map[string]int     `json:"evidence"`
	Requirements           map[string]int     `json:"requirements"`
	Admissibility          map[string]int     `json:"admissibility"`
	ObjectiveScores        map[string]float64 `json:"objective_scores"`
	Commitments            map[string]float64 `json:"commitments"`
	WritingMetrics         map[string]float64 `json:"writing_metrics"`
	ExpectedBlockersBefore []string           `json:"expected_blockers_before"`
	ExpectedBlockersAfter  []string           `json:"expected_blockers_after"`
	ExportReadyAfter       bool               `json:"export_ready_after"`
	Originals              map[string]string  `json:"originals"`
}

func loadGolden(path string) (*goldenSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var g goldenSpec
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, fmt.Errorf("golden %s: %w", path, err)
	}
	if g.FixtureVersion <= 0 || g.TenderNo == "" {
		return nil, fmt.Errorf("golden %s: fixture_version and tender_no are required", path)
	}
	if g.Status != "frozen" {
		return nil, fmt.Errorf("golden %s is not frozen (_status=%q)", path, g.Status)
	}
	return &g, nil
}

type defectSpec struct {
	Defect      string `json:"defect"`
	Gate        string `json:"gate"`
	Expected    string `json:"expected"`
	Description string `json:"description"`
}

type defectsFile struct {
	FixtureVersion int          `json:"fixture_version"`
	Defects        []defectSpec `json:"defects"`
}

func loadDefects(path string) (*defectsFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d defectsFile
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("defects %s: %w", path, err)
	}
	return &d, nil
}

type round struct {
	index          int
	stages         []*Stage
	gates          []*Gate
	tasks          []int64
	tenants        []string
	artifacts      []Artifact
	mainReady      bool
	exportReadback string
	mainExported   bool
	headings       []string
	warningKinds   []string
	cleanup        CleanupInfo
}

func newRound(index int) *round {
	return &round{index: index, stages: []*Stage{}, gates: []*Gate{}, tasks: []int64{}, tenants: []string{}, artifacts: []Artifact{}}
}

var stageTitles = map[string]string{
	"S1": "创建隔离任务", "S2": "导入招标文件与更正", "S3": "导入企业证据与原件", "S4": "提取并复核要求",
	"S5": "确认清单并批准大纲", "S6": "要求级与逐证据可采性", "S7": "承诺台账", "S8": "客观分",
	"S9": "报价勾稽", "S10": "D3 故事板粉队评审、受控渲染与句子级核验", "S11": "响应矩阵", "S12": "一致性核验",
	"S13": "导出就绪", "S14": "创建导出任务", "S15": "导出执行与回读", "S16": "清理与回归", "setup": "环境与产物", "GATES": "缺陷库覆盖",
}

func (r *round) stage(id, title string) *Stage {
	if title == id {
		if t := stageTitles[id]; t != "" {
			title = t
		}
	}
	for _, s := range r.stages {
		if s.ID == id {
			return s
		}
	}
	s := &Stage{ID: id, Title: title, Status: statusPass, Checks: []Check{}}
	r.stages = append(r.stages, s)
	return s
}

func (r *round) addTask(id int64) {
	for _, x := range r.tasks {
		if x == id {
			return
		}
	}
	r.tasks = append(r.tasks, id)
}

func (r *round) addTenant(t string) {
	for _, x := range r.tenants {
		if x == t {
			return
		}
	}
	r.tenants = append(r.tenants, t)
}

func (r *round) check(stageID, id, expected, actual string, ok bool, detail string) bool {
	s := r.stage(stageID, stageID)
	c := Check{ID: id, Expected: expected, Actual: actual, Detail: detail}
	if ok {
		c.Status = statusPass
	} else {
		c.Status = statusFail
		s.Status = statusFail
	}
	s.Checks = append(s.Checks, c)
	return ok
}

func (r *round) fail(stageID, id, expected string, err error, detail string) {
	actual := "error"
	if err != nil {
		actual = err.Error()
	}
	r.check(stageID, id, expected, actual, false, detail)
}

func (r *round) gate(g *Gate) {
	r.gates = append(r.gates, g)
}

func (r *round) summary() (passed, failed int) {
	for _, s := range r.stages {
		for _, c := range s.Checks {
			if c.Status == statusPass {
				passed++
			} else {
				failed++
			}
		}
	}
	for _, g := range r.gates {
		for _, c := range g.Checks {
			if c.Status == statusPass {
				passed++
			} else {
				failed++
			}
		}
		if !g.Remediated || g.Error != "" {
			failed++
		}
	}
	return passed, failed
}

func (r *round) normalized() map[string]any {
	stages := []map[string]any{}
	for _, s := range r.stages {
		checks := []map[string]any{}
		for _, c := range s.Checks {
			checks = append(checks, map[string]any{"id": c.ID, "status": c.Status, "expected": c.Expected, "actual": c.Actual})
		}
		stages = append(stages, map[string]any{"id": s.ID, "status": s.Status, "checks": checks})
	}
	gates := []map[string]any{}
	for _, g := range r.gates {
		checks := []map[string]any{}
		for _, c := range g.Checks {
			checks = append(checks, map[string]any{"id": c.ID, "status": c.Status, "expected": c.Expected, "actual": c.Actual})
		}
		gates = append(gates, map[string]any{"id": g.ID, "gate": g.Gate, "signal_type": g.SignalType, "sentinel": g.Sentinel,
			"observed": g.Observed, "remediated": g.Remediated, "full_ready": g.FullReady, "checks": checks})
	}
	return map[string]any{"stages": stages, "gates": gates, "main_ready": r.mainReady,
		"export_readback": r.exportReadback, "exported": r.mainExported,
		"headings": r.headings, "warnings": r.warningKinds}
}

func compareRounds(a, b map[string]any) []string {
	var diffs []string
	compareValue("", a, b, &diffs)
	sort.Strings(diffs)
	return diffs
}

func compareValue(path string, a, b any, diffs *[]string) {
	if reflect.DeepEqual(a, b) {
		return
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		sk := make([]string, 0, len(keys))
		for k := range keys {
			sk = append(sk, k)
		}
		sort.Strings(sk)
		for _, k := range sk {
			compareValue(path+"/"+k, am[k], bm[k], diffs)
		}
		return
	}
	as, aok2 := a.([]any)
	bs, bok2 := b.([]any)
	if aok2 && bok2 {
		if len(as) != len(bs) {
			*diffs = append(*diffs, fmt.Sprintf("%s: length %d != %d", path, len(as), len(bs)))
			return
		}
		for i := range as {
			compareValue(fmt.Sprintf("%s[%d]", path, i), as[i], bs[i], diffs)
		}
		return
	}
	*diffs = append(*diffs, fmt.Sprintf("%s: %v != %v", path, a, b))
}

func kindList(bs []db.ExportBlocker) string {
	if len(bs) == 0 {
		return ""
	}
	seen := map[string]bool{}
	var out []string
	for _, b := range bs {
		if !seen[b.Kind] {
			seen[b.Kind] = true
			out = append(out, b.Kind)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func hasBlocker(rd *db.ExportReadiness, kind string) bool {
	if rd == nil {
		return false
	}
	for _, b := range rd.Blockers {
		if b.Kind == kind {
			return true
		}
	}
	return false
}

func boolText(b bool) string { return fmt.Sprint(b) }

func failedRowText(rows []*db.ComplianceRow) string {
	out := []string{}
	for _, row := range rows {
		if row.VerifyStatus != "verified" {
			out = append(out, fmt.Sprintf("%s@%s:%s %s", row.RequirementTitle, row.SectionCode, row.VerifyStatus, row.FailureReason))
		}
	}
	return strings.Join(out, "; ")
}

func blockerText(rd *db.ExportReadiness) string {
	if rd == nil {
		return ""
	}
	out := []string{}
	for _, b := range rd.Blockers {
		out = append(out, fmt.Sprintf("%s:%s", b.Kind, b.Message))
	}
	return strings.Join(out, "; ")
}

func fileSHA(path string) (string, int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), int64(len(raw)), nil
}

func artifactOf(kind, path, note string) (Artifact, error) {
	sum, size, err := fileSHA(path)
	if err != nil {
		return Artifact{}, fmt.Errorf("artifact %s: %w", kind, err)
	}
	return Artifact{Kind: kind, Path: filepath.ToSlash(path), Size: size, SHA256: sum, Note: note}, nil
}

func errCode(err error) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, db.ErrStoryboardMissing):
		return "ErrStoryboardMissing"
	case errors.Is(err, db.ErrStoryboardNotApproved):
		return "ErrStoryboardNotApproved"
	case errors.Is(err, db.ErrStoryboardInvalid):
		return "ErrStoryboardInvalid"
	case errors.Is(err, db.ErrStoryboardState):
		return "ErrStoryboardState"
	case errors.Is(err, db.ErrPinkSelfApproval):
		return "ErrPinkSelfApproval"
	case errors.Is(err, db.ErrRedSelfApproval):
		return "ErrRedSelfApproval"
	case errors.Is(err, db.ErrUngroundedClaim):
		return "ErrUngroundedClaim"
	case errors.Is(err, db.ErrWritingVerificationFailed):
		return "ErrWritingVerificationFailed"
	case errors.Is(err, db.ErrEvidenceInadmissible):
		return "ErrEvidenceInadmissible"
	case errors.Is(err, db.ErrCommitmentUnconfirmed):
		return "ErrCommitmentUnconfirmed"
	case errors.Is(err, db.ErrCommitmentMismatch):
		return "ErrCommitmentMismatch"
	default:
		return err.Error()
	}
}
