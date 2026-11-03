package db

import (
	"context"
	"encoding/json"
	"slices"
)

// NodeBrief 是节点在态势快照中的紧凑表示。
type NodeBrief struct {
	ID            int64  `json:"id"`
	State         string `json:"state,omitempty"`
	Priority      int    `json:"priority,omitempty"`
	Owner         string `json:"owner,omitempty"`
	BlockedReason string `json:"blocked_reason,omitempty"`
	Summary       string `json:"summary"`
}

// NodeSummary 从 payload 取 summary（回落 title），截断到 max 个字符。
func NodeSummary(n *Node, max int) string {
	var p struct {
		Summary string `json:"summary"`
		Title   string `json:"title"`
	}
	_ = json.Unmarshal(n.Payload, &p)
	s := p.Summary
	if s == "" {
		s = p.Title
	}
	return truncateRunes(s, max)
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if max <= 0 || len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// Briefs 把节点转换为紧凑表示；withState 时附带状态与阻塞原因。
func Briefs(nodes []*Node, withState bool) []NodeBrief {
	out := make([]NodeBrief, 0, len(nodes))
	for _, n := range nodes {
		b := NodeBrief{ID: n.ID, Summary: NodeSummary(n, 200)}
		if withState {
			b.State, b.BlockedReason = n.State, n.BlockedReason
		}
		if n.Kind == KindIntent {
			b.Priority = n.Priority
			if n.State == StateIntentRunning {
				b.Owner = n.Owner
			}
		}
		out = append(out, b)
	}
	return out
}

// DigestBrief 是活跃 Digest 在态势中的表示；Stale 的摘要不折叠其成员。
type DigestBrief struct {
	ID             int64   `json:"id"`
	Title          string  `json:"title"`
	Summary        string  `json:"summary"`
	MemberIDs      []int64 `json:"member_ids"`
	Round          int64   `json:"round"`
	Stale          bool    `json:"stale,omitempty"`
	ChangedMembers []int64 `json:"changed_members,omitempty"`
}

// ColdEntry 是冷节点索引项：只给出可用 node_detail 展开的定位信息。
type ColdEntry struct {
	ID      int64  `json:"id"`
	Kind    string `json:"kind"`
	State   string `json:"state"`
	Summary string `json:"summary"`
}

// Overview 是有界态势快照（§1.6），不是完整图导出。已被有效 Digest 覆盖的成员与冷节点
// 从“最近”列表中移除：前者由摘要代表，后者进入冷节点索引。
type Overview struct {
	Goals          []NodeBrief   `json:"goals"`
	Hints          []NodeBrief   `json:"hints,omitempty"`
	Frontier       []NodeBrief   `json:"open_intents"`
	Running        []NodeBrief   `json:"running_intents"`
	Paused         []NodeBrief   `json:"paused_intents,omitempty"`
	Waiting        []NodeBrief   `json:"waiting_intents,omitempty"` // blocked + waiting_job：等待后台作业，仍在进行
	RecentTerminal []NodeBrief   `json:"recent_terminal_intents"`
	RecentFacts    []NodeBrief   `json:"recent_facts"`
	RecentFindings []NodeBrief   `json:"recent_findings"`
	Digests        []DigestBrief `json:"digests,omitempty"`
	ColdIndex      []ColdEntry   `json:"cold_index,omitempty"`
	FoldedNodes    int           `json:"folded_nodes,omitempty"`
	Coverage       *Coverage     `json:"coverage,omitempty"`
}

// 典型界限（§1.6）：open=30、recentDone=12、最近 Fact=20、最近 Finding=10、冷索引=15。
const (
	overviewFrontier  = 30
	overviewRunning   = 50
	overviewPaused    = 30
	overviewWaiting   = 30
	overviewTerminal  = 12
	overviewFacts     = 20
	overviewFindings  = 10
	overviewColdIndex = 15
	overviewScan      = 200 // 过滤折叠/冷节点前的扫描上限
)

// Overview 读取有界态势；覆盖率由调用方按需填充（需要任务维度）。
func (s *ExplorationStore) Overview(ctx context.Context) (*Overview, error) {
	var o Overview

	digests, err := s.ActiveDigestsWithMembers(ctx)
	if err != nil {
		return nil, err
	}
	folded := map[int64]bool{}
	for _, d := range digests {
		o.Digests = append(o.Digests, DigestBrief{ID: d.Node.ID, Title: d.Title, Summary: truncateRunes(d.Body, 300),
			MemberIDs: d.MemberIDs, Round: d.Round, Stale: d.Stale, ChangedMembers: d.ChangedMembers})
		if d.Stale {
			continue
		}
		for _, id := range d.MemberIDs {
			folded[id] = true
		}
	}
	o.FoldedNodes = len(folded)

	cold, err := s.ColdNodes(ctx, overviewScan)
	if err != nil {
		return nil, err
	}
	hidden := map[int64]bool{}
	for id := range folded {
		hidden[id] = true
	}
	for _, n := range cold {
		hidden[n.ID] = true
		if !folded[n.ID] && len(o.ColdIndex) < overviewColdIndex {
			o.ColdIndex = append(o.ColdIndex, ColdEntry{ID: n.ID, Kind: n.Kind, State: n.State, Summary: NodeSummary(n, 60)})
		}
	}

	visible := func(nodes []*Node, limit int) []*Node {
		out := nodes[:0:0]
		for _, n := range nodes {
			if !hidden[n.ID] && len(out) < limit {
				out = append(out, n)
			}
		}
		return out
	}
	steps := []struct {
		dst       *[]NodeBrief
		withState bool
		load      func() ([]*Node, error)
		limit     int
	}{
		{&o.Goals, true, func() ([]*Node, error) { return s.NodesByKind(ctx, KindGoal, nil, 100) }, 100},
		{&o.Hints, false, func() ([]*Node, error) { return s.RecentNodes(ctx, KindHint, []string{StateHintActive}, 20) }, 20},
		{&o.Frontier, false, func() ([]*Node, error) { return s.Frontier(ctx, overviewFrontier) }, overviewFrontier},
		{&o.Running, false, func() ([]*Node, error) {
			return s.NodesByKind(ctx, KindIntent, []string{StateIntentRunning}, overviewRunning)
		}, overviewRunning},
		{&o.Paused, false, func() ([]*Node, error) {
			return s.NodesByKind(ctx, KindIntent, []string{StateIntentPaused}, overviewPaused)
		}, overviewPaused},
		{&o.Waiting, true, func() ([]*Node, error) { return s.WaitingIntents(ctx, overviewWaiting) }, overviewWaiting},
		{&o.RecentTerminal, true, func() ([]*Node, error) {
			nodes, err := s.RecentNodes(ctx, KindIntent, []string{StateIntentDone, StateIntentBlocked, StateIntentExhausted, StateIntentStopped}, overviewScan)
			return slices.DeleteFunc(nodes, IsWaitingIntent), err
		}, overviewTerminal},
		{&o.RecentFacts, false, func() ([]*Node, error) {
			return s.RecentNodes(ctx, KindFact, []string{StateConfirmed}, overviewScan)
		}, overviewFacts},
		{&o.RecentFindings, false, func() ([]*Node, error) {
			return s.RecentNodes(ctx, KindFinding, []string{StateConfirmed}, overviewScan)
		}, overviewFindings},
	}
	for _, st := range steps {
		nodes, err := st.load()
		if err != nil {
			return nil, err
		}
		*st.dst = Briefs(visible(nodes, st.limit), st.withState)
	}
	return &o, nil
}
