//go:build windows

package export

// 文档引擎的大文档与连续运行验证（S1，计划 12.5 的“更大文档、长时间稳定性”）。耗时数分钟，默认跳过：
// AUTOBID_OFFICE_SOAK=<次数> 时运行（建议 30）。只证明本机该次连续运行的结果，不代表更长时间或生产负载下稳定。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
)

// largeInput 构造 10 章 × 5 节的长文档：每节三段长正文，隔节一张 18 行宽表，附 120 行偏离表。
func largeInput() BuildInput {
	base := sampleInput()
	long := strings.Repeat("本系统采用分层解耦架构，感知层、网络层、平台层与应用层之间通过标准接口交互，保证系统的可扩展性与可维护性。", 12)
	var wide strings.Builder
	wide.WriteString("| 序号 | 设备名称 | 品牌 | 型号 | 分辨率 | 帧率 | 编码 | 存储 | 接口 | 功耗 | 防护 | 质保 | 数量 |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for i := 1; i <= 18; i++ {
		fmt.Fprintf(&wide, "| %d | 高清网络摄像机%d | 海康威视 | DS-2CD3T47WDV3-L%d | 400万像素 2560×1440 | 25fps | H.265+ | 256GB | RJ45×1 | 6.5W | IP67 | 3年 | %d |\n", i, i, i, i*2)
	}
	in := BuildInput{Doc: base.Doc, Bodies: map[int64]string{}, Bidder: base.Bidder, Date: base.Date}
	in.Doc.Sections = nil
	var id int64
	for ch := 1; ch <= 10; ch++ {
		id++
		in.Doc.Sections = append(in.Doc.Sections, db.SnapshotSection{SectionID: id, Code: strconv.Itoa(ch), Title: fmt.Sprintf("第%d部分 技术方案", ch), Level: 1, Seq: int(id), RevNo: 1, RevID: 1000 + id})
		in.Bodies[1000+id] = long
		for sub := 1; sub <= 5; sub++ {
			id++
			in.Doc.Sections = append(in.Doc.Sections, db.SnapshotSection{SectionID: id, Code: fmt.Sprintf("%d.%d", ch, sub), Title: fmt.Sprintf("实施内容%d-%d", ch, sub), Level: 2, Seq: int(id), RevNo: 1, RevID: 1000 + id})
			body := long + "\n\n" + long + "\n\n" + long
			if sub%2 == 0 {
				body += "\n\n设备清单如下：\n\n" + wide.String()
			}
			in.Bodies[1000+id] = body
		}
	}
	for i := 0; i < 120; i++ {
		row := base.Matrix[i%len(base.Matrix)]
		s := in.Doc.Sections[i%len(in.Doc.Sections)]
		row.RequirementID, row.Code, row.SectionID, row.SectionCode, row.SectionTitle = int64(i+1), fmt.Sprintf("R%03d", i+1), s.SectionID, s.Code, s.Title
		in.Matrix = append(in.Matrix, row)
	}
	return in
}

func refreshAndCheck(t *testing.T, o Office, in BuildInput, dir string) (pages int, took time.Duration) {
	t.Helper()
	tpl, _ := DefaultTemplate()
	res, err := Build(tpl, in)
	if err != nil {
		t.Fatal(err)
	}
	src, outDocx, outPDF := filepath.Join(dir, "in.docx"), filepath.Join(dir, "out.docx"), filepath.Join(dir, "out.pdf")
	if err := os.WriteFile(src, res.Data, 0o644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := o.Refresh(context.Background(), src, outDocx, outPDF); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	took = time.Since(start)
	pdf, err := os.ReadFile(outPDF)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := CheckPDF(pdf, tpl.Geometry)
	if err != nil {
		t.Fatal(err)
	}
	for _, is := range rep.Issues {
		t.Errorf("PDF issue (page %d) %s: %s", is.Page, is.Kind, is.Detail)
	}
	want := len(in.Doc.Sections) + 1 // + 偏离表附录
	if len(rep.TOC) != want {
		t.Fatalf("PDF TOC has %d entries, want %d", len(rep.TOC), want)
	}
	for i, e := range rep.TOC {
		if e.Listed != e.Actual || e.Actual == 0 {
			t.Fatalf("PDF TOC[%d] = %+v", i, e)
		}
	}
	docx, err := os.ReadFile(outDocx)
	if err != nil {
		t.Fatal(err)
	}
	drep, err := CheckDOCX(context.Background(), docx)
	if err != nil {
		t.Fatal(err)
	}
	if len(drep.TOC) != want {
		t.Fatalf("DOCX TOC has %d entries, want %d", len(drep.TOC), want)
	}
	for i, e := range drep.TOC {
		if e.Page != strconv.Itoa(rep.TOC[i].Actual) {
			t.Fatalf("DOCX TOC[%d] page %q, PDF heading on page %d", i, e.Page, rep.TOC[i].Actual)
		}
	}
	return rep.Pages, took
}

func TestOfficeLargeDocumentAndSoak(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("AUTOBID_OFFICE_SOAK"))
	if n <= 0 {
		t.Skip("设置 AUTOBID_OFFICE_SOAK=<次数> 运行文档引擎大文档与连续运行验证（耗时数分钟）")
	}
	o := requireOffice(t)
	eng := comEngines[o.Name()]
	before := listProcs(t, eng.image)
	start := time.Now()
	automation := func() int {
		c := 0
		for _, p := range listProcs(t, eng.image) {
			if (strings.Contains(p.Cmd, "/Automation") || strings.Contains(p.Cmd, "-Embedding")) && !p.Created.Before(start.Add(-2*time.Second)) {
				c++
			}
		}
		return c
	}

	pages, took := refreshAndCheck(t, o, largeInput(), t.TempDir())
	if pages < 100 {
		t.Errorf("large document has only %d pages", pages)
	}
	t.Logf("large document: %d pages, %d TOC entries, refreshed in %s", pages, len(largeInput().Doc.Sections)+1, took.Round(100*time.Millisecond))

	var durations []time.Duration
	var samplePages int
	for i := 0; i < n; i++ {
		p, d := refreshAndCheck(t, o, sampleInput(), t.TempDir())
		if samplePages == 0 {
			samplePages = p
		} else if p != samplePages {
			t.Errorf("run %d produced %d pages, first run %d", i+1, p, samplePages)
		}
		if left := automation(); left > 0 {
			t.Errorf("run %d left %d %s automation processes", i+1, left, eng.label)
		}
		durations = append(durations, d)
	}
	avg := func(ds []time.Duration) time.Duration {
		var s time.Duration
		for _, d := range ds {
			s += d
		}
		return s / time.Duration(len(ds))
	}
	k := max(1, min(5, n/2))
	first, last := avg(durations[:k]), avg(durations[len(durations)-k:])
	if last > 2*first+2*time.Second {
		t.Errorf("refresh slowed down: first %d average %s, last %d average %s", k, first, k, last)
	}
	after := listProcs(t, eng.image)
	for _, p := range before {
		if !alive(after, p.PID) {
			t.Errorf("pre-existing %s process %d was ended", eng.label, p.PID)
		}
	}
	t.Logf("%d consecutive refreshes on %s: %d pages each, first %d avg %s, last %d avg %s, total %s",
		n, o.Version(), samplePages, k, first.Round(100*time.Millisecond), k, last.Round(100*time.Millisecond), time.Since(start).Round(time.Second))
}
