# F1 原件与固定处理 — 验收记录

日期：2026-10-31。等级：**E（工程实现）**。本记录只证明工程实现与合成样本边界，不证明真实招标材料的字段准确率；V/P 未启动。

本页保留 F1 交付时的验收快照。后续已补齐固定解析的页面导入/修订采用与独立升级恢复演练，最新结果见 [`F3A_ACCEPTANCE.md`](F3A_ACCEPTANCE.md)；导出运行环境阻塞已于 2026-11-01 复验收口（原导出引擎已于 2026-11-03 移除，改为 WPS / Word）。下文“尚未包含”指 F1 当时的范围。

方案：[`PLAN.md`](PLAN.md) §9 F1、§10.1。

## 1. 可执行门禁

| 命令 | 结果 |
| --- | --- |
| `go vet ./...` | 通过 |
| `AUTOBID_TEST_DATABASE_URL`（PostgreSQL 16.15）+ `go test -race ./... -count=1` | 通过；全包 0 失败 |
| `npm --prefix web run test:unit` | 15/15 通过 |
| `npm --prefix web run build` | 通过（`tsc -b` + Vite） |
| `go run ./eval/file-processing/baseline -dir ccgp_2026-10-06` | 10 份真实招标全部解析，3,193 块 / 245 表 / 624 页 |

race 使用 WinLibs GCC 16.2.0（`F:\autobid-toolchain\winlibs\mingw64`，`CGO_ENABLED=1`，`CC=gcc`）。数据库测试未使用 `t.Skip` 掩盖缺失依赖。

## 2. F1 完成标准逐项

| # | 方案要求 | 结果 | 证据 |
| --- | --- | --- | --- |
| 1 | 原件可重读、哈希一致 | **PASS** | `filework/store_test.go`：流式写入、SHA-256、读取校验、篡改检测、跨任务共享；`server/file_processing_test.go`：上传后回读字节一致；`filework.Store.Read` 每次重算哈希 |
| 2 | 解析失败仍可重试 | **PASS** | 伪 DOCX 上传返回 415，响应含 `original_file_id`，原件仍可按任务读取；ZIP 恶意样本同样保留原件 |
| 3 | 旧数据明确缺原件 | **PASS** | `tender_documents.original_file_id` 可空；文档页显示“原件未留存”；旧上传/旧数据不伪造原件 |
| 4 | 格式精度 / 合并 / 隐藏 / 公式 / ZIP 边界用例 | **PASS** | `ingest/spreadsheet_test.go`：前导零、长编号、金额原始值/显示值、日期 ISO、合并、隐藏行列/工作表、公式无缓存、空表；`ingest/archive_test.go`：路径穿越、绝对路径、盘符、反斜杠、符号链接、重复路径、嵌套包、压缩比、条目数、单条上限、累计上限 |
| 5 | 迁移与关联原件后既有检索证据状态不变 | **PASS** | `db/file_processing_integration_test.go`：关联真实 `task_file_objects`、更新 `parse_revision`/`page_count` 不使 evidence 失效；仅内容列变化才 stale；`Migrate` 重复执行幂等 |
| 6 | 重新解析修订的要求失效粒度 | **PASS** | `TestAdoptTenderParseRevisionDiffAndGating`：无实质变化不推进清单/来源；变化分块的要求 `needs_revision`；删除分块的 `source_chunk_id` 置 NULL；快照不可修改并保存被替换前正文 |
| 7 | “待重新提取要求”阻断确认与导出 | **PASS** | 同上 + `TestAdoptUnlinkedChunkChangeBumpsSource`：仅新增分块、原未关联要求的分块变化都推进 `source_epoch`；`ConfirmRequirements` 拒绝，`ExportReadiness` 返回 `requirements_source_changed`，即使清单状态被其他路径改回 confirmed |
| 8 | 表格切分与单元格溯源贯通 | **PASS** | `ingest/spreadsheet_integration_test.go`：XLSX 入库后 `page_count`、`page_start/page_end` 为 NULL，`bbox_meta.page_is_physical=false`，窗口重复表头；`requirement_items.source_loc` 为 `{kind:"spreadsheet", sheet, range, ...}`；`TestXLSXRequirementSourceHashCoversCellValues` 证明单元格原始值变化会改变 `source_hash` 并要求复核 |
| 9 | 提取与采用并发 | **PASS** | `TestAdoptConcurrentExtractionKeepsPending`：提取读取旧 epoch 后提交的修订不会被这次提取清除待提取状态 |
| 10 | 旧上传兼容 | **PASS** | 全量既有测试通过；`FileStore` 未配置时保持旧上传行为；成功响应结构未破坏 |

## 3. 数据与模块落点

- `db/schema.sql`：
  - `task_file_objects`（租户/任务、内容哈希、包内路径、父文件、格式、处理状态、可用性）；
  - `tender_parse_revisions`（不可变快照，`(document_id, revision_no)` 唯一）；
  - `tender_documents.original_file_id / parse_revision`；
  - `requirement_manifests.source_epoch / extracted_source_epoch`；
  - 检索失效触发器改为 `UPDATE OF` 内容列或 `DELETE`；回填前先移除旧触发器。
- `filework/`：内容寻址存储、流式哈希、原子移动、授权读取、孤立对象清理。
- `ingest/`：`.xlsx/.csv/.tsv/.zip` 扩展名优先路由；ZIP 中央目录识别 DOCX/XLSX；excelize v2.9.1；CSV GB18030/分隔符；表格行窗口（600 token / 40 行）；来源定位。
- `db/tender_revisions.go`：`AdoptTenderParseRevision` 差量更新与失效传播。
- `server/`：旧 `POST /api/tasks/{id}/documents` 保存原件；解析失败返回原件 ID；ZIP 返回 202 清单并登记子文件（`kind=derived`，不自动成为正式文档）。
- `web/`：文档页显示原件留存、表格工作表/区域；要求页显示来源工作表/区域与“原文已变更 · 待重新提取”。

## 4. 明确 BLOCKED / 不在 F1 范围

- **F2 执行服务（BLOCKED）**：本机无 Docker、WSL 未安装、无 Podman；真实复杂样本 0（门槛 ≥3 份、≥2 个项目）。未建 F2 表、未实现 Runner、未注册 Agent 脚本工具，未回落宿主执行。
- **F3 业务闭环（未开始）**：脚本产物预览/人工采用页面、正式采用 HTTP 路由、处理记录页与 SSE 摘要未实现；当前 `AdoptTenderParseRevision` 只有 db 层与测试入口。
- **F4 外部验证（未开始）**：无真实复杂附件、无人工金标准；合成样本只证明工程边界。
- **V/P**：未启动，不因本记录改变 ROADMAP 的 D0~D5 状态。

## 5. 已知限制

1. **旧库升级验证方式**：`Migrate` 在本次共享测试库上执行并通过，重复执行幂等；触发器列行为与本方案新增约束已由集成测试覆盖。未单独保存一份旧 schema 副本做“空库 + 旧库”双跑；若需要更强的迁移证据，应在备份数据库上执行恢复演练。
2. **ZIP 子文件**：只列清单并登记原件/格式/状态，不自动转换为正式招标文档；部分展开失败时，已登记的成功子文件保留，孤立对象由 GC 清理。
3. **Excel 公式**：不重新计算；缓存缺失标为待核对，不把公式结果当作已确认报价。日期按样式规范为 ISO，原始序列号保留在 `cell.raw`。
4. **CSV 分隔符**：对 `.csv` 用前几条记录在逗号/分号/Tab 中择优；极端格式需显式指定或人工复核。
5. **表格窗口**：跨窗口合并单元格按覆盖关系裁剪；前部少切/多切导致 seq 整体偏移时，仍按 seq 差量，偏移部分保守标为待复核（安全但复核量增大）。
6. **默认关闭**：`AUTOBID_FILE_PROCESSING_ENABLED` 默认 false；未启用时不保存原件、ZIP 返回 415，固定解析的 PDF/DOCX 等旧行为不变。
7. **上传入口**：旧 `POST /documents` 仍先整体读入内存再登记原件（`filework.Store.Put` 本身支持流式）；流式 `/files` 入口、异步作业与正式采用 HTTP/页面留给 F3。`AdoptTenderParseRevision` 当前只有 db 层与测试入口。
8. **GC 范围**：启动与每 6 小时清理默认租户下 24 小时前、无登记行引用的对象；多租户遍历留待 Phase 7。

## 6. 二轮检查修正：解析修订的原件哈希校验（2026-10-31）

- `AdoptTenderParseRevision` 关联原件时，在事务内对文件登记行加共享锁，并校验其 `sha256` 必须等于文档的原始字节 `content_hash`；不匹配返回 `ErrInvalidParseRevision`，拒绝发生在快照及分块写入之前。
- 新增 `TestAdoptTenderParseRevisionOriginalHash`，覆盖原件尚未关联与已关联两种情况：同任务内哈希不匹配的文件被拒绝，文档、分块、清单和快照数量保持不变；匹配原件可正常采用新解析结果，原字节哈希保持不变。
- 先在真实 PostgreSQL 上运行该测试复现错误关联成功，再修正并通过 `go test -race ./db -run '^TestAdoptTenderParseRevisionOriginalHash$' -count=1 -v`。
- 修正后 `go vet ./...` 与配置 `AUTOBID_TEST_DATABASE_URL` 的全量 `go test -race ./... -count=1` 均通过。本次未修改前端。
