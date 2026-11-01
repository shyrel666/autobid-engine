// Command gen 生成 F0 复杂文件处理样本。样本全部为合成工程样本，只证明工程边界，
// 不代表真实招标材料的提取质量。生成结果按固定时间戳写入，重复执行字节一致。
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/autobid/autobid-engine/ingest/ingesttest"
	"github.com/xuri/excelize/v2"
	"golang.org/x/text/encoding/simplifiedchinese"
)

var fixedTime = time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)

type entry struct {
	name   string
	data   []byte
	method uint16
	mode   os.FileMode
}

func main() {
	out := flag.String("out", "eval/file-processing/fixtures", "输出目录")
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	xlsx := buildXLSX()
	write(*out, "clean.xlsx", xlsx)
	write(*out, "gb18030.csv", buildCSVGB18030())
	write(*out, "quote.tsv", buildTSV())
	write(*out, "safe.zip", buildSafeZIP(xlsx))
	write(*out, "store-docx.zip", buildStoreDOCXZIP())
	write(*out, "not_docx.docx", buildNotDOCX())
	write(*out, "zip-traversal.zip", buildZip("zip-traversal.zip", []entry{{name: "../evil.txt", data: []byte("evil")}}, deflate))
	write(*out, "zip-absolute.zip", buildZip("", []entry{{name: "/etc/passwd", data: []byte("root")}}, deflate))
	write(*out, "zip-drive.zip", buildZip("", []entry{{name: "C:/evil.txt", data: []byte("evil")}}, deflate))
	write(*out, "zip-backslash.zip", buildZip("", []entry{{name: `a\b.txt`, data: []byte("evil")}}, deflate))
	write(*out, "zip-symlink.zip", buildZip("", []entry{{name: "link", data: []byte("../outside"), method: zip.Store, mode: os.ModeSymlink | 0o777}}, deflate))
	write(*out, "zip-duplicate.zip", buildZip("", []entry{
		{name: "a/b.txt", data: []byte("one")},
		{name: "./a/b.txt", data: []byte("two")},
	}, deflate))
	write(*out, "zip-nested.zip", buildZip("", []entry{{name: "inner.zip", data: buildZip("", []entry{{name: "x.txt", data: []byte("x")}}, deflate)}}, deflate))
	write(*out, "zip-ratio.zip", buildZip("", []entry{{name: "zeros.txt", data: bytes.Repeat([]byte("0"), 512<<10)}}, deflate))
	var many []entry
	for i := 0; i < 201; i++ {
		many = append(many, entry{name: fmt.Sprintf("files/%03d.txt", i), data: []byte("x")})
	}
	write(*out, "zip-many.zip", buildZip("", many, deflate))
	write(*out, "zip-oversize.zip", buildZip("", []entry{{name: "big.bin", data: bytes.Repeat([]byte("A"), 96<<10)}}, deflate))
	fmt.Println("generated", *out)
}

const (
	store   = zip.Store
	deflate = zip.Deflate
)

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func write(dir, name string, data []byte) {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fatal(err)
	}
	sum := sha256.Sum256(data)
	fmt.Printf("%s %s %d\n", hex.EncodeToString(sum[:]), name, len(data))
}

func buildXLSX() []byte {
	f := excelize.NewFile()
	defer f.Close()
	const quote = "报价清单"
	_ = f.SetSheetName("Sheet1", quote)
	must(f.SetCellValue(quote, "A1", "智慧园区设备报价清单（虚构样本）"))
	must(f.MergeCell(quote, "A1", "F1"))
	headers := []string{"序号", "设备名称", "型号", "数量", "单价（元）", "合价（元）", "交付日期"}
	for i, h := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 2)
		must(f.SetCellValue(quote, cell, h))
	}
	money, _ := f.NewStyle(&excelize.Style{NumFmt: 4}) // #,##0.00
	dateStyle, _ := f.NewStyle(&excelize.Style{NumFmt: 14})
	rows := [][]any{
		{1, "网络摄像机", "CAM-001", 10, 1250.50},
		{2, "门禁控制器", "AC-0007", 2, 8800},
		{3, "线缆", "CBL-000123", "00100", 12.8},
		{4, "安装服务", "-", 1, 30000},
	}
	for r, row := range rows {
		rowNo := r + 3
		for c, v := range row {
			cell, _ := excelize.CoordinatesToCellName(c+1, rowNo)
			must(f.SetCellValue(quote, cell, v))
		}
		must(f.SetCellStyle(quote, fmt.Sprintf("E%d", rowNo), fmt.Sprintf("E%d", rowNo), money))
		must(f.SetCellStyle(quote, fmt.Sprintf("F%d", rowNo), fmt.Sprintf("F%d", rowNo), money))
		must(f.SetCellFormula(quote, fmt.Sprintf("F%d", rowNo), fmt.Sprintf("D%d*E%d", rowNo, rowNo)))
	}
	must(f.SetCellValue(quote, "G3", time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)))
	must(f.SetCellStyle(quote, "G3", "G3", dateStyle))
	must(f.SetCellValue(quote, "H2", "资产编号"))
	must(f.SetCellValue(quote, "H3", "000012345678901234"))
	_ = f.SetRowVisible(quote, 5, false)
	_ = f.SetColVisible(quote, "E", false)

	scoring := "评分表"
	_, _ = f.NewSheet(scoring)
	must(f.SetCellValue(scoring, "A1", "技术评分表（虚构样本）"))
	must(f.MergeCell(scoring, "A1", "D1"))
	scoringRows := [][]any{
		{"序号", "评分因素", "分值", "评分标准"},
		{1, "技术方案", 50, "总体架构合理、功能完整得50分，基本满足得30分。"},
		{2, "类似项目业绩", 30, "每提供一个类似项目业绩得10分，最多得30分，须提供合同关键页及验收报告。"},
		{3, "投标报价", 20, "满足招标文件要求且投标价格最低的得20分。"},
	}
	for r, row := range scoringRows {
		for c, v := range row {
			cell, _ := excelize.CoordinatesToCellName(c+1, r+2)
			must(f.SetCellValue(scoring, cell, v))
		}
	}

	hidden := "隐藏页"
	_, _ = f.NewSheet(hidden)
	must(f.SetCellValue(hidden, "A1", "★ 隐藏工作表仍须提取：交付物须包含全部设备清单。"))
	_ = f.SetSheetVisible(hidden, false)

	_, _ = f.NewSheet("空表")
	buf, err := f.WriteToBuffer()
	if err != nil {
		fatal(err)
	}
	return buf.Bytes()
}

func buildCSVGB18030() []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	w.Comma = ';'
	rows := [][]string{
		{"序号", "产品名称", "型号", "数量", "单价", "备注"},
		{"1", "交换机", "SW-000045", "0008", "12800.50", "含三年维保"},
		{"2", "防火墙", "FW-000000012345", "2", "86000.00", "含“报价;含税”\n第二行说明"},
	}
	for _, row := range rows {
		if err := w.Write(row); err != nil {
			fatal(err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		fatal(err)
	}
	out, err := simplifiedchinese.GB18030.NewEncoder().Bytes(buf.Bytes())
	if err != nil {
		fatal(err)
	}
	return out
}

func buildTSV() []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	w.Comma = '\t'
	rows := [][]string{
		{"序号", "资产编号", "启用日期", "原值（元）", "备注"},
		{"1", "000000000000000001", "2026-12-15", "1250.50", "摄像机"},
		{"2", "000000000000000002", "2026-12-16", "8800.00", "门禁控制器"},
	}
	for _, row := range rows {
		if err := w.Write(row); err != nil {
			fatal(err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		fatal(err)
	}
	return buf.Bytes()
}

func buildSafeZIP(xlsx []byte) []byte {
	docx := ingesttest.BuildDOCX("智慧园区项目招标文件（虚构）",
		ingesttest.Para{Text: "第一章 投标人须知"},
		ingesttest.Para{Text: "★1.1 投标人须具备有效营业执照。"},
		ingesttest.Table{Cols: 3, Rows: []ingesttest.Row{
			{Header: true, Cells: []ingesttest.Cell{{Text: "序号"}, {Text: "审查因素"}, {Text: "审查内容"}}},
			{Cells: []ingesttest.Cell{{Text: "1"}, {Text: "营业执照"}, {Text: "具有有效的营业执照"}}},
		}},
	)
	return buildZip("", []entry{
		{name: "招标文件.docx", data: docx},
		{name: "附件/报价清单.xlsx", data: xlsx},
		{name: "附件/数据.csv", data: []byte("编号,名称\n001,摄像机\n")},
		{name: "说明.txt", data: []byte("本压缩包为虚构工程样本。\n")},
	}, deflate)
}

func buildStoreDOCXZIP() []byte {
	docx := ingesttest.BuildDOCX("Store 方式收入的 DOCX（虚构）",
		ingesttest.Para{Text: "第一章 总则"},
		ingesttest.Para{Text: "1.1 本文件以 Store 方式压缩。"},
	)
	return buildZip("", []entry{{name: "标包.docx", data: docx, method: store}}, store)
}

func buildNotDOCX() []byte {
	return buildZip("", []entry{{name: "readme.txt", data: []byte("this is not a docx")}}, deflate)
}

func buildZip(_ string, entries []entry, method uint16) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: method}
		if e.method != 0 {
			h.Method = e.method
		}
		h.Modified = fixedTime
		if e.mode != 0 {
			h.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			fatal(err)
		}
		if _, err := w.Write(e.data); err != nil {
			fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		fatal(err)
	}
	return buf.Bytes()
}

func must(err error) {
	if err != nil {
		fatal(err)
	}
}
