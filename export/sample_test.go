package export

// 测试样本：长正文、12 列宽表、架构图（PNG）、宽甘特图（PNG）、未渲染图表、设备选型表与长偏离表。

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/compliance"
	"github.com/autobid/autobid-engine/db"
)

// stubPNG 生成指定尺寸的 PNG（带边框与色块），代替 mmdc 渲染结果用于版式测试。
func stubPNG(w, h int, seed uint8) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{255, 255, 255, 255}
			switch {
			case x < 4 || y < 4 || x >= w-4 || y >= h-4:
				c = color.RGBA{30, 60, 120, 255}
			case (x/120+y/90)%3 == int(seed)%3 && (x%120) > 10 && (y%90) > 10:
				c = color.RGBA{180, 200, 235, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// SampleInput 构造一份覆盖版式验收要素的导出输入。
func sampleInput() BuildInput {
	long := strings.Repeat("本系统采用分层解耦架构，感知层、网络层、平台层与应用层之间通过标准接口交互，保证系统的可扩展性与可维护性。", 12)
	var wide strings.Builder
	wide.WriteString("| 序号 | 设备名称 | 品牌 | 型号 | 分辨率 | 帧率 | 编码 | 存储 | 接口 | 功耗 | 防护 | 质保 | 数量 |\n")
	wide.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for i := 1; i <= 18; i++ {
		fmt.Fprintf(&wide, "| %d | 高清网络摄像机%d | 海康威视 | DS-2CD3T47WDV3-L%d | 400万像素 2560×1440 | 25fps | H.265+ | 256GB | RJ45×1 | 6.5W | IP67 | 3年 | %d |\n", i, i, i, i*2)
	}
	sections := []db.SnapshotSection{
		{SectionID: 1, Code: "1", Title: "总体技术方案", Level: 1, Seq: 0, RevNo: 1, RevID: 101},
		{SectionID: 2, Code: "1.1", Title: "系统总体架构", Level: 2, Seq: 1, RevNo: 1, RevID: 102},
		{SectionID: 3, Code: "1.2", Title: "前端感知设备选型", Level: 2, Seq: 2, RevNo: 2, RevID: 103},
		{SectionID: 4, Code: "2", Title: "实施进度计划", Level: 1, Seq: 3, RevNo: 1, RevID: 104},
		{SectionID: 5, Code: "3", Title: "项目组织与人员", Level: 1, Seq: 4, RevNo: 1, RevID: 105},
		{SectionID: 6, Code: "4", Title: "售后服务方案", Level: 1, Seq: 5, RevNo: 1, RevID: 106},
	}
	bodies := map[int64]string{
		101: "## 总体技术方案\n\n" + long + "\n\n- **安全可靠**：满足等保三级要求\n- 统一运维\n  - 集中告警\n  - 自动巡检\n\n1. 第一阶段\n2. 第二阶段",
		102: long + "\n\n系统总体架构见下图。",
		103: "前端设备选型如下表所示（12 列以上的宽表格放入横向页面）：\n\n" + wide.String() + "\n上述设备均满足招标技术参数要求。",
		104: long + "\n\n实施进度安排见甘特图。",
		105: long,
		106: long + "\n\n我方承诺 7×24 小时响应，故障响应时间不超过 1 小时。",
	}
	charts := []db.SnapshotChart{
		{ChartID: 11, SectionID: 2, RevNo: 1, ChartType: db.ChartArchitecture, Title: "系统总体架构图", SourceFormat: db.ChartSourceMermaid,
			Source: "flowchart TB\n  A-->B", RenderStatus: db.ChartRendered, RenderFormat: db.ChartFormatPNG, WidthPx: 1600, HeightPx: 1100},
		{ChartID: 12, SectionID: 4, RevNo: 1, ChartType: db.ChartGantt, Title: "实施进度甘特图", SourceFormat: db.ChartSourceMermaid,
			Source: "gantt\n  title x", RenderStatus: db.ChartRendered, RenderFormat: db.ChartFormatPNG, WidthPx: 2400, HeightPx: 720},
		{ChartID: 13, SectionID: 5, RevNo: 1, ChartType: db.ChartOrg, Title: "项目组织架构图", SourceFormat: db.ChartSourceMermaid,
			Source: "flowchart TD\n  o1[\"项目经理\"]\n  o2[\"技术负责人\"]\n  o1 --> o2", RenderStatus: db.ChartUnrendered, RenderError: "mmdc 不可用"},
		{ChartID: 14, SectionID: 3, RevNo: 2, ChartType: db.ChartEquipmentTable, Title: "核心设备选型表", SourceFormat: db.ChartSourceJSON,
			Source:       `{"columns":["序号","设备","型号","数量"],"rows":[["1","核心交换机","S7706","2"],["2","存储服务器","R5300 G5","4"]]}`,
			RenderStatus: db.ChartRendered, RenderFormat: db.ChartFormatNative},
	}
	var matrix []db.SnapshotMatrixRow
	kinds := []string{compliance.KindStar, compliance.KindScoring, compliance.KindRequired, compliance.KindTriangle}
	for i := 1; i <= 60; i++ {
		s := sections[i%len(sections)]
		matrix = append(matrix, db.SnapshotMatrixRow{RequirementID: int64(i), Code: fmt.Sprintf("R%02d", i), Title: fmt.Sprintf("第 %d 条技术要求：系统应支持多级联网与统一管理", i),
			Kind: kinds[i%len(kinds)], SectionID: s.SectionID, SectionCode: s.Code, SectionTitle: s.Title, RevNo: s.RevNo, Anchor: fmt.Sprintf("¶%d", i%5+1),
			ResponseSummary: "完全满足：本方案在对应章节给出了实现方式与承诺指标。", Deviation: compliance.DeviationNone, VerifyStatus: compliance.StatusVerified})
	}
	return BuildInput{
		Doc:    db.DocSnapshot{TaskName: "某市智慧园区安防项目", OutlineVersion: 3, Sections: sections, CreatedAt: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)},
		Bodies: bodies, Charts: charts, Matrix: matrix, Bidder: "示例科技有限公司", Date: "2026-10-28",
		ChartImages: map[int64][]byte{11: stubPNG(1600, 1100, 1), 12: stubPNG(2400, 720, 2)},
	}
}
