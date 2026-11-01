# 复杂文件处理：来源、输出与限额契约（F0 冻结）

冻结日期：2026-10-31。状态：**工程契约，E 级；F1 已按此契约实现固定处理部分**。本文件是 [`docs/file-processing/PLAN.md`](../../docs/file-processing/PLAN.md) §4、§5、§8.4 和 §9 F0 的可执行摘要。任何数值和字段变更都要在本文件记录日期与原因；不得用真实评测结果之外的数字冒充实测。

## 1. 来源定位（source locator）

正式内容必须能回到原件。固定处理器和脚本候选统一使用以下最小结构：

```json
{
  "kind": "page",
  "file_id": 123,
  "sha256": "64 位十六进制",
  "page_start": 3,
  "page_end": 4
}
```

```json
{
  "kind": "spreadsheet",
  "file_id": 123,
  "sha256": "64 位十六进制",
  "sheet": "报价清单",
  "range": "A2:F31",
  "row": 3,
  "column": "D"
}
```

规则：

- `kind=page` 只用于具有物理页码的 PDF / DOCX / 扫描件；PDF 页码从 1 开始。
- `kind=spreadsheet` 用于 XLSX / CSV / TSV。Excel 没有稳定物理页码，禁止伪造 `page_start`；`tender_documents.page_count` 对表格类文档为 NULL，`document_chunks.page_start/page_end` 为 NULL，`bbox_meta.page_is_physical=false`。
- CSV / TSV 的 `sheet` 使用文件名（去扩展名）；没有工作表概念时也保留该字段。
- `range` 使用 A1 记法，包含表头行（若有）；`row`、`column` 为可选单元格定位（1 基行号 / 列字母）。
- 旧数据缺原件时显示“原件未留存”，不得从 `raw_text` 重建原件并声称是原件。
- 每个正式要求/候选内容都必须携带 `file_id` 与 `sha256`；无法定位到原件的内容只能作为报告，不得自动采用。

## 2. 固定处理器输出（入库前）

固定解析器的内部 `Parsed`/`ParseMeta` 允许与候选契约不同，但必须保留：

- `schema_version`（当前固定为 `1`）；
- 原件 `file_id`、`sha256`、原文件名、媒体类型；
- 工作表清单：名称、可见性、是否处理、行/列上限、告警；
- 每个表格：工作表、A1 区域、合并区域、原始值、显示值、公式与缓存状态、隐藏行列；
- `warnings`（编码、分隔符、空表、公式无缓存、被截断等）；
- `coverage`（已处理 / 未处理的工作表、行窗口、附件）。

## 3. 候选输出最小契约（脚本 / 复杂处理，F2 起）

```json
{
  "schema_version": "1",
  "source_files": [
    {"file_id": 123, "sha256": "…", "name": "报价清单.xlsx"}
  ],
  "blocks": [
    {"id": "b1", "type": "text", "text": "…", "source": {"kind": "page", "page_start": 2, "page_end": 2}}
  ],
  "tables": [
    {"sheet": "报价清单", "range": "A1:F10", "cells": [{"row": 1, "column": "A", "value": "序号"}], "source": {"kind": "spreadsheet", "sheet": "报价清单", "range": "A1:F10"}}
  ],
  "warnings": [{"code": "formula_cache_missing", "message": "F3 公式没有缓存值", "source": {"kind": "spreadsheet", "sheet": "报价清单", "range": "F3"}}],
  "coverage": {"handled": ["sheet:报价清单"], "unhandled": ["sheet:空表"]}
}
```

约束：

- 每个 block/cell 必须能由可信预检清单核对；脚本自报的覆盖不能作为完整性证明。
- 计算结果标记为派生值并附输入定位和计算方式，不得伪装成原文。
- 输出中的提示指令、HTML/JavaScript、公式前缀（`=`, `+`, `-`, `@`）在预览与 CSV 下载时必须转义或隔离。
- 结构校验失败、哈希不符、来源无法核对或 `coverage` 不完整时，页面明确提示并阻止标记为完整解析。

## 4. 限额（首轮建议值，需真实样本调优）

| 项目 | 限额 | 执行位置 |
| --- | --- | --- |
| 单次上传 | 64 MiB | HTTP `MaxBytesReader` |
| ZIP 条目数 | 200 | 读取中央目录及展开过程中 |
| ZIP 解压总量 | 256 MiB | 逐条累计 |
| ZIP 单条解压 | 64 MiB | 逐条读取 |
| ZIP 压缩比 | 100 | 读取过程中按实际解压字节复核 |
| 嵌套压缩包 | 拒绝 | 扩展名 + 条目检查 |
| XLSX 单元格 | 2,000,000 | 解析器 |
| XLSX 行 / 列 / 工作表 | 200,000 / 512 / 200 | 解析器 |
| XLSX 解压 / XML 大小 | 256 MiB / 32 MiB | excelize `UnzipSizeLimit` / `UnzipXMLSizeLimit` |
| 表格行窗口 | 600 token 估算 / 40 数据行 | `ingest` 切分；表头重复 |
| 脚本大小 | 64 KiB | F2 |
| 单次执行 | 120 秒 / 1 CPU / 512 MiB / 32 进程 | F2 |
| 输出 | 32 MiB / 100 文件；临时可写区 128 MiB | F2 |
| 日志 | 1 MiB / 次；模型摘要 8 KiB | F2 |
| 并发 | 每任务 1，全局 2 | F2 |
| 自动修复 | 初始 1 次 + 最多 2 次修正；任务累计 10 分钟 / 10 次提交 | F2 |

表格窗口的 token 口径沿用 `ingest.EstimateTokens`（CJK 字符约 1 token/字，其余约 4 字符/token）。F0 样本的单行约 20–80 字符，600 token 约容纳 25–40 行；首轮取 40 行上限，二者取先到者。

## 5. 明确不支持（首版）

- `.xls`、`.doc`、`.wps`、`.rtf`、`.et` 等旧格式：返回明确不支持原因，不假定已转换。
- 加密文件（加密 XLSX / DOCX / PDF / ZIP）：明确失败，不尝试破解。
- 宏文件（`.xlsm`、`.docm`）：不执行宏，首版不支持。
- 嵌套压缩包（ZIP 内再含 ZIP/RAR/7Z/TAR/GZ 等）：拒绝。
- 交互式 Shell、任意宿主命令、在线安装依赖、浏览器自动化、自动下载外部材料、修改源码、数据库直连、自动签章提交。

## 6. 环境探测（2026-10-31）

| 依赖 | 状态 | 证据 |
| --- | --- | --- |
| PostgreSQL 16.15 | 可用 | `psql --version`；`AUTOBID_TEST_DATABASE_URL` 连接成功 |
| Go 1.24 + WinLibs GCC | 可用 | `go vet`、真实 PostgreSQL `go test -race` 可执行 |
| Docker | **不可用** | PATH 上无 `docker`；`docker version` 返回 command not found |
| WSL | **未安装** | `wsl -l -v` 返回“未安装适用于 Linux 的 Windows 子系统” |
| Podman | **不可用** | PATH 上无 `podman` |

结论：**F2 的 Linux 容器执行后端未验证，F2 启动关口未通过**。按方案要求，不回落为宿主 Python/Bash，不注册 Agent 脚本工具。F1 的固定处理不受影响。

## 7. F2 启动关口（F0 冻结）

F2 必须在同时满足以下两项后才能建表/编码：

1. 至少 **3 份真实复杂材料、覆盖至少 2 个真实项目**，经人工确认固定工具无法完成其关键提取，且受限脚本有明确可审查的收益；不允许用本目录的合成样本凑数。
2. 目标环境中的 Linux 容器后端通过实际启动、挂载、只读、无网络、资源限制、终止与产物收集验证。

当前计数：真实复杂材料 **0**；容器后端 **0**。因此 F2/F3 的脚本路径保持 `[ ]`，F1 独立推进。
