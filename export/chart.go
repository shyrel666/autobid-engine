package export

// 图表源码生成：结构化规格 -> Mermaid（架构图 / 甘特图 / 组织架构图）或结构化 JSON（设备选型表）。
// 生成是确定性的（相同规格得到相同源码与 source_hash），由代码校验规格，而不是把模型输出的源码原样入库。

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
)

var ErrChartSpec = errors.New("chart: invalid spec")

func specErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrChartSpec, fmt.Sprintf(format, args...))
}

// ArchitectureSpec 是分层架构图：每层一个子图，edges 按节点名连接。
type ArchitectureSpec struct {
	Direction string `json:"direction,omitempty"` // TB（默认）/ LR
	Layers    []struct {
		Name  string   `json:"name"`
		Nodes []string `json:"nodes"`
	} `json:"layers"`
	Edges []struct {
		From  string `json:"from"`
		To    string `json:"to"`
		Label string `json:"label,omitempty"`
	} `json:"edges,omitempty"`
}

// GanttSpec 是实施进度甘特图。任务的开始可用 start（YYYY-MM-DD）或 after（前置任务 id）。
type GanttSpec struct {
	Start string `json:"start"`
	Tasks []struct {
		ID        string `json:"id,omitempty"`
		Name      string `json:"name"`
		Phase     string `json:"phase,omitempty"`
		Start     string `json:"start,omitempty"`
		After     string `json:"after,omitempty"`
		Days      int    `json:"days"`
		Milestone bool   `json:"milestone,omitempty"`
	} `json:"tasks"`
}

// OrgSpec 是项目组织架构图。
type OrgSpec struct {
	Nodes []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Person string `json:"person,omitempty"`
		Parent string `json:"parent,omitempty"`
	} `json:"nodes"`
}

// TableSpec 是设备选型表，导出时渲染为原生 Word 表格。
type TableSpec struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
}

// ChartSource 是生成结果。
type ChartSource struct {
	Format string          // db.ChartSourceMermaid / db.ChartSourceJSON
	Source string          // Mermaid 源码或规范化的 JSON
	Hash   string          // SHA-256(类型 + 源码)
	Spec   json.RawMessage // 规范化后的规格
}

const maxChartItems = 200

// BuildChartSource 按图表类型校验规格并生成源码。
func BuildChartSource(chartType, title string, spec json.RawMessage) (*ChartSource, error) {
	if strings.TrimSpace(title) == "" {
		return nil, specErr("title is required")
	}
	if len(spec) == 0 {
		return nil, specErr("spec is required")
	}
	var (
		src    string
		format = db.ChartSourceMermaid
		norm   any
		err    error
	)
	switch chartType {
	case db.ChartArchitecture:
		var s ArchitectureSpec
		if err = strictJSON(spec, &s); err == nil {
			src, err = architectureMermaid(&s)
		}
		norm = s
	case db.ChartGantt:
		var s GanttSpec
		if err = strictJSON(spec, &s); err == nil {
			src, err = ganttMermaid(title, &s)
		}
		norm = s
	case db.ChartOrg:
		var s OrgSpec
		if err = strictJSON(spec, &s); err == nil {
			src, err = orgMermaid(&s)
		}
		norm = s
	case db.ChartEquipmentTable:
		var s TableSpec
		if err = strictJSON(spec, &s); err == nil {
			err = s.validate()
		}
		format, norm = db.ChartSourceJSON, s
		if err == nil {
			b, _ := json.Marshal(s)
			src = string(b)
		}
	default:
		return nil, specErr("unknown chart_type %q", chartType)
	}
	if err != nil {
		return nil, err
	}
	n, _ := json.Marshal(norm)
	return &ChartSource{Format: format, Source: src, Hash: sha256Hex([]byte(chartType + "\n" + src)), Spec: n}, nil
}

func strictJSON(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return specErr("%v", err)
	}
	return nil
}

// mermaidLabel 转义节点文字（Mermaid 的引号标签中 " 需写成 #quot;，换行写成 <br/>）。
func mermaidLabel(s string) string {
	s = strings.TrimSpace(s)
	s = strings.NewReplacer(`"`, "#quot;", "\r\n", "<br/>", "\n", "<br/>", "`", "'").Replace(s)
	return `"` + s + `"`
}

func architectureMermaid(s *ArchitectureSpec) (string, error) {
	dir := strings.ToUpper(strings.TrimSpace(s.Direction))
	if dir == "" {
		dir = "TB"
	}
	if dir != "TB" && dir != "LR" && dir != "TD" && dir != "RL" && dir != "BT" {
		return "", specErr("direction must be TB or LR")
	}
	if len(s.Layers) == 0 {
		return "", specErr("architecture needs at least one layer")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "flowchart %s\n", dir)
	ids := map[string]string{}
	n := 0
	for li, l := range s.Layers {
		if strings.TrimSpace(l.Name) == "" || len(l.Nodes) == 0 {
			return "", specErr("layer %d needs a name and nodes", li+1)
		}
		fmt.Fprintf(&b, "  subgraph L%d[%s]\n", li+1, mermaidLabel(l.Name))
		fmt.Fprintf(&b, "    direction LR\n")
		for _, node := range l.Nodes {
			node = strings.TrimSpace(node)
			if node == "" {
				return "", specErr("layer %q has an empty node", l.Name)
			}
			if _, dup := ids[node]; dup {
				return "", specErr("node %q appears twice", node)
			}
			n++
			if n > maxChartItems {
				return "", specErr("too many nodes")
			}
			ids[node] = fmt.Sprintf("n%d", n)
			fmt.Fprintf(&b, "    n%d[%s]\n", n, mermaidLabel(node))
		}
		b.WriteString("  end\n")
	}
	// 层与层之间按顺序连接（未给出 edges 时表达分层依赖关系）。
	if len(s.Edges) == 0 {
		for i := 1; i < len(s.Layers); i++ {
			fmt.Fprintf(&b, "  L%d --> L%d\n", i, i+1)
		}
	}
	for _, e := range s.Edges {
		from, ok1 := ids[strings.TrimSpace(e.From)]
		to, ok2 := ids[strings.TrimSpace(e.To)]
		if !ok1 || !ok2 {
			return "", specErr("edge %q -> %q references an unknown node", e.From, e.To)
		}
		if e.Label != "" {
			fmt.Fprintf(&b, "  %s -->|%s| %s\n", from, mermaidLabel(e.Label), to)
		} else {
			fmt.Fprintf(&b, "  %s --> %s\n", from, to)
		}
	}
	return b.String(), nil
}

var reGanttID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

func ganttMermaid(title string, s *GanttSpec) (string, error) {
	if len(s.Tasks) == 0 {
		return "", specErr("gantt needs at least one task")
	}
	if len(s.Tasks) > maxChartItems {
		return "", specErr("too many tasks")
	}
	parse := func(v string) error {
		_, err := time.Parse("2006-01-02", v)
		return err
	}
	if s.Start != "" && parse(s.Start) != nil {
		return "", specErr("start must be YYYY-MM-DD")
	}
	var b strings.Builder
	// todayMarker off：不画渲染当天的红线，图表只由规格决定（同一规格任何一天渲染结果相同）。
	fmt.Fprintf(&b, "gantt\n  title %s\n  dateFormat YYYY-MM-DD\n  axisFormat %%m-%%d\n  todayMarker off\n", strings.ReplaceAll(title, "\n", " "))
	ids := map[string]bool{}
	phase := "\x00"
	for i, t := range s.Tasks {
		if strings.TrimSpace(t.Name) == "" {
			return "", specErr("task %d has no name", i+1)
		}
		if t.ID != "" {
			if !reGanttID.MatchString(t.ID) || ids[t.ID] {
				return "", specErr("task id %q is invalid or duplicated", t.ID)
			}
			ids[t.ID] = true
		}
		if t.Days <= 0 && !t.Milestone {
			return "", specErr("task %q needs days > 0", t.Name)
		}
		var start string
		switch {
		case t.After != "":
			if !ids[t.After] {
				return "", specErr("task %q is after unknown task %q", t.Name, t.After)
			}
			start = "after " + t.After
		case t.Start != "":
			if parse(t.Start) != nil {
				return "", specErr("task %q start must be YYYY-MM-DD", t.Name)
			}
			start = t.Start
		case s.Start != "" && i == 0:
			start = s.Start
		case i > 0:
			start = "" // Mermaid：省略开始时间表示紧接上一个任务
		default:
			return "", specErr("the first task needs start (or spec.start)")
		}
		p := strings.TrimSpace(t.Phase)
		if p == "" {
			p = "实施计划"
		}
		if p != phase {
			fmt.Fprintf(&b, "  section %s\n", strings.ReplaceAll(p, ":", "："))
			phase = p
		}
		name := strings.NewReplacer(":", "：", "#", "＃", "\n", " ").Replace(strings.TrimSpace(t.Name))
		parts := []string{}
		if t.Milestone {
			parts = append(parts, "milestone")
		}
		if t.ID != "" {
			parts = append(parts, t.ID)
		}
		if start != "" {
			parts = append(parts, start)
		}
		days := t.Days
		if t.Milestone && days <= 0 {
			days = 0
		}
		parts = append(parts, fmt.Sprintf("%dd", days))
		fmt.Fprintf(&b, "  %s : %s\n", name, strings.Join(parts, ", "))
	}
	return b.String(), nil
}

func orgMermaid(s *OrgSpec) (string, error) {
	if len(s.Nodes) == 0 {
		return "", specErr("org chart needs at least one node")
	}
	if len(s.Nodes) > maxChartItems {
		return "", specErr("too many nodes")
	}
	ids := map[string]string{}
	for i, n := range s.Nodes {
		if !reGanttID.MatchString(n.ID) || strings.TrimSpace(n.Name) == "" {
			return "", specErr("node %d needs an id (letters/digits) and a name", i+1)
		}
		if _, dup := ids[n.ID]; dup {
			return "", specErr("node id %q duplicated", n.ID)
		}
		ids[n.ID] = fmt.Sprintf("o%d", i+1)
	}
	roots := 0
	var b strings.Builder
	b.WriteString("flowchart TD\n")
	for _, n := range s.Nodes {
		label := n.Name
		if n.Person != "" {
			label += "\n" + n.Person
		}
		fmt.Fprintf(&b, "  %s[%s]\n", ids[n.ID], mermaidLabel(label))
	}
	parents := map[string]string{}
	for _, n := range s.Nodes {
		if n.Parent == "" {
			roots++
			continue
		}
		if _, ok := ids[n.Parent]; !ok {
			return "", specErr("node %q has unknown parent %q", n.ID, n.Parent)
		}
		parents[n.ID] = n.Parent
		fmt.Fprintf(&b, "  %s --> %s\n", ids[n.Parent], ids[n.ID])
	}
	if roots == 0 {
		return "", specErr("org chart needs a root node (empty parent)")
	}
	for id := range parents { // 环检测
		seen := map[string]bool{}
		for cur := id; cur != ""; cur = parents[cur] {
			if seen[cur] {
				return "", specErr("org chart has a cycle at %q", cur)
			}
			seen[cur] = true
		}
	}
	return b.String(), nil
}

func (t *TableSpec) validate() error {
	if len(t.Columns) == 0 || len(t.Columns) > 30 {
		return specErr("equipment table needs 1..30 columns")
	}
	if len(t.Rows) == 0 || len(t.Rows) > 2000 {
		return specErr("equipment table needs 1..2000 rows")
	}
	for i, c := range t.Columns {
		if strings.TrimSpace(c) == "" {
			return specErr("column %d has no name", i+1)
		}
	}
	for i, r := range t.Rows {
		if len(r) != len(t.Columns) {
			return specErr("row %d has %d cells, expected %d", i+1, len(r), len(t.Columns))
		}
	}
	return nil
}

// ParseTableSource 解析设备选型表 JSON 源码。
func ParseTableSource(src string) (*TableSpec, error) {
	var t TableSpec
	if err := json.Unmarshal([]byte(src), &t); err != nil {
		return nil, err
	}
	return &t, t.validate()
}

// ChartTypes 返回支持的图表类型（排序后，供工具 Schema 使用）。
func ChartTypes() []string {
	out := []string{db.ChartArchitecture, db.ChartGantt, db.ChartOrg, db.ChartEquipmentTable}
	sort.Strings(out)
	return out
}
