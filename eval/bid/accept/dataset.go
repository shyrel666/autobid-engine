package main

// 确定性虚构数据集：招标 DOCX、更正 DOCX 与 M1 不消费的虚构企业 JSON。
// 所有名称均为虚构演示标识，不读取当前时间、不访问网络；同一版本重复生成字节一致。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/autobid/autobid-engine/ingest/ingesttest"
)

const (
	demoTenant     = "e2e-autobid"
	demoTenderNo   = "DEMO-2026-SMART-PARK-001"
	demoTenderName = "智慧园区安防系统采购项目招标文件（虚构）"
	demoCorrection = "智慧园区安防系统采购项目更正公告（虚构）"
	demoBidder     = "演示智联科技有限公司（虚构）"
	demoCompany    = "虚构省虚构市演示路 1 号"
	demoCreditCode = "DEMO00000000000000"

	demoDOCXMIME = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

// fixtureSet 是本次运行使用的固定夹具字节。
type fixtureSet struct {
	TenderPath     string
	CorrectionPath string
	G09Path        string
	Tender         []byte
	Correction     []byte
	G09            []byte
}

func docxParagraph(text string) ingesttest.Para { return ingesttest.Para{Text: text} }

func docxRow(header bool, cells ...string) ingesttest.Row {
	out := ingesttest.Row{Header: header}
	for _, c := range cells {
		out.Cells = append(out.Cells, ingesttest.Cell{Text: c})
	}
	return out
}

// buildTenderDOCX 构造主链招标文件。每条预期要求都是独立编号标题，避免把多个条款压进同一分块。
func buildTenderDOCX() []byte {
	qualification := ingesttest.Table{Cols: 3, Rows: []ingesttest.Row{
		docxRow(true, "序号", "审查因素", "审查内容"),
		docxRow(false, "1", "营业执照", "具有有效的营业执照"),
		docxRow(false, "2", "专业资质", "具备安全技术防范系统设计施工资质"),
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
		docxRow(false, "2", "类似项目业绩", "30", "每提供一个类似项目业绩得10分，最多得30分，须提供合同关键页及验收报告。"),
		docxRow(false, "3", "投标报价", "20", "按评标基准价公式计算，满足招标文件要求且投标价格最低的得20分。"),
	}}
	return ingesttest.BuildDOCX(demoTenderName,
		docxParagraph(demoTenderName),
		docxParagraph("招标编号："+demoTenderNo),
		docxParagraph("发布时间：2026-10-01 09:00（北京时间）"),
		docxParagraph("第一章 投标人须知"),
		docxParagraph("1.1 投标人资格要求"),
		docxParagraph("★1.1.1 资质要求"),
		docxParagraph("投标人须具备安全技术防范系统设计施工二级及以上资质，并提供资质证书复印件。"),
		docxParagraph("1.1.2 业绩要求"),
		docxParagraph("投标人须提供近三年类似项目业绩证明材料，包括合同关键页及验收报告。"),
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
		docxParagraph("2.3.1 团队要求"),
		docxParagraph("投标人须承诺投入本项目的实施团队不少于 12 人，其中项目经理须具备信息系统项目管理师资格。"),
		docxParagraph("2.3.2 驻场服务要求"),
		docxParagraph("投标人须提供驻场服务，驻场服务人员须持有厂家认证证书。"),
		docxParagraph("第三章 评标办法"),
		docxParagraph("3.1 评分标准"),
		scoring,
	)
}

// buildCorrectionDOCX 构造只覆盖 2.3.2 的更正公告（文字变更，避免数值覆盖造成一致性噪声）。
// 正文只保留一个章节：更正类文件的所有非空分块都会被提取，标题或章前文字会变成额外要求。
func buildCorrectionDOCX() []byte {
	return ingesttest.BuildDOCX(demoCorrection,
		docxParagraph("2.3.2 驻场服务要求"),
		docxParagraph("原要求“驻场服务人员须持有厂家认证证书”，现更正为“驻场服务人员须持有有效期内的厂家认证证书及身份证明”。"),
	)
}

// buildG09TenderDOCX 构造 G09 专用夹具：两条独立要求都允许 120 天。
func buildG09TenderDOCX() []byte {
	return ingesttest.BuildDOCX("G09 一致性冲突夹具（虚构）",
		docxParagraph("G09 一致性冲突夹具（虚构）"),
		docxParagraph("第一章 工期条款"),
		docxParagraph("1.1 建设工期"),
		docxParagraph("投标人须承诺建设工期不超过 120 天。"),
		docxParagraph("1.2 交付周期"),
		docxParagraph("投标人须承诺交付周期不超过 120 天。"),
	)
}

// generateFixtures 写出确定性的 DOCX 与企业 JSON，并返回本次字节。
func generateFixtures(artifactsDir, companyDir string) (*fixtureSet, error) {
	fs := &fixtureSet{Tender: buildTenderDOCX(), Correction: buildCorrectionDOCX(), G09: buildG09TenderDOCX()}
	fixturesDir := filepath.Join(artifactsDir, "fixtures")
	if err := os.MkdirAll(fixturesDir, 0o755); err != nil {
		return nil, err
	}
	fs.TenderPath = filepath.Join(fixturesDir, "tender.docx")
	fs.CorrectionPath = filepath.Join(fixturesDir, "correction.docx")
	fs.G09Path = filepath.Join(fixturesDir, "g09-consistency.docx")
	for path, data := range map[string][]byte{fs.TenderPath: fs.Tender, fs.CorrectionPath: fs.Correction, fs.G09Path: fs.G09} {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, err
		}
	}
	if err := writeCompanyFixtures(companyDir); err != nil {
		return nil, err
	}
	return fs, nil
}

// writeCompanyFixtures 生成 M1 只生成、不消费的虚构企业资料。
func writeCompanyFixtures(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	files := map[string]any{
		"company-profile.json": map[string]any{
			"fixture":                    true,
			"notice":                     "全部信息为虚构演示数据，不得用于真实投标。",
			"company_name":               demoBidder,
			"unified_social_credit_code": demoCreditCode,
			"registered_address":         demoCompany,
			"established":                "2020-01-01",
			"registered_capital_cny":     10000000,
		},
		"credentials.json": []map[string]any{
			{"name": "安全技术防范系统设计施工资质（虚构）", "level": "二级", "issuer": "虚构发证机构", "valid_until": "2027-12-31", "holder": demoBidder},
			{"name": "信息系统建设和服务能力证书（虚构）", "level": "CS2", "issuer": "虚构行业协会", "valid_until": "2028-06-30", "holder": demoBidder},
		},
		"performance.json": []map[string]any{
			{"project": "虚构智慧园区安防系统建设项目", "buyer": "虚构产业园区管理委员会", "year": 2024, "amount_cny": 8600000,
				"scope": "视频监控、门禁与停车诱导系统集成"},
			{"project": "虚构政务服务中心安防改造项目", "buyer": "虚构市政务服务中心", "year": 2025, "amount_cny": 4200000,
				"scope": "安防平台升级与运维服务"},
		},
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

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func fileSHA(path string) (string, int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	return sha256Hex(raw), int64(len(raw)), nil
}

func artifactOf(kind, path, note string) (Artifact, error) {
	sum, size, err := fileSHA(path)
	if err != nil {
		return Artifact{}, fmt.Errorf("artifact %s: %w", kind, err)
	}
	return Artifact{Kind: kind, Path: filepath.ToSlash(path), Size: size, SHA256: sum, Note: note}, nil
}
