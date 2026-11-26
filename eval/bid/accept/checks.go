package main

// 检查项注册、状态（pass/fail）、门禁结果与两轮归一化比较所需的模型。

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/autobid/autobid-engine/db"
)

const (
	statusPass = "pass"
	statusFail = "fail"
	actor      = "e2e-acceptance"
)

// Check 是一条可核对的检查项。expected/actual 不包含数据库自增 ID 或时间戳，便于两轮归一化比较。
type Check struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Stage 是主链路或其他场景的一个阶段。
type Stage struct {
	ID     string  `json:"id"`
	Title  string  `json:"title"`
	Status string  `json:"status"`
	Checks []Check `json:"checks"`
}

// Gate 是一个门禁/解析校验的正反例结果。
type Gate struct {
	ID         string  `json:"id"`
	Gate       string  `json:"gate"`
	Blocked    bool    `json:"blocked"`
	Observed   string  `json:"observed"`
	Remediated bool    `json:"remediated"`
	FullReady  bool    `json:"full_ready,omitempty"`
	Error      string  `json:"error,omitempty"`
	Checks     []Check `json:"checks,omitempty"`
}

// Artifact 是报告列出的产物。
type Artifact struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Note   string `json:"note,omitempty"`
}

// Deferred 是明确延后、报告中必须写 BLOCKED 的能力。
type Deferred struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// Determinism 是同一执行器两轮完整运行的归一化比较结果。
type Determinism struct {
	Enabled      bool     `json:"enabled"`
	Consistent   bool     `json:"consistent"`
	Differences  []string `json:"differences"`
	SecondFailed int      `json:"second_failed"`
}

// CleanupInfo 记录任务清理结果。
type CleanupInfo struct {
	Mode   string `json:"mode"`
	Tasks  int    `json:"tasks"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// goldenSpec 是 eval/bid/fixtures/golden.json 中冻结的期望值。
type goldenSpec struct {
	FixtureVersion       int                `json:"fixture_version"`
	TenderNo             string             `json:"tender_no"`
	BidDeadline          string             `json:"bid_deadline"`
	VetoTotal            int                `json:"veto_total"`
	VetoByKind           map[string]int     `json:"veto_by_kind"`
	ScoringTotal         int                `json:"scoring_total"`
	ScoringWeights       map[string]float64 `json:"scoring_weights"`
	MandatoryClauseTotal int                `json:"mandatory_clause_total"`
	NonScoringTotal      int                `json:"non_scoring_required_total"`
	PriceItemsOutOfScope int                `json:"price_items_out_of_scope"`
	Superseded           int                `json:"superseded"`
	MappedRequirements   int                `json:"mapped_requirements"`
	OutlineSections      int                `json:"outline_sections"`
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
	return &g, nil
}

// round 保存一轮完整执行的阶段、门禁、任务与产物。
type round struct {
	index          int
	stages         []*Stage
	gates          []*Gate
	tasks          []int64
	artifacts      []Artifact
	mainReady      bool
	mainExported   bool
	exportReadback string
	warningKinds   []string
	headings       []string
}

func newRound(index int) *round { return &round{index: index} }

// stage 返回指定 ID 的阶段，不存在时新建。
func (r *round) stage(id, title string) *Stage {
	for _, s := range r.stages {
		if s.ID == id {
			return s
		}
	}
	s := &Stage{ID: id, Title: title, Status: statusPass, Checks: []Check{}}
	r.stages = append(r.stages, s)
	return s
}

// check 在阶段中记录一项检查。
func (r *round) check(stageID, id, expected, actual string, pass bool, detail string) {
	s := r.stage(stageID, stageID)
	st := statusPass
	if !pass {
		st = statusFail
		s.Status = statusFail
	}
	s.Checks = append(s.Checks, Check{ID: id, Status: st, Expected: expected, Actual: actual, Detail: detail})
}

// fail 记录一项失败检查（用于错误路径）。
func (r *round) fail(stageID, id, expected string, err error, detail string) {
	actual := "error"
	if err != nil {
		actual = err.Error()
	}
	r.check(stageID, id, expected, actual, false, detail)
}

func (r *round) addTask(id int64) {
	if id > 0 {
		r.tasks = append(r.tasks, id)
	}
}

// gate 新建一个门禁结果并挂到本轮。
func (r *round) gate(id, name string) *Gate {
	g := &Gate{ID: id, Gate: name, Checks: []Check{}}
	r.gates = append(r.gates, g)
	return g
}

func (g *Gate) check(id, expected, actual string, pass bool, detail string) {
	st := statusPass
	if !pass {
		st = statusFail
	}
	g.Checks = append(g.Checks, Check{ID: id, Status: st, Expected: expected, Actual: actual, Detail: detail})
}

func (g *Gate) fail(id, expected string, err error) {
	actual := "error"
	if err != nil {
		actual = err.Error()
		g.Error = actual
	}
	g.check(id, expected, actual, false, "")
}

// summary 统计本轮全部检查的通过 / 失败数。
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
	}
	return passed, failed
}

// --- 归一化快照（忽略时间戳、数据库 ID、任务 ID、路径与 SHA-256） ---

type normCheck struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
}

type normStage struct {
	ID     string      `json:"id"`
	Status string      `json:"status"`
	Checks []normCheck `json:"checks"`
}

type normGate struct {
	ID         string `json:"id"`
	Blocked    bool   `json:"blocked"`
	Observed   string `json:"observed"`
	Remediated bool   `json:"remediated"`
	FullReady  bool   `json:"full_ready,omitempty"`
}

type normRound struct {
	Stages         []normStage `json:"stages"`
	Gates          []normGate  `json:"gates"`
	MainReady      bool        `json:"main_ready"`
	MainExported   bool        `json:"main_exported"`
	ExportReadback string      `json:"export_readback"`
	WarningKinds   []string    `json:"warning_kinds"`
	Headings       []string    `json:"headings"`
}

func (r *round) normalized() normRound {
	out := normRound{MainReady: r.mainReady, MainExported: r.mainExported, ExportReadback: r.exportReadback,
		WarningKinds: append([]string{}, r.warningKinds...), Headings: append([]string{}, r.headings...)}
	for _, s := range r.stages {
		ns := normStage{ID: s.ID, Status: s.Status, Checks: []normCheck{}}
		for _, c := range s.Checks {
			ns.Checks = append(ns.Checks, normCheck{ID: c.ID, Status: c.Status, Expected: c.Expected, Actual: c.Actual})
		}
		out.Stages = append(out.Stages, ns)
	}
	for _, g := range r.gates {
		out.Gates = append(out.Gates, normGate{ID: g.ID, Blocked: g.Blocked, Observed: g.Observed, Remediated: g.Remediated, FullReady: g.FullReady})
	}
	return out
}

// compareRounds 比较两轮归一化结果，返回差异列表。
func compareRounds(a, b normRound) []string {
	ja, _ := json.MarshalIndent(a, "", "  ")
	jb, _ := json.MarshalIndent(b, "", "  ")
	if string(ja) == string(jb) {
		return nil
	}
	var diffs []string
	sa, sb := map[string]normStage{}, map[string]normStage{}
	for _, s := range a.Stages {
		sa[s.ID] = s
	}
	for _, s := range b.Stages {
		sb[s.ID] = s
	}
	for id, s := range sa {
		o, ok := sb[id]
		if !ok {
			diffs = append(diffs, "stage "+id+" only in round 1")
			continue
		}
		if s.Status != o.Status {
			diffs = append(diffs, fmt.Sprintf("stage %s status %s != %s", id, s.Status, o.Status))
		}
		ca, cb := map[string]normCheck{}, map[string]normCheck{}
		for _, c := range s.Checks {
			ca[c.ID] = c
		}
		for _, c := range o.Checks {
			cb[c.ID] = c
		}
		for cid, c := range ca {
			d, ok := cb[cid]
			if !ok {
				diffs = append(diffs, "check "+cid+" only in round 1")
				continue
			}
			if c != d {
				diffs = append(diffs, fmt.Sprintf("check %s: %+v != %+v", cid, c, d))
			}
		}
		for cid := range cb {
			if _, ok := ca[cid]; !ok {
				diffs = append(diffs, "check "+cid+" only in round 2")
			}
		}
	}
	for id := range sb {
		if _, ok := sa[id]; !ok {
			diffs = append(diffs, "stage "+id+" only in round 2")
		}
	}
	ga, gb := map[string]normGate{}, map[string]normGate{}
	for _, g := range a.Gates {
		ga[g.ID] = g
	}
	for _, g := range b.Gates {
		gb[g.ID] = g
	}
	for id, g := range ga {
		o, ok := gb[id]
		if !ok {
			diffs = append(diffs, "gate "+id+" only in round 1")
			continue
		}
		if g != o {
			diffs = append(diffs, fmt.Sprintf("gate %s: %+v != %+v", id, g, o))
		}
	}
	for id := range gb {
		if _, ok := ga[id]; !ok {
			diffs = append(diffs, "gate "+id+" only in round 2")
		}
	}
	if a.MainReady != b.MainReady || a.MainExported != b.MainExported || a.ExportReadback != b.ExportReadback {
		diffs = append(diffs, fmt.Sprintf("export summary %v/%v/%s != %v/%v/%s", a.MainReady, a.MainExported, a.ExportReadback,
			b.MainReady, b.MainExported, b.ExportReadback))
	}
	if strings.Join(a.WarningKinds, ",") != strings.Join(b.WarningKinds, ",") {
		diffs = append(diffs, "warning kinds differ: "+strings.Join(a.WarningKinds, ",")+" != "+strings.Join(b.WarningKinds, ","))
	}
	if strings.Join(a.Headings, "|") != strings.Join(b.Headings, "|") {
		diffs = append(diffs, "headings differ")
	}
	sort.Strings(diffs)
	return diffs
}

// kindList 把阻断原因去重为稳定文本，用于断言与报告。
func kindList(blockers []db.ExportBlocker) string {
	seen := map[string]bool{}
	var out []string
	for _, b := range blockers {
		if !seen[b.Kind] {
			seen[b.Kind] = true
			out = append(out, b.Kind)
		}
	}
	return strings.Join(out, ",")
}

func kindsContain(blockers []db.ExportBlocker, kind string) bool {
	for _, b := range blockers {
		if b.Kind == kind {
			return true
		}
	}
	return false
}

func intsText(vals []int64) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, ",")
}

// deferredItems 固定列出 M1 不覆盖、报告中必须写 BLOCKED 的能力。
func deferredItems() []Deferred {
	return []Deferred{
		{ID: "D2.evidence", Status: "blocked", Reason: "证据检索与绑定、过期证书、子公司业绩、无证据业绩：依赖 D2 证据库，M1 只有虚构企业文件且不消费"},
		{ID: "evidence_binding", Status: "blocked", Reason: "虚拟企业 JSON 仅在 M1 生成、未导入知识库、未绑定证据，不能宣称证据链路已覆盖"},
		{ID: "D2.commitment", Status: "blocked", Reason: "承诺低于评分阈值、报价不一致、报价与商务文件冲突、型号错误与参数低于阈值、承诺台账：依赖 D2/D3，M1 未实现"},
		{ID: "D3.writing", Status: "blocked", Reason: "真实大模型写作质量与句子级事实核验：M1 使用确定性替身写作，只验流程"},
		{ID: "6B.pdf", Status: "blocked", Reason: "PDF 导出与 文档引擎 目录页码刷新：M1 只导出 DOCX，目录未刷新仅记警告"},
		{ID: "V.accuracy", Status: "blocked", Reason: "真实项目/企业的准确率与召回率：无真实材料与人工标注，不能做 V 级"},
		{ID: "4.4.multi_agent", Status: "blocked", Reason: "多 Agent 检索与写作：无稳定收益证据前保持关闭"},
		{ID: "API.UI", Status: "blocked", Reason: "HTTP REST 路由、SSE 与浏览器端到端操作：本执行器直接调用 Go 业务接口，API/UI 需另行验收"},
	}
}
