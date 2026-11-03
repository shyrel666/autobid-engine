package main

// D3 确定性虚构数据集：招标/更正 DOCX、企业证据 JSON、多份虚构原件、缺陷库与证据普查表。
// 所有名称均为“演示/虚构”，不读取当前时间、不访问网络；同一 fixture_version 重复生成字节一致。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/autobid/autobid-engine/evidence"
	"github.com/autobid/autobid-engine/ingest/ingesttest"
)

const (
	fixtureVersion = 1
	tenderNo       = "DEMO-2026-SMART-PARK-D3-001"
	tenderName     = "智慧园区安防系统采购项目招标文件（虚构）"
	correctionName = "智慧园区安防系统采购项目更正公告（虚构）"
	bidderName     = "演示智联科技有限公司（虚构）"
	parentName     = "演示控股集团有限公司（虚构）"
	subsidiaryName = "演示智联子公司（虚构）"
	creditCode     = "DEMO00000000000000"

	tenderPublished     = "2026-10-01T09:00:00+08:00"
	correctionPublished = "2026-10-10T09:00:00+08:00"
	originalDeadline    = "2026-11-20T09:00:00+08:00"
	finalDeadline       = "2026-12-05T09:00:00+08:00"

	demoDOCXMIME = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

func docxParagraph(text string) ingesttest.Para { return ingesttest.Para{Text: text} }

func docxRow(header bool, cells ...string) ingesttest.Row {
	out := ingesttest.Row{Header: header}
	for _, c := range cells {
		out.Cells = append(out.Cells, ingesttest.Cell{Text: c})
	}
	return out
}

func buildTenderDOCX() []byte {
	qualification := ingesttest.Table{Cols: 3, Rows: []ingesttest.Row{
		docxRow(true, "序号", "审查因素", "审查内容"),
		docxRow(false, "1", "营业执照", "具有有效的营业执照"),
		docxRow(false, "2", "专业资质", "具备安全技术防范系统设计施工二级及以上资质"),
		docxRow(false, "3", "投标保证金", "按招标文件要求提交投标保证金"),
	}}
	conformity := ingesttest.Table{Cols: 3, Rows: []ingesttest.Row{
		docxRow(true, "序号", "审查因素", "审查内容"),
		docxRow(false, "1", "签署盖章", "投标文件按招标文件要求签署并加盖公章"),
		docxRow(false, "2", "投标有效期", "投标有效期不少于投标截止之日起 90 日历天"),
	}}
	scoring := ingesttest.Table{Cols: 4, Rows: []ingesttest.Row{
		docxRow(true, "序号", "评分因素", "分值", "评分标准"),
		docxRow(false, "1", "技术方案", "50", "总体架构合理、技术路线先进、功能完整，完全满足采购需求得50分，基本满足得30分。"),
		docxRow(false, "2", "类似项目业绩", "30", "每提供一个类似安防项目业绩得10分，最多得30分，须提供合同关键页及验收报告。"),
		docxRow(false, "3", "投标报价", "20", "按评标基准价公式计算，满足招标文件要求且投标价格最低的得20分。"),
	}}
	return ingesttest.BuildDOCX(tenderName,
		docxParagraph(tenderName),
		docxParagraph("招标编号："+tenderNo),
		docxParagraph("发布时间：2026-10-01 09:00（北京时间）"),
		docxParagraph("第一章 投标人须知"),
		docxParagraph("1.1 投标人资格要求"),
		docxParagraph("★1.1.1 资质要求"),
		docxParagraph("投标人须具备安全技术防范系统设计施工二级及以上资质，证书须在投标截止日有效，主体须为投标人自身，并提供资质证书原件（证书首页及有效期页）。"),
		docxParagraph("1.1.2 业绩要求"),
		docxParagraph("投标人须提供近三年 2 个类似安防项目业绩，须提供合同关键页（标的页、金额页、签章页）及验收报告，业绩主体须为投标人自身。"),
		docxParagraph("1.2 资格审查"),
		qualification,
		docxParagraph("1.3 符合性审查"),
		conformity,
		docxParagraph("1.4 投标截止时间"),
		docxParagraph("投标截止时间：2026年11月20日9时00分（北京时间）。"),
		docxParagraph("第二章 技术要求"),
		docxParagraph("2.1 总体技术要求"),
		docxParagraph("2.1.1 工期要求"),
		docxParagraph("投标人须承诺总工期不超过 90 日历天，自合同签订之日起计算。"),
		docxParagraph("2.1.2 质保要求"),
		docxParagraph("投标人须承诺免费质保期不少于 3 年，自项目验收合格之日起计算。"),
		docxParagraph("2.1.3 可用性要求"),
		docxParagraph("投标人须承诺系统可用性不低于 99.9%，保障业务连续运行。"),
		docxParagraph("2.2 售后服务要求"),
		docxParagraph("2.2.1 响应与到场要求"),
		docxParagraph("投标人须承诺接到故障报修后 2 小时内响应，4 小时内到达现场。"),
		docxParagraph("2.2.2 修复要求"),
		docxParagraph("投标人须承诺一般故障在 24 小时内修复完毕。"),
		docxParagraph("2.3 项目团队要求"),
		docxParagraph("2.3.1 项目经理要求"),
		docxParagraph("投标人须为本项目配备项目经理一名，须具备信息系统项目管理师资格，并提供近 6 个月社保缴纳记录。"),
		docxParagraph("2.3.2 团队人数要求"),
		docxParagraph("投标人须承诺投入本项目的实施团队不少于 12 人。"),
		docxParagraph("2.4 设备要求"),
		docxParagraph("2.4.1 核心摄像机型号要求"),
		docxParagraph("投标人须提供核心网络摄像机，型号为 SP-D2-4000，分辨率不低于 400 万像素，并提供产品彩页。"),
		docxParagraph("第三章 评标办法"),
		docxParagraph("3.1 评分标准"),
		scoring,
	)
}

func buildCorrectionDOCX() []byte {
	return ingesttest.BuildDOCX(correctionName,
		docxParagraph(correctionName),
		docxParagraph("一、投标截止时间更正"),
		docxParagraph("投标截止时间更正为 2026年12月5日9时00分（北京时间）。"),
	)
}

type originalSpec struct {
	Key       string
	Title     string
	Kind      string
	PageLabel string
	PageStart int
	PageEnd   int
	Body      []string
}

var originalSpecs = []originalSpec{
	{Key: "qual_valid", Title: "安全技术防范系统设计施工资质证书（虚构·有效）", Kind: "certificate", PageLabel: "证书首页", PageStart: 1, PageEnd: 1,
		Body: []string{"证书编号：DEMO-AF-0001", "持证单位：" + bidderName, "资质等级：二级", "有效期至：2027-06-30", "认证范围：安全技术防范系统设计施工"}},
	{Key: "qual_valid_validity", Title: "安全技术防范系统设计施工资质证书有效期页（虚构·有效）", Kind: "certificate", PageLabel: "有效期页", PageStart: 2, PageEnd: 2,
		Body: []string{"证书编号：DEMO-AF-0001", "有效期至：2027-06-30", "发证机构：虚构发证机构"}},
	{Key: "qual_critical", Title: "安全技术防范系统设计施工资质证书（虚构·临界）", Kind: "certificate", PageLabel: "证书首页", PageStart: 1, PageEnd: 1,
		Body: []string{"证书编号：DEMO-AF-0002", "持证单位：" + bidderName, "资质等级：二级", "有效期至：2026-11-25", "认证范围：安全技术防范系统设计施工"}},
	{Key: "qual_expired", Title: "安全技术防范系统设计施工资质证书（虚构·过期）", Kind: "certificate", PageLabel: "证书首页", PageStart: 1, PageEnd: 1,
		Body: []string{"证书编号：DEMO-AF-0003", "持证单位：" + bidderName, "资质等级：二级", "有效期至：2026-10-01"}},
	{Key: "perf1_contract", Title: "虚构智慧园区安防系统建设项目合同（关键页）", Kind: "contract", PageLabel: "标的页", PageStart: 3, PageEnd: 3,
		Body: []string{"项目名称：虚构智慧园区安防系统建设项目", "采购人：虚构产业园区管理委员会", "合同金额：8600000.00 元", "标的：视频监控、门禁与停车诱导系统集成"}},
	{Key: "perf1_amount", Title: "虚构智慧园区安防系统建设项目合同（金额页）", Kind: "contract", PageLabel: "金额页", PageStart: 4, PageEnd: 4,
		Body: []string{"合同金额：8600000.00 元", "签订日期：2024-03-01"}},
	{Key: "perf1_seal", Title: "虚构智慧园区安防系统建设项目合同（签章页）", Kind: "contract", PageLabel: "签章页", PageStart: 8, PageEnd: 8,
		Body: []string{"双方签章：虚构产业园区管理委员会 / " + bidderName}},
	{Key: "perf1_accept", Title: "虚构智慧园区安防系统建设项目验收报告", Kind: "acceptance_report", PageLabel: "验收结论", PageStart: 1, PageEnd: 1,
		Body: []string{"项目名称：虚构智慧园区安防系统建设项目", "验收结论：合格", "验收日期：2024-09-01"}},
	{Key: "perf2_contract", Title: "虚构政务服务中心安防改造项目合同（关键页）", Kind: "contract", PageLabel: "标的页", PageStart: 2, PageEnd: 2,
		Body: []string{"项目名称：虚构政务服务中心安防改造项目", "合同金额：4200000.00 元", "标的：安防平台升级与运维服务"}},
	{Key: "perf2_amount", Title: "虚构政务服务中心安防改造项目合同（金额页）", Kind: "contract", PageLabel: "金额页", PageStart: 3, PageEnd: 3,
		Body: []string{"合同金额：4200000.00 元", "签订日期：2025-02-10"}},
	{Key: "perf2_seal", Title: "虚构政务服务中心安防改造项目合同（签章页）", Kind: "contract", PageLabel: "签章页", PageStart: 6, PageEnd: 6,
		Body: []string{"双方签章：虚构市政务服务中心 / " + bidderName}},
	{Key: "perf2_accept", Title: "虚构政务服务中心安防改造项目验收报告", Kind: "acceptance_report", PageLabel: "验收结论", PageStart: 1, PageEnd: 1,
		Body: []string{"项目名称：虚构政务服务中心安防改造项目", "验收结论：合格", "验收日期：2025-08-15"}},
	{Key: "perf_sub_contract", Title: "子公司虚构安防项目合同（虚构）", Kind: "contract", PageLabel: "标的页", PageStart: 1, PageEnd: 1,
		Body: []string{"项目名称：子公司虚构安防项目", "主体：" + subsidiaryName, "标的：视频监控系统"}},
	{Key: "perf_sub_accept", Title: "子公司虚构安防项目验收报告（虚构）", Kind: "acceptance_report", PageLabel: "验收结论", PageStart: 1, PageEnd: 1,
		Body: []string{"项目名称：子公司虚构安防项目", "验收日期：2025-05-01"}},
	{Key: "perf_old_contract", Title: "窗口外虚构安防项目合同（虚构）", Kind: "contract", PageLabel: "标的页", PageStart: 1, PageEnd: 1,
		Body: []string{"项目名称：窗口外虚构安防项目", "签订日期：2019-01-01", "标的：安防系统"}},
	{Key: "perf_old_accept", Title: "窗口外虚构安防项目验收报告（虚构）", Kind: "acceptance_report", PageLabel: "验收结论", PageStart: 1, PageEnd: 1,
		Body: []string{"项目名称：窗口外虚构安防项目", "验收日期：2020-01-01"}},
	{Key: "perf_noacc_contract", Title: "缺验收虚构安防项目合同（虚构）", Kind: "contract", PageLabel: "标的页", PageStart: 1, PageEnd: 1,
		Body: []string{"项目名称：缺验收虚构安防项目", "签订日期：2025-03-01"}},
	{Key: "person_cert", Title: "项目经理信息系统项目管理师证书（虚构）", Kind: "certificate", PageLabel: "证书首页", PageStart: 1, PageEnd: 1,
		Body: []string{"姓名：演示项目经理（虚构）", "证书：信息系统项目管理师", "有效期至：2027-12-31"}},
	{Key: "person_ss", Title: "项目经理社保缴纳记录（虚构）", Kind: "social_security", PageLabel: "社保记录", PageStart: 1, PageEnd: 1,
		Body: []string{"姓名：演示项目经理（虚构）", "缴纳月数：12 个月", "参保单位：" + bidderName}},
	{Key: "product_ok", Title: "核心摄像机产品彩页（虚构·正确型号）", Kind: "product_catalog", PageLabel: "产品参数页", PageStart: 1, PageEnd: 1,
		Body: []string{"型号：SP-D2-4000", "分辨率：400 万像素", "制造商：" + bidderName}},
	{Key: "product_bad", Title: "核心摄像机产品彩页（虚构·错误型号）", Kind: "product_catalog", PageLabel: "产品参数页", PageStart: 1, PageEnd: 1,
		Body: []string{"型号：SP-D2-2000", "分辨率：200 万像素", "制造商：" + bidderName}},
}

type fixtureSet struct {
	Tender     []byte
	Correction []byte
	Originals  map[string][]byte
	Hashes     map[string]string
}

func buildOriginalDOCX(spec originalSpec) []byte {
	blocks := []any{docxParagraph(spec.Title)}
	for _, line := range spec.Body {
		blocks = append(blocks, docxParagraph(line))
	}
	blocks = append(blocks, docxParagraph("本文件为 E 级验收虚构原件，不具备法律效力。"))
	return ingesttest.BuildDOCX(spec.Title, blocks...)
}

func generateFixtures(artifactsDir, companyDir string) (*fixtureSet, error) {
	fs := &fixtureSet{Tender: buildTenderDOCX(), Correction: buildCorrectionDOCX(), Originals: map[string][]byte{}, Hashes: map[string]string{}}
	for _, spec := range originalSpecs {
		data := buildOriginalDOCX(spec)
		fs.Originals[spec.Key] = data
		fs.Hashes[spec.Key] = sha256Hex(data)
	}
	fixturesDir := filepath.Join(artifactsDir, "fixtures")
	if err := os.MkdirAll(fixturesDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(fixturesDir, "tender.docx"), fs.Tender, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(fixturesDir, "correction.docx"), fs.Correction, 0o644); err != nil {
		return nil, err
	}
	if err := writeCompanyFixtures(companyDir, fs); err != nil {
		return nil, err
	}
	return fs, nil
}

func writeCompanyFixtures(dir string, fs *fixtureSet) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	files := map[string]any{
		"company-profile.json": map[string]any{
			"fixture": true, "notice": "全部信息为虚构演示数据，不得用于真实投标。",
			"company_name": bidderName, "parent_name": parentName, "subsidiary_name": subsidiaryName,
			"unified_social_credit_code": creditCode,
		},
		"credentials.json": []map[string]any{
			{"category": "company_profile", "code": "PROFILE", "title": "企业主体关系（虚构）", "subject_name": bidderName, "subject_type": "bidder",
				"attributes": map[string]any{"legal_name": bidderName, "parent_name": parentName, "subsidiary_names": []string{subsidiaryName}}},
			{"category": "qualification", "code": "QUAL-VALID", "title": "安全技术防范系统设计施工二级（虚构·有效）", "subject_name": bidderName,
				"subject_type": "bidder", "certificate_no": "DEMO-AF-0001", "issuer": "虚构发证机构", "issued_on": "2024-07-01", "valid_until": "2027-06-30",
				"scope": "安全技术防范系统设计施工", "attributes": map[string]any{"level": "二级"}},
			{"category": "qualification", "code": "QUAL-CRITICAL", "title": "安全技术防范系统设计施工二级（虚构·临界）", "subject_name": bidderName,
				"subject_type": "bidder", "certificate_no": "DEMO-AF-0002", "issuer": "虚构发证机构", "issued_on": "2023-11-25", "valid_until": "2026-11-25",
				"scope": "安全技术防范系统设计施工", "attributes": map[string]any{"level": "二级"}},
			{"category": "qualification", "code": "QUAL-EXPIRED", "title": "安全技术防范系统设计施工二级（虚构·过期）", "subject_name": bidderName,
				"subject_type": "bidder", "certificate_no": "DEMO-AF-0003", "issuer": "虚构发证机构", "issued_on": "2020-10-01", "valid_until": "2026-10-01",
				"scope": "安全技术防范系统设计施工", "attributes": map[string]any{"level": "二级"}},
			{"category": "personnel", "code": "PM-1", "title": "项目经理（虚构）", "subject_name": "演示项目经理（虚构）", "subject_type": "person",
				"certificate_no": "DEMO-PM-0001", "issuer": "虚构行业协会", "issued_on": "2024-01-01", "valid_until": "2027-12-31",
				"attributes": map[string]any{"person": "演示项目经理（虚构）", "role": "项目经理", "social_security_months": 12}},
			{"category": "product", "code": "CAM-OK", "title": "核心网络摄像机 SP-D2-4000（虚构）", "subject_name": bidderName, "subject_type": "bidder",
				"valid_until": "2027-12-31", "scope": "视频监控设备",
				"attributes": map[string]any{"model": "SP-D2-4000", "parameters": map[string]any{"resolution": "400万", "protocol": "ONVIF"}}},
			{"category": "product", "code": "CAM-BAD", "title": "核心网络摄像机 SP-D2-2000（虚构）", "subject_name": bidderName, "subject_type": "bidder",
				"valid_until": "2027-12-31", "scope": "视频监控设备",
				"attributes": map[string]any{"model": "SP-D2-2000", "parameters": map[string]any{"resolution": "200万", "protocol": "ONVIF"}}},
		},
		"performance.json": []map[string]any{
			{"code": "PERF-1", "project_name": "虚构智慧园区安防系统建设项目", "buyer": "虚构产业园区管理委员会", "contract_no": "DEMO-HT-0001",
				"contract_date": "2024-03-01", "acceptance_date": "2024-09-01", "amount": "8600000.0000", "currency": "CNY", "subject_name": bidderName,
				"subject_type": "bidder", "is_subsidiary": false, "scope": "视频监控、门禁与停车诱导系统集成"},
			{"code": "PERF-2", "project_name": "虚构政务服务中心安防改造项目", "buyer": "虚构市政务服务中心", "contract_no": "DEMO-HT-0002",
				"contract_date": "2025-02-10", "acceptance_date": "2025-08-15", "amount": "4200000.0000", "currency": "CNY", "subject_name": bidderName,
				"subject_type": "bidder", "is_subsidiary": false, "scope": "安防平台升级与运维服务"},
			{"code": "PERF-SUB", "project_name": "子公司虚构安防项目", "buyer": "虚构客户", "contract_no": "DEMO-HT-SUB",
				"contract_date": "2025-01-01", "acceptance_date": "2025-05-01", "amount": "3000000.0000", "currency": "CNY", "subject_name": subsidiaryName,
				"subject_type": "subsidiary", "is_subsidiary": true, "scope": "视频监控系统集成"},
			{"code": "PERF-OLD", "project_name": "窗口外虚构安防项目", "buyer": "虚构客户", "contract_no": "DEMO-HT-OLD",
				"contract_date": "2019-01-01", "acceptance_date": "2020-01-01", "amount": "1000000.0000", "currency": "CNY", "subject_name": bidderName,
				"subject_type": "bidder", "is_subsidiary": false, "scope": "安防系统集成"},
			{"code": "PERF-NOACC", "project_name": "缺验收虚构安防项目", "buyer": "虚构客户", "contract_no": "DEMO-HT-NOACC",
				"contract_date": "2025-03-01", "acceptance_date": nil, "amount": "900000.0000", "currency": "CNY", "subject_name": bidderName,
				"subject_type": "bidder", "is_subsidiary": false, "scope": "安防系统集成"},
		},
		"originals.json": originalSpecs,
		"hashes.json":    fs.Hashes,
	}
	for name, v := range files {
		raw, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		raw = append(raw, '\n')
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (fs *fixtureSet) putOriginal(store *evidence.Store, tenant, key string) (rel, sum string, err error) {
	data, ok := fs.Originals[key]
	if !ok {
		return "", "", fmt.Errorf("unknown original %q", key)
	}
	return store.Put(tenant, "docx", data)
}

func originalSpecByKey(key string) (originalSpec, bool) {
	for _, s := range originalSpecs {
		if s.Key == key {
			return s, true
		}
	}
	return originalSpec{}, false
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
