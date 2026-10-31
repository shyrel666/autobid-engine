# E 级端到端验收夹具（虚构数据）

本目录与 `../accept/` 中的执行器配套，只用于 **E 级工程验收**：证明代码、PostgreSQL 状态机与门禁按设计工作。
它不是 V 级业务效果验证，也不是 P 级生产启用。全部项目、企业、证书与业绩均为虚构，不与 `ccgp_2026-10-06/` 或任何真实项目、企业关联。

## 目录

| 路径 | 说明 | M1 是否消费 |
| --- | --- | --- |
| `golden.json` | 首轮复核后冻结的黄金期望 | 是（执行器逐项核对） |
| `company/company-profile.json` | 虚构企业名称、统一社会信用代码（明显假号）、注册地址 | 否，仅生成 |
| `company/credentials.json` | 虚构资质证书与有效期 | 否，仅生成 |
| `company/performance.json` | 虚构类似项目业绩 | 否，仅生成 |

招标 DOCX、更正公告 DOCX 与 G09 专用 DOCX 由 `eval/bid/accept/dataset.go` 在每次运行时确定性生成到 `e2e-artifacts/fixtures/`。
生成过程不读取当前时间、不访问网络；同一 `fixture_version` 重复生成字节一致。

## 主链招标文件（`tender.docx`）

- 项目名称：智慧园区安防系统采购项目（虚构）
- 招标编号：`DEMO-2026-SMART-PARK-001`
- 发布时间：`2026-10-01 09:00 +08:00`
- 结构：投标人须知（资格 / 符合性审查表、投标截止时间）、技术要求（工期、质保、可用性、售后、团队、驻场）、评标办法（评分表）。

每条预期要求都使用独立编号标题，并被 `ingest.SplitLines` 切成独立 `document_chunks`。执行器先输出
`e2e-artifacts/extraction-census.md`（分块编号、标题、内容摘要、提取要求），再运行要求提取，避免把多个条款压进同一分块后误判解析器。

## 更正公告（`correction.docx`）

- 发布时间：`2026-10-10 09:00 +08:00`（晚于招标文件，效力优先）
- 只覆盖 `2.3.2 驻场服务要求`，文字由“持有厂家认证证书”更正为“持有有效期内的厂家认证证书及身份证明”。
- 期望：旧要求 `stale` 且指向新要求，提取结果 `Superseded = 1`，大纲只映射新要求。

## G09 专用一致性冲突夹具

`buildG09TenderDOCX()` 生成两条独立要求（建设工期 / 交付周期均允许 120 天）与两个自定义章节。
初始正文一章写“总工期 90 天”、另一章写“总工期 120 天”，两条响应都满足各自矩阵门槛，但一致性核验应将同一工期指标的不同承诺记为矛盾。

## 边界

- 不导入知识库、不绑定证据；虚构企业 JSON 不属于 M1 通过条件，报告固定写 `evidence_binding = BLOCKED`。
- 不调用真实大模型；替身写作只验证流程，不代表写作质量。
- 不覆盖 HTTP / SSE / UI、PDF 导出、LibreOffice 目录页码刷新、报价与承诺台账核验、多 Agent。
- 数据库连接串只从 `AUTOBID_TEST_DATABASE_URL` 读取，不写入报告或夹具。

## 复现

```bash
go run ./eval/bid/accept -report-dir . -artifacts-dir ./e2e-artifacts -verify-determinism
```
