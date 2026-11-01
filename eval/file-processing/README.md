# eval/file-processing — 复杂文件处理评测与样本

状态（2026-10-31）：**F0 / F1 已完成（E 级）；F2 启动关口未通过**。F1 验收（原件留存、XLSX/CSV/TSV/ZIP、表格切分与溯源、解析修订门禁）见 [`docs/file-processing/F1_ACCEPTANCE.md`](../../docs/file-processing/F1_ACCEPTANCE.md)。本目录只保存工程样本、人工期望、冻结契约和可复现实验工具，不把合成样本结果当作真实提取质量或业务验收。

- 实施方案：[`docs/file-processing/PLAN.md`](../../docs/file-processing/PLAN.md)
- 来源/输出/限额契约：[`CONTRACT.md`](CONTRACT.md)
- 样本清单与哈希：[`manifest.json`](manifest.json)
- 人工期望：[`expected.json`](expected.json)
- 现有导入基线：[`BASELINE.md`](BASELINE.md)
- 阶段进度：[`docs/file-processing/PROGRESS.md`](../../docs/file-processing/PROGRESS.md)

## 1. 样本分层

所有样本都是**合成工程样本**，只用于证明解析、边界和失效传播；不代表真实招标材料的字段准确率。真实语料仍只有 `ccgp_2026-10-06/` 的 10 份公开招标文件（5 DOCX + 5 PDF），全部属于输入侧，且没有任何真实 XLSX、CSV、TSV 或招标包 ZIP。

| 样本 | 类型 | 分类 | 用途 |
| --- | --- | --- | --- |
| `clean.xlsx` | XLSX | 固定处理 | 多工作表、隐藏工作表、隐藏行列、合并单元格、公式无缓存、前导零、长编号、金额、日期、空表 |
| `gb18030.csv` | CSV | 固定处理 | GB18030、分号分隔、引号内换行、前导零、长编号、金额 |
| `quote.tsv` | TSV | 固定处理 | Tab 分隔、前导零、长编号、日期、金额 |
| `safe.zip` | ZIP | 固定处理 | 安全清单与受限展开：DOCX/XLSX/CSV/TXT 子文件 |
| `store-docx.zip` | ZIP | 固定处理 | 以 Store 方式收入的 DOCX 招标包；`.zip` 扩展名优先，不得误判为 DOCX |
| `not_docx.docx` | 伪 DOCX | 固定诊断 | 扩展名为 .docx 但中央目录没有 `word/document.xml`，必须明确失败 |
| `zip-traversal.zip` 等 | 恶意 ZIP | 固定诊断 | 路径穿越、绝对路径、盘符、反斜杠、符号链接、重复路径、嵌套包、高压缩比、条目数、单条超限 |
| `ccgp_2026-10-06/` | DOCX/PDF | 固定处理 | 现有真实导入基线；仅 10 份输入侧文件 |

### 脚本适用分类

- **固定工具适用**：上表所有合法样本与现有 PDF/DOCX。按方案 §7.2，确定性固定解析器对新上传文件直接入库，不额外要求人工采用。
- **脚本候选（合成）**：非结构化、跨表自由文本、固定规则无法稳定映射的复杂附件。本目录**没有**为演示而伪造脚本必选样本；合成样本最多证明候选校验边界，不计入 F2 门槛。
- **明确不支持**：`.xls`、`.doc`、`.wps`、`.rtf`、`.et`、加密文件、宏文件、嵌套压缩包。返回明确原因，不假定已转换。

## 2. F2 启动关口（冻结）

F2 必须同时满足：

1. 至少 **3 份真实复杂材料**，覆盖至少 **2 个真实项目**，人工确认固定工具无法完成关键提取，且受限脚本有可审查收益。
2. Linux 容器执行后端在目标环境通过实际启动、只读挂载、无网络、资源限制、终止与产物收集验证。

当前：真实复杂材料 **0**；容器后端 **0**（PATH 无 `docker`，WSL 未安装，无 `podman`）。因此 **F2 未启动，脚本工具不注册，不回落宿主执行**。F1 的固定处理独立推进。

## 3. 复现

生成样本（固定时间戳，重复执行字节一致）：

```bash
GOPROXY=https://goproxy.cn go run ./eval/file-processing/gen -out eval/file-processing/fixtures
```

运行现有导入基线（读取真实语料，输出 JSON 摘要）：

```bash
go run ./eval/file-processing/baseline -dir ccgp_2026-10-06
```

单元/集成测试：

```bash
go test ./ingest ./filework ./db
```

真实 PostgreSQL 与 race 的完整门禁命令见仓库根 `AGENTS.md`。未设置 `AUTOBID_TEST_DATABASE_URL` 时数据库测试会跳过，不能据此宣称 F1 验收通过。
