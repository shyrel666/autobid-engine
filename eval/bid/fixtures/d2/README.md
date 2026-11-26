# D2 工程验收夹具

`eval/bid/d2` 使用可重复生成的虚构数据验证代码、PostgreSQL 状态机、规则与门禁。公开开发集的原件摘录和抽查期望由 `ingest` 集成测试读取。 [公开开发集核对](../../README.md#d2-评分与证明材料原件核对)

## 数据边界

- 企业：演示智联科技有限公司（虚构）；母公司、子公司均为虚构名称；统一社会信用代码为明显假号 `DEMO00000000000000`。
- 招标项目：智慧园区安防系统采购项目（虚构），编号 `DEMO-2026-SMART-PARK-D2-001`；与 M1 的 `-001` 项目区分。
- 原件由 `ingest/ingesttest.BuildDOCX` 确定性生成，只用于解析、哈希与页码登记，不具备法律效力，也不宣称等同真实扫描件版式。
- 不读取当前时间、不访问网络、不调用大模型；同一 `fixture_version` 重复生成字节一致。

## 文件

| 文件 | 用途 |
| --- | --- |
| `golden.json` | 冻结的黄金期望（`_status=frozen`）。执行器拒绝未冻结 golden。证据主体 / 有效期 / 原件字号与哈希在证据普查后冻结。 |
| `defects.json` | 合成缺陷库：每个 D2 缺陷对应一个门禁（G01~G14）。 |
| `company/credentials.json` | 虚构资质、人员、产品与主体关系。 |
| `company/performance.json` | 虚构业绩（投标人自身、子公司、窗口外、缺验收）。 |
| `company/originals.json` | 原件构造参数与页角色。 |
| `company/hashes.json` | 确定性原件 SHA-256（与 golden.originals 一致）。 |
| `public-proof-a.json`、`public-proof-b.json` | 36 个公开开发集项目的 69 条原件抽查，记录来源页或章节、评分项、原件证明要求和已知差异。分块、表和行索引从 0 开始。 |

## 运行

```bash
export AUTOBID_TEST_DATABASE_URL=postgres://...   # 只从环境变量读取，不写入报告
go run ./eval/bid/d2 -report-dir . -artifacts-dir ./d2-artifacts -verify-determinism
```

- 主链与每个门禁使用独立租户 `d2-<runid>-r<round>-<scenario>` 与任务；默认精确清理本次任务与租户证据。
- 原件写入 `d2-artifacts/evidence/`（可用 `AUTOBID_EVIDENCE_DIR` 覆盖），数据库只保存相对路径与 SHA-256。
- 虚构企业 JSON 默认生成到 `d2-artifacts/fixtures/company/`，可用 `-company-fixtures` 指定其他输出目录。
- `D2_ACCEPTANCE_REPORT.md`、`d2-acceptance-report.json`、`d2-artifacts/` 只保留在本地，不提交 Git。

## 冻结规则

`golden.json` 中的期望值在看到验收结果之前冻结；发现解析器 / 规则确有 bug 时修代码并补单元测试，不放宽 golden。`manual` 结论不得当作 `admissible`。
