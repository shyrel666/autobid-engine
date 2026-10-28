package export

// 图表源码生成单元测试：结构化规格 -> Mermaid / JSON 的确定性输出与规格校验（不接受直接给出的 Mermaid）。

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/autobid/autobid-engine/db"
)

func TestBuildChartSource(t *testing.T) {
	cases := []struct {
		typ, spec string
		want      []string
	}{
		{db.ChartArchitecture, `{"layers":[{"name":"感知层","nodes":["摄像机","门禁"]},{"name":"平台层","nodes":["视频\"平台\""]}],"edges":[{"from":"摄像机","to":"视频\"平台\"","label":"RTSP"}]}`,
			[]string{"flowchart TB", `subgraph L1["感知层"]`, `n3["视频#quot;平台#quot;"]`, `n1 -->|"RTSP"| n3`}},
		{db.ChartGantt, `{"start":"2026-11-01","tasks":[{"id":"t1","name":"调研: 需求","phase":"准备","days":10},{"name":"实施","after":"t1","days":30},{"name":"验收","milestone":true,"days":0}]}`,
			[]string{"gantt", "dateFormat YYYY-MM-DD", "section 准备", "调研： 需求 : t1, 2026-11-01, 10d", "实施 : after t1, 30d", "验收 : milestone, 0d"}},
		{db.ChartOrg, `{"nodes":[{"id":"pm","name":"项目经理","person":"张三"},{"id":"dev","name":"开发组","parent":"pm"}]}`,
			[]string{"flowchart TD", `o1["项目经理<br/>张三"]`, "o1 --> o2"}},
	}
	for _, c := range cases {
		src, err := BuildChartSource(c.typ, "图", json.RawMessage(c.spec))
		if err != nil {
			t.Fatalf("%s: %v", c.typ, err)
		}
		for _, w := range c.want {
			if !strings.Contains(src.Source, w) {
				t.Fatalf("%s source missing %q:\n%s", c.typ, w, src.Source)
			}
		}
		again, _ := BuildChartSource(c.typ, "图", json.RawMessage(c.spec))
		if again.Hash != src.Hash || src.Format != db.ChartSourceMermaid {
			t.Fatalf("%s: generation is not deterministic", c.typ)
		}
	}
	tbl, err := BuildChartSource(db.ChartEquipmentTable, "设备", json.RawMessage(`{"columns":["a","b"],"rows":[["1","2"]]}`))
	if err != nil || tbl.Format != db.ChartSourceJSON {
		t.Fatalf("table: %+v %v", tbl, err)
	}
	for _, bad := range []struct{ typ, spec string }{
		{db.ChartArchitecture, `{"layers":[]}`},
		{db.ChartArchitecture, `{"layers":[{"name":"a","nodes":["x"]}],"edges":[{"from":"x","to":"y"}]}`},
		{db.ChartArchitecture, `{"layers":[{"name":"a","nodes":["x"]}],"mermaid":"graph TD; A-->B"}`}, // 不接受直接给出的源码
		{db.ChartGantt, `{"tasks":[{"name":"a","days":1}]}`},
		{db.ChartGantt, `{"start":"2026-11-01","tasks":[{"name":"a","after":"nope","days":1}]}`},
		{db.ChartOrg, `{"nodes":[{"id":"a","name":"A","parent":"b"},{"id":"b","name":"B","parent":"a"}]}`},
		{db.ChartEquipmentTable, `{"columns":["a","b"],"rows":[["1"]]}`},
		{"pie", `{}`},
	} {
		if _, err := BuildChartSource(bad.typ, "图", json.RawMessage(bad.spec)); !errors.Is(err, ErrChartSpec) {
			t.Fatalf("%s %s accepted: %v", bad.typ, bad.spec, err)
		}
	}
}
