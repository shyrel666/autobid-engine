# 复杂文件处理实施进度

方案：[`PLAN.md`](PLAN.md)。等级说明沿用 ROADMAP：E = 工程实现；V = 外部验证；P = 生产启用。本页只登记事实，不改变 D0~D5 的状态。

## F0 基线与样本 — `[x] E`（2026-10-31）

- 样本清单与哈希：[`eval/file-processing/manifest.json`](../../eval/file-processing/manifest.json)
- 人工期望：[`eval/file-processing/expected.json`](../../eval/file-processing/expected.json)
- 来源/输出/限额契约：[`eval/file-processing/CONTRACT.md`](../../eval/file-processing/CONTRACT.md)
- 现有导入基线：[`eval/file-processing/BASELINE.md`](../../eval/file-processing/BASELINE.md)
- 样本生成器：[`eval/file-processing/gen/main.go`](../../eval/file-processing/gen/main.go)
- 基线工具：[`eval/file-processing/baseline/main.go`](../../eval/file-processing/baseline/main.go)
- 固定工具 / 脚本适用分类、F2 门槛与容器探测结论见 `manifest.json`、`CONTRACT.md` 与 `README.md`。
- 结论：F2 启动关口未通过（真实复杂材料 0，容器后端不可用）；不回落宿主执行。

## F1 原件与固定处理 — `[x] E`（2026-10-31）

验收记录（逐项 PASS/BLOCKED、命令与限制）：[`F1_ACCEPTANCE.md`](F1_ACCEPTANCE.md)。

| 子项 | 状态 | 证据 |
| --- | --- | --- |
| F1.a 触发器按列修正、原件表、解析修订快照、epoch 列 | [x] | `db/schema.sql`；`db/file_processing_integration_test.go` 的触发器列行为与 Migrate 幂等 |
| F1.b 内容寻址原件存储与旧上传兼容 | [x] | `filework/`；`server/file_processing_test.go`；解析失败返回原件 ID；文本登记 `text_snapshot` |
| F1.c 格式路由与 XLSX/CSV/TSV/ZIP 固定解析 | [x] | `ingest/spreadsheet.go`、`ingest/archive.go`、`ingest/*_test.go`；excelize v2.9.1 |
| F1.d 表格切分与单元格溯源贯通 | [x] | `ingest/spreadsheet.go` 行窗口；`db/requirements.go` source locator；`ingest/spreadsheet_integration_test.go` |
| F1.e AdoptTenderParseRevision 与要求 epoch 门禁 | [x] | `db/tender_revisions.go`；`db/file_processing_integration_test.go`；`ExportReadiness` 新增独立阻断 |
| F1.f 验收（真实 PostgreSQL、race、前端、文档） | [x] | `go vet`、真实 PostgreSQL `go test -race ./...`、`npm run test:unit` 15/15、`npm run build` 均通过 |

### F1 关键行为

- 检索失效触发器只在 `raw_text/title/tender_no/lot_no/doc_role/published_at`（文档）或 `content/section_code/section_title/page_start/page_end/tables_json`（分块）变化或删除时触发；关联原件、`parse_revision`、`page_count`、`parse_meta`、`token_estimate`、`bbox_meta` 不使证据失效。
- 表格类文档：`tender_documents.page_count` 为 NULL，`document_chunks.page_start/page_end` 为 NULL，`bbox_meta.page_is_physical=false`；分块按工作表 → 行窗口切分，重复表头，窗口 600 token / 40 数据行。
- `requirement_items.source_loc` 对表格来源写入 `{kind:"spreadsheet", file_id, sha256, sheet, range}`；单元格原始值变化会改变 `source_hash` 并使要求待复核。
- 新文档 / 采用修订推进 `requirement_manifests.source_epoch`；`source_epoch > extracted_source_epoch` 时确认拒绝、导出阻断，即使清单状态被改回 confirmed。
- `AdoptTenderParseRevision`：旧正文/分块先写不可变快照；按 seq 差量 UPDATE/INSERT/DELETE；保持文档 ID 与分块 ID；`content_hash` 保持原始上传字节哈希。
- ZIP 只列清单并登记子文件（父文件、包内路径、格式、处理状态），不自动成为正式文档；路径穿越/盘符/反斜杠/链接/重复路径/嵌套/高压缩比/条目与字节上限均拒绝。
- 默认开关 `AUTOBID_FILE_PROCESSING_ENABLED=false`；未启用时保持旧上传行为。

### F1 尚未包含

- 正式采用 HTTP 路由、处理记录/预览页面、SSE 状态摘要（F3）。
- 脚本执行与作业表（F2，关口未通过）。
- 真实复杂材料外部验证与保留集（F4/V）。

## F2 执行服务 — 未启动（关口未通过）

- 容器后端：docker 不在 PATH；WSL 未安装；无 podman。
- 真实复杂材料：0；门槛为至少 3 份、覆盖至少 2 个真实项目。
- 未建 F2 表、未编码 Runner、未注册 Agent 脚本工具。

## F3a 固定解析业务闭环 — 已实现，专项通过，全量 E 待收口

详见 [`F3A_ACCEPTANCE.md`](F3A_ACCEPTANCE.md)。ZIP 子文件页面导入、持久化修订预览/采用、统一事务、幂等与冲突检查、浏览器 3/3 及独立升级恢复演练已完成。真实 PostgreSQL 专项测试通过；全量竞态测试仅剩两个既有 LibreOffice 导出用例因本机运行环境失败，未勾选完整 E。

## F3b / F4 — 未开始

F3b 的脚本闭环依赖 F2；脚本/Agent 工具未交付。固定处理的流式上传、异步作业与必需材料门禁已拆出到 F3a+。F4 需要真实保留集与外部人工处理对照。

## F3a+ 固定处理增强 — 已实现，专项通过，完整 E 待 LibreOffice 收口

见 [`F3A_PLUS_ACCEPTANCE.md`](F3A_PLUS_ACCEPTANCE.md)。已交付可在上传前登记的必需材料清单及导出事务门禁、流式原件上传、固定解析后台队列、租约/次数/截止时间、恢复/取消/显式重试，以及页面闭环。真实 PostgreSQL 竞态专项、浏览器 4/4、前端单测/构建、独立旧库升级恢复演练通过。全量验收仍保留两个 LibreOffice 导出失败，不勾选完整 E；没有启用 F2 或使用合成数据宣称 V/P。
