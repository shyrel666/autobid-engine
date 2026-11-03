# AutoBid 复杂文件处理与受限脚本执行实施方案

编写日期：2026-10-31。状态：**实施设计，尚未开发或验收**。2026-10-31 依据源码审查修订：解析修订改为“原地按 seq 差量更新 + 不可变快照”模型，补充检索触发器、租户外键、格式路由、表格切分和沙箱前置关口等约束。

本方案依据当前工作区源码（包含尚未提交的改动）、`ROADMAP.md`、`CLAUDE.md` 和 `../artex-reference` 编写。它落实本次讨论确认的方向：先补齐复杂文件处理与成熟工具调用，再增加隔离的自主脚本能力。所有阶段复选框均表示待实施，不代表现有功能。

## 1. 目标与范围

让使用者上传复杂招标材料后，系统能提取表格、整理附件、辅助核对，并在固定工具不能处理时通过受限 Python 脚本生成可审查的候选结果。用户操作仍是“上传、查看、核对、采用”，无需学习 Shell。

实施优先级：**原件留存与溯源 → 固定文件工具 → 隔离执行 → Agent 自主脚本 → 真实场景验证**。这项工作服务于 D1 输入完整性和 D4 文件核对，不取代 D0 评测、D2 证据可采性、D3 句子级落证或 D5 回测。

首版支持：

- 现有 PDF、DOCX、图片、文本流程的原件留存、明确失败原因和处理记录。
- XLSX、CSV、TSV 的工作表/表头/单元格提取；ZIP 招标包的清单和受限展开。
- 固定工具无法解决的表格清洗、字段映射、型号和数量比对，允许 Agent 编写 Python 脚本产出 JSON/CSV/纯文本候选结果。
- 任务级输入、执行记录、产物预览、人工采用、失效传播。

首版不包括：交互式 Shell、任意宿主命令、在线安装依赖、浏览器自动化、自动下载外部材料、修改源码、数据库直连、自动签章提交。XLS、DOC、WPS、RTF、加密文件、宏文件先返回明确的不支持原因；后续依据真实需求增加专用转换适配器，不假定已支持。

## 2. 已核实的代码基线

| 现有落点 | 实际能力与限制 | 本方案如何衔接 |
| --- | --- | --- |
| `ingest/parse.go`：`DetectFormat`、`Parse` | 支持文本/Markdown/DOCX/PDF/图片，没有独立 XLSX/CSV/ZIP 解析器；CSV 经 `text/*` 或无 NUL 字节兜底被当普通文本；XLSX 因含 NUL 字节返回不支持。`.zip` 不在扩展名分支，掉入内容嗅探：`PK` 开头且字节中含 `word/document.xml` 即判为 DOCX，以 Store 方式收入 .docx 的招标包会被误判 | 显式格式路由：`.xlsx/.csv/.tsv/.zip` 先按扩展名分派，DOCX 嗅探改为读取 ZIP 中央目录条目名；避免将结构化表格误当无来源的文本 |
| `ingest/table.go`：`Table`、`Cell` | 表格已有合并单元格和页码；没有工作表、A1 坐标、公式状态 | 增加可选来源定位，保持原 PDF/DOCX JSON 兼容 |
| `server/knowledge.go`：`readMultipartFile`、`uploadDocument` | 请求上限 64 MiB；整文件读入内存，然后同步解析。`withTask` 不替换 `*http.Request`，net/http 结束请求时会自动清理 multipart 临时文件 | 保留旧接口；新入口改为流式落盘并边写边算哈希，增加异步处理入口；若新入口包装或替换 Request，需自行 `MultipartForm.RemoveAll()` |
| `ingest/store.go`：`Ingest`；`db/documents.go` | 保存文件名、MIME、原字节哈希、解析文本/分块；此链路没有持久保存上传文件字节 | 先建设任务原件存储；不能拿哈希冒充可重新解析的原件 |
| `evidence/store.go` | D2 企业证据已有内容寻址原件存储及读取哈希校验 | 借鉴原子写入和校验做法；不得把招标文件塞入企业证据表 |
| `ingest/ocr.go`、`export/render.go`、`export/office.go` | 已调用 Tesseract、mmdc、LibreOffice/Python；用途及参数由后端控制 | 固定工具可继续复用；不把现有 Python 导出脚本视为任意代码执行器 |
| `export/proc.go`、`export/proc_windows.go` | 有超时、进程树清理、Windows 隐藏窗口 | 这些只管理进程生命周期，不提供文件、网络、凭据或资源隔离 |
| `export/runner.go`、`db/exports.go` | 导出任务有队列、租约、递增围栏令牌、心跳和恢复 | 借鉴设计，新建处理任务表；不能复用 `export_jobs` 存任意脚本任务 |
| `agent/worker.go`、`server/task_llm.go`、`bidtools/bidtools.go` | Worker 由业务工具与延迟工具装配；有 Skill 解锁，无通用 Bash | 经 `Kit.WithProcessing` 注入服务；新增领域 Skill 和延迟工具 |
| `server/proposal.go`：`llmSectionWriter.WriteSection` | 当前源码已允许章节写作循环调用 `SectionWriterTools()`，最多 6 轮，再有收尾轮 | 文档中“单次 LLM 调用”描述已落后于此处代码；首版不向章节写作器加入脚本工具 |
| `internal/harness/tools.go`、`internal/tool/deferred.go`、`guard/guard.go` | 直调/包装工具已有解锁、收尾门禁与真实目标解包 | 新工具覆盖直调和 `ExecuteExtraTool` 两条入口，不另造绕过入口 |
| `server/events.go` | SSE Hub 是非持久通知，可能丢事件 | 处理状态与审计落 PostgreSQL；页面重连通过 GET 补全 |
| `server/engine.go` | 暂停/删除会取消 Agent Context | 新 Runner 要显式接入任务生命周期，不能假定取消 Agent 就取消独立作业 |
| `db/db.go`：`Migrate` | 嵌入 `db/schema.sql`，校验和及事务锁控制幂等迁移 | 所有新 DDL 放这里，不放一个不会被执行的 migrations 文件 |
| `db/schema.sql`：`retrieval_source_changed`、`requirement_source_changed` 触发器 | 检索失效触发器为 `AFTER UPDATE OR DELETE`，**不区分列**，任何对 `tender_documents`/`document_chunks` 的 UPDATE 都会把该文档全部检索证据标为 `stale`；要求失效触发器只在分块内容/标题/页码/`tables_json` 变化或删除时把引用要求置 `needs_revision` 并 bump 清单 epoch | 检索触发器改为按内容列触发，元数据列更新不得使证据失效；解析修订复用要求触发器做精确失效（见 §4.3） |
| `db/scoring.go`：`chunkKey`；`db/requirements.go`：提取与孤立处理 | 要求编码由 `D{docID}-{seq}` 派生；重新提取按编码比对哈希与字段，未再产出的编码置 `needs_revision` | 解析修订必须保持文档 ID 不变，否则全部要求编码改变、确认与证据绑定失效 |
| `db/schema.sql`：`tasks`、`tender_documents` | `tasks` 无 `tenant_id`，现有 Phase 5 表均为 `tenant_id` + `REFERENCES tasks(id)`；`tender_documents` 无 `(tenant_id,task_id,id)` 唯一约束 | 任务到租户的绑定在 Phase 7 前只能在服务层校验；新表间复合外键需先补被引用表唯一约束 |

参考项目的差别：ARTEX 在 `agent/worker.go` 调用 `defaultToolsExcept(...)` 引入 norma 的 Bash/文件工具，并设置工作目录、代理环境；`server/assembly.go` 按开关注入交互式 Shell。本项目保留自身运行时和业务工具结构，不需要为本功能整体切换到 norma。

## 3. 核心设计

### 3.1 数据流

```text
上传原件 → 校验/保存原件 → 文件预检
                          ↓
                固定处理器优先执行
          ↓ 新文件、确定性解析成功              ↓ 固定处理器无法完成且脚本能力已启用
   直接入库（同现有 PDF/DOCX，             Agent 生成 Python → 隔离执行
   附原件与来源定位）                              ↓
          │                              产物收集 + 格式与来源校验
          │                                        ↓
          │                              预览候选结果、显示缺口、影响范围
          │                                        ↓ 用户采用（脚本产物 / 已有文档重新解析修订）
          │                              业务服务再次校验版本/权限/来源
          ↓                                        ↓
                正式文档 → 现有要求提取 → 待复核要求清单
```

脚本可以提取、转换和计算，不能批准证据或承诺，不能直接写 `assets`、要求、章节或导出任务。退出码为 0 只代表进程结束成功，不代表信息正确、材料可采或业务批准。

### 3.2 模块边界（以下新文件均为拟新增）

| 模块 | 职责 |
| --- | --- |
| `filework/store.go` | 任务原件、不可变脚本和中间产物的内容寻址存储；原子落盘、哈希校验、授权解析文件 ID |
| `filework/service.go`、`runner.go` | 提交、预算预留、作业执行、取消、租约心跳、重启协调、产物收集 |
| `filework/validator.go`、`adopt.go` | 输出契约、来源检查、候选校验，以及调用业务层采用；不在执行容器内写业务数据 |
| `sandbox/executor.go`、`docker.go` | 隔离执行接口及首个 Linux 容器后端；启动/等待/终止/查询能力 |
| `ingest/spreadsheet.go`、`archive.go` | 固定表格解析与 ZIP 校验/清单；复用已有解析模型 |
| `db/file_processing.go`、`db/file_adoptions.go` | 文件登记、作业状态机、租约、原子预算、候选采用与审计 |
| `bidtools/file_processing.go`、`bidtools/skills.go` | `file-processing` Skill 和工具适配 |
| `server/file_processing.go` | 上传、预检、作业查询、取消、预览、采用与能力状态 API |
| `web/src/features/proposal/documents/processing/` | 在现有文档页加入处理记录、预览与采用，不新增终端产品页 |

`sandbox` 不依赖业务数据库；`filework` 通过 `db/` 访问持久状态；Agent 和 HTTP 共用 `filework.Service`，不能各自实现一套执行逻辑。新增能力字段同时注入 `APIConfig`、`LLMServiceConfig` 和 `bidtools.Kit`，由 `cmd/server/main.go` 统一组装与关闭。

## 4. 原件、产物与数据库

### 4.1 数据域归属

- 招标原件与正式解析结果归域 A，仍由 `tender_documents/document_chunks` 表示正式文档；任务原件登记不写入 `assets`。
- 处理作业和采用审计属于过程记录，归域 B；脚本输出默认只是中间产物。
- 要求、章节及其审批归域 C，继续走既有状态机。
- 企业知识库和导出附件归域 D；企业证明原件继续使用现有 D2 模型。
- 文件对象的物理存储可以复用技术，但业务归属和权限必须显式登记。首版只接入任务招标原件；读取企业证据或知识库原件需另做授权适配，不能自动开放整个证据目录。

### 4.2 拟增表与约束

所有新表含 `tenant_id`、`task_id`、创建/更新操作人与时间；需要复核的记录含复核人和时间。Phase 7 前租户由服务端固定，禁止使用请求中的租户声明作为授权依据。`tasks` 表没有 `tenant_id`，任务到租户的绑定在 Phase 7 前只在服务层校验（与现有 Phase 5 表一致，新表引用任务仍用 `REFERENCES tasks(id)`）；复合外键 `(tenant_id,task_id,id)` 只覆盖新表之间及新表到 `tender_documents` 的引用，方案不宣称数据库层已强制任务归属租户。

| 表 | 关键字段与约束 |
| --- | --- |
| `task_file_objects` | `id, tenant_id, task_id, kind(original/script/derived/text_snapshot), sha256, byte_size, relative_path, original_name, media_type, source_job_id, parent_file_id, availability`；唯一键含租户和任务，复合引用同任务；原件/脚本内容不可修改；`relative_path` 由服务端生成，不接受外部输入 |
| `file_processing_jobs` | `id, operation, request_key, request_hash, params, script_file_id, parent_job_id, policy_version, runtime_digest, status, version, attempts, max_attempts, deadline_at, cancel_requested_at, error_code`；另存 `origin(http/agent), exploration_id, intent_id, session_id, tool_use_id` 用于取消与归因；唯一 `(tenant_id,task_id,request_key)`；同 key 不同输入返回 409；`parent_job_id` 同任务复合引用，修正次数按父链长度计 |
| `file_processing_inputs` | `job_id, file_id, source_sha256, source_document_id, source_version`；同任务外键；输入快照提交后不可修改 |
| `file_processing_attempts` | `job_id, attempt_no, lease_owner, lease_token, lease_until, executor_id, started_at, finished_at, exit_code, resource_usage, log_file_id`；每次尝试独立记录，令牌单调增长 |
| `file_processing_outputs` | `job_id, attempt_no, file_id, logical_name, output_schema_version, validation_status, provenance`；只允许当前成功尝试发布可采用产物 |
| `file_processing_budgets` | 每任务/策略的 `reserved_ms, consumed_ms, job_count, version` 等；固定处理与脚本执行分别记账；并发提交事务预留，重试记账，不能因 Agent 会话重建而重置 |
| `file_processing_adoptions` | `output_id, target_kind, target_id, source_hash, expected_version, status, reviewed_by, adopted_by`；重复采用同目标幂等；结果保留来源及原件定位 |
| `file_processing_events` | `job_id, attempt_no, event_type, actor, details, created_at`；重要状态迁移持久追加，不以 SSE 或内存 Guard 审计替代 |
| `tender_parse_revisions` | 域 A 的正式解析修订快照：`document_id, revision_no, original_file_id, source_job_id, parser_version, raw_text, chunks_snapshot(JSONB), parse_meta, adopted_by, adopted_at`；`(document_id, revision_no)` 唯一，写入后不可修改；保存的是**被替换前**的正文与分块，当前正文仍在 `tender_documents/document_chunks`（见 §4.3） |

表名与字段是实施契约草案，编码时可调整命名，但不得删去不可变输入、任务隔离、预算、取消、令牌与采用版本检查。迁移须先补齐被引用表的 `(tenant_id,task_id,id)` 唯一约束，禁止只凭全局文件 ID 关联。

DDL 随阶段加入：F1 只加 `task_file_objects`、`tender_parse_revisions` 及 `tender_documents` 新列；固定处理需要作业记录时再加 `file_processing_jobs/inputs/outputs/events/adoptions`；`file_processing_attempts`、`file_processing_budgets` 等执行相关表随 F2 通过关口后加入，避免迁移中出现未使用的表。

### 4.3 存储与旧数据兼容

- 新目录建议 `AUTOBID_FILEWORK_DIR`，默认 `data/filework`；对象路径由服务端生成，脚本只看到映射后的 `/inputs/f001` 等路径，不能指定宿主路径。物理对象按租户内容寻址（同租户跨任务可共享同一字节），`task_file_objects` 行按任务登记；`DeleteTask` 级联删除登记行后，物理对象由 GC 按跨任务引用计数回收。
- 新上传入口流式写入临时文件并同时计算 SHA-256（不先整文件读入内存），校验通过后原子移动到内容寻址路径。
- 在现有文档上新增可空 `original_file_id` 与 `parse_revision`（INT，默认 0，乐观锁）；保持 `content_hash` 为原始上传字节哈希的语义。转换结果另存自己的哈希，不覆盖原件哈希。
- **检索失效触发器先行修正**：`trg_tender_retrieval_stale`、`trg_chunks_retrieval_stale` 改为 `AFTER UPDATE OF <内容列> OR DELETE`（文档：`raw_text, title, tender_no, lot_no, doc_role, published_at` 等实质列；分块：`content, section_code, section_title, page_start, page_end, tables_json`）。该修正须与新列在同一次迁移中完成且排在任何回填 UPDATE 之前，否则关联原件或写入 `parse_revision` 会把该文档（回填时则是全部租户）的检索证据批量标为 `stale`。
- 现有文件去重键含原件哈希，因此重新解析同一原件不能简单再次调用 `InsertTenderDocument` 并期待分块更新。采用**原地按 seq 差量更新 + 不可变快照**模型，新增 `db.AdoptTenderParseRevision(ctx, tx, docID, expectedRevision, …)`，在一个事务内：
  1. `SELECT … FOR UPDATE` 锁定文档并核对 `parse_revision = expectedRevision`，不符返回 409；同时锁定该任务的 `requirement_manifests` 行（不存在则创建），与要求提取串行化；
  2. 把当前 `raw_text`、分块全集（含 ID、seq、内容、页码、`tables_json`、`bbox_meta`）写入 `tender_parse_revisions` 快照；
  3. 与新解析结果按 seq 比对：同 seq 内容不同则 UPDATE（保持分块 ID），新增 seq 则 INSERT，消失的 seq 则 DELETE；内容完全相同的分块不写；
  4. 更新 `raw_text/parse_meta/page_count`，`parse_revision + 1`，写采用记录；
  5. 只要第 3 步有任何 INSERT/UPDATE/DELETE（实质变化），就调用 `requirement_manifest_bump`（已确认清单变为 `stale`、已批准或待批准大纲变为 `superseded`），并递增 `requirement_manifests.source_epoch`。没有实质变化的修订只留快照，不推进。
- 该模型保持文档 ID 与 `(doc_id, seq)` 唯一键不变，**不需要修改任何分块读取路径**，正文检索不会同时召回新旧两版。要求失效分两层：
  - **已关联要求的精确失效**：复用 `requirement_source_changed()`，内容真正变化或被删除的分块使引用要求置 `needs_revision`，被删除分块的 `source_chunk_id` 由外键置 NULL。
  - **完整性失效（触发器覆盖不到）**：现有触发器不处理新增分块（例如重新解析补出遗漏的强制条款），也不处理没有关联要求的分块变化；而 `ConfirmRequirements` 只检查 `needs_revision` 条目，单靠 bump 用户仍可不重新提取就重新确认。为此 `requirement_manifests` 增加 `source_epoch`、`extracted_source_epoch`（BIGINT，默认 0），`source_epoch > extracted_source_epoch` 即为“待重新提取要求”。要求提取事务锁定清单行，读取分块前取 `source_epoch`，提交时把它写入 `extracted_source_epoch`（不能写提交时的最新值，避免提取过程中并发采用的修订被误清）；`ConfirmRequirements` 在待重新提取时拒绝确认（包装 `ErrRequirementReview`，沿用现有 422 `requirements_need_review` 映射，并在消息中说明原因）；`ExportReadiness` 增加独立阻断项“招标原文已变更，需重新提取并复核要求”，即使清单状态被其他路径改回 `confirmed` 也照样阻断。采用页面与要求页显示该状态和进入提取的入口。
- 新上传正式文档（`InsertTenderDocument` 返回 `created=true`）在清单已确认后同样存在“新增原文未进入清单”的缺口，属于现有问题。建议在同一入库事务内复用 `source_epoch` 推进，F1 实施时需回归 `eval/bid/accept` 中“确认后上传更正文件”的 D1 流程，确认行为变化符合预期再启用。
- 已知限制：新解析在前部多切或少切段落时，后续 seq 整体偏移，偏移部分表现为内容变化/编码孤立并被保守标为待复核（安全但复核量增大）。采用页面须预先显示“本次修订影响 N 个分块、M 条要求”。彻底解决需要基于内容锚点的要求编码，不在本方案范围。
- 不选的方案：保留旧分块并增加修订过滤（须改约 7 处分块读取 SQL、迁移唯一约束和回填，且现有要求触发器不触发，需另写失效逻辑）；新建文档行取代旧行（要求编码 `D{docID}-{seq}` 全部改变，确认与证据绑定全部失效，还需在 D1 后发覆盖中排除旧行）。如以后需要按旧分块 ID 直接查询历史正文，可由快照表离线迁移到前者。
- 分块读取点清单（用于单元格定位等字段扩展时逐一核对）：`db/documents.go` 的 `docCols` 计数子查询、`DocumentOutline`、`TaskChunks`、`ChunkWindow`；`db/requirements.go` 的 `chunkSourceSQL`（单条校验与全量提取）；`retrieval/evidence.go` 可访问性校验；间接调用方 `retrieval/sources.go`、`bidtools/documents.go`、`server/http.go` 文档导出、`db/report.go`、`db/scoring.go`、`eval/bid/accept`。
- 旧数据没有原件时显示“原件未留存”；只有用户重新上传且哈希匹配才关联（仅更新 `original_file_id`，因触发器已按列修正，不使证据失效）。不能从 `raw_text` 重建一个文件并声称是原件。文本输入登记为 `text_snapshot` 类型对象。
- 新上传即使解析失败，也保留原件及失败记录，便于重试。对旧同步接口维持成功响应结构，失败响应可增加原件 ID；新复杂处理入口采用异步 202。
- 先写临时对象、计算哈希、原子移动，再在事务登记；失败产生的孤立对象由延迟 GC 清理。GC 只清理自身目录、无有效引用且超过保留期的对象，不删仍被正式文档/采用记录引用的文件。

## 5. 固定文件处理器优先

### 5.1 表格

XLSX 解析器先通过小样本选型并锁定依赖，不在本方案虚构已安装的库。输出必须同时保留：工作表名称、可见性、A1 单元格/区域、合并区域、原始值、显示值、公式及缓存状态。不得默默忽略隐藏行列/工作表，也不得自动执行宏、外部链接或重新计算不可信公式。

CSV/TSV 处理保留编码、分隔符、行列位置与原始文本；明确保护型号前导零、长编号、日期和金额精度。金额核对用十进制定点语义，不以浮点近似写入报价。公式缓存缺失或无法证明新鲜时标为待核对，不能作为已确认报价依据。

扩展 `Table`/`Cell` 或增加兼容的 `source_locator`，结构如 `{file_id, sha256, sheet, range, row, column}`。Excel 不具有稳定物理页码，禁止伪造 `page_start`；PDF/DOCX 原页码字段继续保留。同步更新 `sectionJSON`、文档工具返回和前端来源展示，使单元格定位不会在切分入库时丢失。

特别注意：当前 `db.InsertTenderDocument` 会把缺省页码提升为至少 1。实施时须调整该逻辑及相关 schema/引用渲染，明确区分 `source_locator.kind=page` 与 `spreadsheet/text`。旧字段如因兼容暂留占位值，必须标记 `page_is_physical=false` 且不得对用户呈现为“第 1 页”，更不能用它做证明材料页码核验。`Parsed.finish()` 同样把 `PageCount` 强制为至少 1，表格类文档的 `tender_documents.page_count` 应为 NULL，不写占位页数。

单元格定位须贯通到下游：`requirement_items.source_loc` 在来源为表格时写入 `{kind:"spreadsheet", file_id, sheet, range}`，`source_hash` 覆盖单元格原值；`compliance` 的页码核验遇到非物理页定位时改按单元格核对或明确跳过并标注，不得把占位页码当核验通过。

表格切分入库：现有 `Split/SplitLines` 按条款标题切分，不适用于清单/报价表。表格类文档按“工作表 → 行窗口”切分：每个分块含连续若干行并重复表头，按 token 上限（建议沿用现有分块估算口径，具体值由 F0 样本确定）截断，分块标题为 `工作表名 行 a–b`；`tables_json` 只保存本窗口内的行。要求提取对清单类表格不逐行生成要求，按 F0 人工期望确定哪些表（评分表、资格审查表、技术参数表）进入既有规则。

### 5.2 ZIP 与扫描件

- ZIP 先列清单再展开；拒绝绝对路径、盘符、路径穿越、链接、重复规范化路径；限制条目数、展开字节和压缩比。限制要在读取过程中执行，不能只相信 ZIP 头部；首版拒绝嵌套压缩包。格式路由先按扩展名识别 `.zip`，DOCX 判定改为检查中央目录中存在 `word/document.xml` 条目，防止以 Store 方式收入 .docx 的招标包被当作 DOCX 解析。
- 每个子文件分别登记哈希、父文件、包内路径、格式和处理状态。文件角色、发布时间、分包必须逐份确认，不能把 ZIP 时间戳或解压顺序当成文件效力顺序。
- 扫描件继续复用 `OCRProvider`；失败、缺页、低质量结果明确呈现。自动处理不能把 OCR 猜出的数字标成已核验承诺。
- 现有 OCR、图表和导出仍可使用原固定适配器；首轮不顺带重构全部外部程序。只有已完成隔离适配和验收的程序，才能进入自主处理工具清单。

## 6. 自主脚本执行边界

### 6.1 首选实现

首个执行后端采用 **Linux 容器内的固定 Python 运行环境**。Windows 开发机通过本地可用的 Linux 容器服务运行；需要先探测环境并实际验证挂载与限制，不假定当前机器已安装或可用。执行器不可用时关闭脚本入口，保留固定解析能力，不回落为宿主 Python/Bash。

执行器接口至少包含 `Capabilities / Start / Wait / Cancel / Inspect`，执行请求只含已授权输入清单、脚本字节、固定运行策略和限额。镜像、入口、挂载、用户、环境由服务端决定；不接受模型传入 Docker 参数或任意可执行文件。

必需边界：

- 非 root 用户、只读根文件系统、移除 capabilities、禁止提权；保留引擎默认 seccomp 配置，不使用 `unconfined`；不使用 privileged、host 网络/PID、宿主设备或 Docker socket 挂载。
- 禁止外网及宿主网络访问；输入只读；仅本次尝试的临时输出区可写；不挂载仓库、数据库目录、其他任务目录、企业证据根目录。
- 不继承服务端环境变量；不注入模型密钥、数据库连接串、服务 Bearer Token。固定 Python 依赖在构建镜像时安装并锁版本/镜像摘要，运行时不能 pip install。
- 内存/CPU/进程数/时间/文件数量/日志和输出字节都有限额，另设 `ulimit` 的 `fsize`、`nofile`。输出区采用有容量上限的临时文件系统等可验证机制，不能把普通宿主目录挂载当作磁盘配额。
- 按服务器构造的容器 ID 终止整次执行；仅结束 `docker` 客户端不算终止容器。启动时和定期协调数据库记录与运行实例，清理遗留作业。
- 容器停止后由可信收集器限量提取普通文件，拒绝链接、设备、FIFO、越界路径及异常归档，重新计算哈希；脚本自己报告的哈希和来源不能作为信任依据。若经 `docker cp` 取得 tar 流，按条目数、单条与累计字节流式解析，超限即中止。
- 更强隔离后端（如 gVisor `runsc`）可作为后续可选执行器，经同一 `Executor` 接口与验收矩阵接入，首版不依赖。

Docker 提供隔离、挂载和资源控制机制，但限制需要显式配置及验证；容器也不是绝对安全边界。执行服务的 Docker 控制权只对可信后端开放，跨企业生产启用另受 Phase 7 与部署审查约束。依据：[运行配置](https://docs.docker.com/engine/containers/run/)、[资源限制](https://docs.docker.com/engine/containers/resource_constraints/)、[引擎安全边界](https://docs.docker.com/engine/security/)（2026-10-31 查阅）。

### 6.2 初始策略值（建议，非实测）

| 项目 | 首轮建议值 | 实施要求 |
| --- | --- | --- |
| 单次输入 | 沿用 64 MiB 上传上限；ZIP 展开总量 256 MiB、最多 200 个条目、压缩比上限 100 | 可配置、服务端上界；执行中检查；需真实大文件调优 |
| 单脚本 | 64 KiB；单次最多 120 秒、1 CPU、512 MiB、32 个进程 | 内存与 tmpfs 合并核算；不足明确失败 |
| 输出 | 累计 32 MiB、最多 100 个文件；临时可写区 128 MiB | 超限立即终止/拒收，不继续填满磁盘 |
| 日志 | 每次尝试 stdout+stderr 合计 1 MiB；给模型摘要不超过 8 KiB | 流式截断/计数，不能先无限缓冲再截断 |
| 并发 | 每任务 1、全局 2 | 数据库/执行器共同控制，跨实例有效 |
| 自动修复 | 初始执行 + 最多 2 次修正（按 `parent_job_id` 父链长度计）；累计任务脚本执行预算 10 分钟、最多 10 次提交 | 基础设施重试也消耗预算；同 key 查询不重复扣费；固定处理作业单独记账，不占用脚本预算 |

执行期限取作业期限、Agent 剩余预算和任务 deadline 的最小值。排队时间计入截止时间。OOM、限额、取消、失去租约不能通过自动增加额度规避。

### 6.3 能力开关

拟增 `AUTOBID_FILE_PROCESSING_ENABLED`、`AUTOBID_SCRIPT_EXECUTION_ENABLED`、`AUTOBID_FILEWORK_DIR`、`AUTOBID_SANDBOX_IMAGE` 和策略配置。两个能力默认关闭；固定处理和脚本分别启用。脚本开关打开仍须能力自检通过；镜像未固定摘要或任何必需隔离能力不可验证时，不注册执行工具。状态接口返回具体原因，不泄露密钥或内部路径。

## 7. 作业状态机、恢复与采用

### 7.1 执行状态

```text
queued → running → validating → succeeded
   └──────────────→ failed / timed_out / cancelled
running / validating → recovering → queued（预算和重试仍允许）或 failed
```

取消意图单独持久化，避免取消请求与完成结果竞态。`succeeded` 表示执行及结构校验完成，结果是否可采用由单独的校验/来源状态决定，不代表业务正确。在 `validating` 阶段中断时，若产物已由可信收集器落盘并登记哈希，恢复后只重做校验，不重跑脚本；产物未完整登记才回到 `queued`。

- 领取使用行锁与 `SKIP LOCKED`，在事务中增令牌、占用并发槽并扣/预留预算；心跳以令牌 CAS 续租。实际耗时在结束时结算，异常中断保守计费。
- 心跳失败无法在租约截止前确认续租时停止执行；完成、发布产物与释放槽都检查当前令牌和取消意图。旧执行者不能写回或删除新尝试目录。
- 恢复先识别并终止旧容器，再重发；不能只因租约到期就同时运行两份。数据库登记 executor ID 前崩溃的窗口，通过确定性标签/实例标识协调回收。
- `Pause/KillWork/DeleteTask` 对关联 Agent 作业写入取消并终止执行；网页直接发起的任务级处理作业在暂停时同样停止，删除先做取消/清理屏障再处理外键。恢复不会重启已取消作业，用户或 Agent 用新请求键显式重试。
- 服务正常重启先停止领取、终止在途执行并记录；异常退出由下次启动协调。原 deadline、尝试次数和总预算不重置。
- 首版 Agent 执行工具采用“持久化提交后在本次调用中有界等待”的语义，返回 job ID、状态和摘要；Context 取消触发作业取消。超时不假定后台完成会自动唤醒 Planner，独立查询以数据库为准。

### 7.2 候选采用

产物生命周期：`unvalidated → reviewable / rejected → adopted`，源材料或目标版本变化后标记 `stale`。校验只验证结构及来源可解析；业务复核由人完成。

首版只开放“采用解析候选为正式招标文档/文档新解析修订”；清洗/比对报告可预览下载，不能直接变成要求、证据、报价或正文。采用之后仍调用现有要求提取流程，并由用户确认清单。后续增加目标类型时分别接入其业务服务，禁止通用 SQL 写回。

信任口径区分：**确定性固定解析器**（PDF、DOCX、XLSX、CSV/TSV、ZIP 内可解析子文件）对**新上传文件**沿用现有直接入库方式（增加原件留存与溯源），与当前 PDF/DOCX 一致，不额外要求人工采用；后续要求提取与清单确认照常进行。**人工采用只用于**两类：脚本生成的候选结果，以及对已有正式文档的重新解析修订（经 `AdoptTenderParseRevision`，见 §4.3）。

采用事务必须重新核对：租户任务、原件哈希及可用性、产物版本、来源定位、当前文档/清单版本、采用请求幂等性。脚本提取出的每项正式内容都必须能定位到原件页码或单元格；计算结果标记为派生值并附输入定位和计算方式，不伪装原文。无法可靠核对的内容只作报告，不进入自动采用。

F3a 已将 `Ingest` 拆为事务外 `PrepareTender` 与事务内 `PersistTender`，通过 `db.Transaction` 合并实体、文档、分块、来源及记录提交；采用修订同样把 `AdoptTenderParseRevision` 与采用记录放在同一事务。中途故障注入验证无半成品。后续脚本采用也须复用这一提交边界，不能在入库后另补“已采用”状态。

变更须保持 D1 的角色/发布时间/后发覆盖规则，以及要求 epoch、大纲 superseded、章节撤销批准、证据 stale 和导出门禁。处理失败本身不一律阻断所有项目导出；但被标记为必需且仍未采用/已失效的输入，不得被悄悄忽略。对此增加明确的“必需材料处理未完成”就绪度检查，而不是把全部处理任务作为新门禁。招标原文实质变化后的“待重新提取要求”阻断项见 §4.3。

## 8. Agent、API 与页面契约

### 8.1 Agent 工具

新增 `file-processing` Skill；只给任务 Worker，经 `DeferredTools` 解锁。Planner 可看到处理状态摘要，首版不执行脚本；`SectionWriterTools()` 保持现有写作辅助范围。

| 拟新增工具 | 输入概要 | 输出概要 |
| --- | --- | --- |
| `file_inspect` | `file_id` | 格式、工作表/附件清单、可用固定操作、限制、原件哈希 |
| `file_process` | `input_file_ids, operation, params, request_key` | 固定处理作业 ID、状态、候选产物与警告 |
| `python_process` | `input_file_ids, script, purpose, output_contract, request_key` | 隔离执行作业 ID、退出状态、验证结果与摘要 |
| `file_job_status` | `job_id` | 持久状态、失败原因、产物 ID |
| `file_read_output` | `output_id, offset, limit` | 有界分页内容及来源；每次检查归属和失效状态 |

不提供 Agent 自动采用工具。所有请求参数由服务端完整解码，拒绝未知字段、未知操作、未授权文件；现有轻量 Schema 校验不是嵌套参数安全校验的替代。脚本审查/静态扫描可帮助发现错误，但不作为隔离保证。

新执行工具不加入 `WorkerWritebackTools`；验证直调、`ExecuteExtraTool`、Hook 改写后三条路径均无法在收尾阶段启动执行。即使 Skill 已解锁、权限模式跳过一般工具权限，服务自身仍检查开关、范围、预算与隔离能力。`intercept_pending` 仅用于在途工具审批，不承载候选采用状态。

### 8.2 拟新增 HTTP 路由

所有任务路由通过现有 `withTask` 及 Bearer 鉴权接入，在 `server/http.go` 显式注册：

| 方法与路径 | 语义 |
| --- | --- |
| `GET /api/file-processing/status` | 固定处理/脚本能力、后端自检和禁用原因 |
| `POST /api/tasks/{id}/files` | 保存原件；返回 file ID，不自动确认内容 |
| `GET /api/tasks/{id}/files` | 可用原件和来源状态 |
| `GET /api/tasks/{id}/files/{fileID}/inspect` | 有界预检；复杂预检转作业，不长期阻塞请求 |
| `POST /api/tasks/{id}/file-jobs` | 提交固定处理或脚本，返回 202/job ID |
| `GET /api/tasks/{id}/file-jobs` | 分页处理记录 |
| `GET /api/tasks/{id}/file-jobs/{jobID}` | 当前状态及日志摘要 |
| `POST /api/tasks/{id}/file-jobs/{jobID}/cancel` | 幂等取消意图 |
| `GET /api/tasks/{id}/file-jobs/{jobID}/outputs/{outputID}` | 有界预览；下载使用鉴权附件响应 |
| `POST /api/tasks/{id}/file-jobs/{jobID}/adopt` | `output_id, expected_version, target_kind, review_note, request_key`；人工采用 |

统一状态语义：不存在或跨任务资源 404；版本/幂等冲突 409；格式或产物校验失败 422；额度不足 429；执行器不可用 503。SSE 用 `db.Activity` 推送处理状态摘要，页面失联/重连后重新 GET，不要求用户从日志判断成功。

### 8.3 页面与权限

在 `DocumentsPage`/`DocumentParts` 增加“处理材料”入口、处理记录与预览抽屉，显示原件、采用状态、缺口和失败原因。工作表预览可跳至来源单元格；附件列表逐件显示是否处理。高级详情才展示脚本、运行环境摘要、完整日志下载和版本。

前端文件类型允许列表与后端真实支持范围同步；未支持格式不应仅因文件选择器放开而显示“支持”。产物文本转义展示，不执行 HTML/JavaScript；CSV 下载防止把模型生成文本作为电子表格公式执行，同时保留内部原值供审计。

`X-Autobid-User` 目前只是审计声明，不能作为可靠身份或跨用户审批证明。首版按现有单团队鉴权使用；Phase 7 前不宣称多企业生产隔离或可信的撰写/审批角色分离。

### 8.4 候选解析输出最小契约

固定处理器与 Python 统一输出版本化 JSON，至少包含 `schema_version`、`source_files`、`blocks`、`tables`、`warnings` 和 `coverage`。每个 block/cell 携带原件 ID、哈希和页/单元格位置；`coverage` 列出已处理和未处理的页、工作表或附件。脚本声称的覆盖必须与可信预检清单对照，未识别的范围显示缺口。脚本本身不能决定文档效力、清单确认状态或复核结论。

去重分两层：`request_key` 只负责幂等（同 key 同输入返回原作业，同 key 不同输入 409）；`purpose`、输入文件列表、参数、脚本哈希、策略版本和输出契约构成的请求指纹仅用于可选的结果缓存，不同 key 命中同一指纹时可复用已有产物，但复用前重新校验输入权限与失效状态，且不重复扣预算。格式修正产生新作业并以 `parent_job_id` 关联，不覆盖旧脚本和输出。结构校验失败、来源无法核对或 `coverage` 不完整时，页面明确提示并阻止把结果标记为完整解析。

## 9. 分阶段实施与完成标准

| 阶段 | 改动与交付 | 可执行完成标准 |
| --- | --- | --- |
| F0 基线与样本 | 固定失败样本；定义来源/输出契约、限额、环境探测；记录现有导入基线 | [x] E（2026-10-31）开发样本清单、人工期望值、固定工具/脚本适用分类齐全；已记录容器后端阻塞（PATH 无 `docker`、WSL 未安装、无 `podman`），未回落宿主执行。证据见 [`eval/file-processing/`](../../eval/file-processing/) 与 [`PROGRESS.md`](PROGRESS.md) |
| F1 原件与固定处理 | 检索触发器按列修正；新原件存储/表；旧上传兼容；格式路由；XLSX/CSV/TSV/ZIP；表格切分；页码和单元格溯源贯通到要求；`AdoptTenderParseRevision` | [x] E（2026-10-31）原件可重读、哈希一致；解析失败仍可重试；旧数据明确缺原件；格式精度/合并/隐藏/公式/ZIP 边界用例通过；迁移与关联原件后既有检索证据状态不变；重新解析修订的要求失效粒度用例通过；“待重新提取要求”阻断确认与导出（含仅新增分块、原未关联要求的分块变化）用例通过。证据见 [`F1_ACCEPTANCE.md`](F1_ACCEPTANCE.md) |
| F2 执行服务 | 作业/预算/审计表；Runner；容器 Python；取消/恢复/收集校验 | [ ] **启动关口**：F0 拿出固定工具确实无法处理的真实样本（数量门槛在 F0 冻结），且容器后端在目标环境验证可用；关口未过不建 F2 表、不编码。[ ] 真实隔离后端完成正常执行、所有越界/限额测试、失去租约与重启恢复；未达标不开放 Agent 脚本工具 |
| F3 业务闭环 | Skill/工具、HTTP/SSE、文档页、采用事务、下游失效与必要材料门禁 | [ ] 完成“上传→处理→查看来源→人工采用→要求提取/确认”；无绕过审批，旧入口兼容，竞态与幂等通过 |
| F4 外部验证与灰度 | 冻结样本与协议，比较固定工具、脚本辅助、人工处理 | [ ] 按下节报告实际质量/时间/成本，达到预先冻结门槛后单团队灰度；V/P 状态分别记录 |

先完成 F0/F1，可独立提高文件处理能力。F2 必须先于脚本工具对模型可见；F3 的采用链首先用于重新解析修订。未遇到固定工具无法解决的真实需求时，不为演示而强制走脚本。

### 9.1 F3a 提前交付范围（2026-10-31）

按用户确认，将不依赖容器的固定解析闭环提前交付：ZIP 子文件页面导入、既有文档重新解析候选/差异预览/人工采用、持久记录、事务及下游门禁；同时补齐独立数据库升级和备份恢复演练。实际路由与验收见 [`F3A_ACCEPTANCE.md`](F3A_ACCEPTANCE.md)。

本阶段固定解析随 HTTP 请求同步执行，不创建异步 Runner；以 `file_processing_records` 保存请求指纹、候选、预览、失败或采用结果。解析在事务外进行，正式入库和结果提交保持原子性；失去响应可用同请求键重试，已生成候选可刷新后继续核对。固定处理的流式上传、异步作业和必需材料门禁现拆至 F3a+；F3b 仅承接依赖 F2 的脚本/Agent 工具与脚本业务闭环。F2 启动关口保持不变。

F3a 专项、浏览器与恢复演练通过；2026-11-01 真实 PostgreSQL 全量竞态及两个 LibreOffice 导出用例复验通过，F3a 已收口为 E，详见[复验记录](../LIBREOFFICE_VALIDATION.md)。不代表完整 F3/F4 或 V/P 完成。

### 9.2 F3a+ 固定处理增强（2026-10-31 开始）

实施顺序：LibreOffice 真实导出收口 → 必需材料清单/门禁 → 流式原件上传 → 持久化固定解析作业与恢复。此阶段不依赖 F2，不接受用户脚本，不注册任意代码执行工具。

- 必需材料清单可在上传前登记，绑定同任务原件；只有原件可用且正式导入后才算完成。取消必需标记须留操作人和说明。未上传、未导入、候选待采用或来源失效均阻断；非必需失败不一律阻断。创建导出快照与清单变更采用同一串行化边界并保存门禁快照。
- 新 `POST /files` 流式落盘与 SHA-256，原件成功登记后才返回；保留旧同步上传兼容。解析阶段仍有界读入内存，单文件/ZIP 限额与 worker 并发限制继续生效，不宣称全链路流式解析。
- 固定作业在 PostgreSQL 持久化不可变输入、请求指纹、状态、尝试数、截止时间、租约/围栏令牌和错误。只实现固定处理需要的执行字段，不提前建设 F2 脚本/容器表。网络重发复用同请求键，显式重试创建新请求；确定性解析失败不自动重试，过期租约恢复受尝试次数和截止时间约束。
- 后台解析与 HTTP 请求生命周期分离；正式入库/候选及成功状态同事务提交，旧执行者不得写回。暂停/删除取消在途固定作业；已取消任务不因恢复自动重启。GET 为状态权威来源，SSE 仅用于刷新提示。
- 验收：真实 PostgreSQL 下验证未上传/未导入/失效阻断、非必需失败、并发导出与标记变更、同键冲突/重发、租约回收和旧令牌拒绝、断线后查询、取消/重启恢复、解析失败保留原件；浏览器覆盖上传→后台处理→刷新→查看/采用→门禁。执行 `go vet ./...`、真实 PostgreSQL `go test -race ./...`、前端单测/构建及浏览器闭环。合成样本仅支持 E；F4/V/P 继续等待真实材料。

## 10. 验收矩阵与评测边界

### 10.1 工程测试（E）

| 类别 | 必测情形 |
| --- | --- |
| 文件正确性 | 编码/分隔符、前导零、超长编号、金额、小数、日期、空表、合并单元格、隐藏行列/表、公式缓存缺失、重复表头、损坏文件、缺原件；大表按工作表/行窗口切分且表头重复、单元格定位进入 `source_loc` |
| 格式路由 | `.csv` 以 `text/csv`、`application/vnd.ms-excel` 或无 MIME 上传均进表格解析；`.xlsx` 不再返回不支持；以 Store 方式收入 .docx 的 ZIP 不被判为 DOCX |
| 包处理 | ZIP 路径穿越/盘符/重复路径/链接/伪造大小/高压缩比/嵌套包；展开中触达上限即停止 |
| 迁移与失效 | 空库与旧库升级后 `retrieval_evidence` 状态不变；仅关联 `original_file_id` 不使证据失效；重新解析修订后：未变分块的要求保持 `confirmed`，变化分块的要求变为 `needs_revision`，被删除分块的要求变为 `needs_revision` 且 `source_chunk_id` 为 NULL，清单 epoch 递增；快照写入后不可修改。**仅新增分块**（补出遗漏的强制条款、没有任何已关联要求变化）：清单变为 `stale`、已批准大纲变为 `superseded`、确认被拒绝、导出被阻断，直到重新提取并复核后才解除。**原未关联要求的分块变化**：结果同上。**无实质变化的修订**：清单、大纲、导出状态均不变。**提取与采用并发**：提取开始后才提交的修订不会被这次提取清除待提取状态 |
| 隔离 | 读取宿主与其他任务文件、读取服务密钥、连接数据库/外网/宿主端口、改原件、提权、资源耗尽、无限输出、遗留子进程；以实际执行结果验证 |
| 输出 | 伪造 provenance、错误哈希、链接/特殊文件、超大文件、缺字段、错误金额类型、混淆原文与计算值、输出含提示指令；不通过即不可采用 |
| 生命周期 | 排队超时、运行取消、Agent 暂停、KillWork、删除任务、服务重启、心跳断连、旧令牌晚到、旧容器存活、全局并发与预算争抢 |
| 工具门禁 | 未解锁直调、包装调用、收尾执行、Hook 改写输入、执行器不可用；拒绝必须发生在副作用前 |
| 采用事务 | 同结果重复采用、同 key 不同参数、跨任务 ID、原件/目标版本变化、并发采用（`parse_revision` 乐观锁，仅一个成功，其余 409）、中途数据库故障（快照/分块/版本无半成品）；旧批准撤销与导出门禁传播 |
| UI/API | 状态码、下载授权、文本转义、断线重连、能力关闭、失败重试、原件定位、旧上传兼容 |

后端必须通过 `go vet ./...` 与设置 `AUTOBID_TEST_DATABASE_URL` 的真实 PostgreSQL `go test -race ./...`，检查集成测试未被跳过。隔离测试必须在真实后端执行，替身 Executor 仅用于普通单元测试；无容器环境时记录“F2 未验收”，不能用 skip 宣称通过。前端通过 `npm --prefix web run test:unit`、`npm --prefix web run build` 及覆盖上述闭环的浏览器验证。

迁移验证包含空库、当前旧库升级、重复执行幂等及备份恢复演练。加入故障注入测试，核对正式业务状态和文件引用，而非只检查 HTTP 200。

### 10.2 业务验证（V）

建议新增 `eval/file-processing/`：样本 manifest、哈希、来源权限、人工校对期望、固定版本评测器与报告。真实材料按项目族隔离开发集和保留集；合成攻击/异常样本只证明工程边界，不能证明真实提取质量。

在看到结果前冻结：关键字段准确率、应提取行列/附件完整率、来源定位正确率、失败识别率、人工复核/修正时间、端到端 P95、模型与执行成本。固定工具与“固定工具+脚本”使用相同输入和复核口径。建议硬门槛为：验收样本中没有静默丢失关键附件/字段、没有未授权读写、没有越过人工采用；数值质量与时间收益门槛由 F0 样本基线提出并在保留集运行前确认。

不得仅报告脚本运行成功率，也不得把当前 10 份已看过的招标文件当保留集。缺真实复杂附件或人工金标准时，F4/V 明确阻塞，仍可完成 E。脚本没有稳定收益的场景继续使用固定工具。

## 11. 上线、回退与维护

- 分批启用固定处理和脚本；初始仅单团队/测试项目。界面保留原有上传入口，所有功能状态以服务端能力响应为准。
- 关闭开关后停止接受/领取相应新作业；在途脚本终止并落取消记录，候选产物保留可查。已采用文档继续按既有来源/版本状态使用，不能因开关关闭自动回滚业务内容。
- 原件和已引用产物依业务留存策略保留；临时执行目录在收集后清理，孤立产物/日志按配置的保留期回收。磁盘不足时停止领取，不能删正式引用来腾空间。
- 故障回退优先关闭功能、回退程序版本，采用加法迁移保留数据；数据库回滚涉及数据删除时需另行制定恢复方案。
- 监测排队时长、超时/OOM、租约丢失、孤立容器、输出拒收、预算耗尽、采用失败及处理成功但人工大量纠错的情况。
- 本文不变更既有 ROADMAP 完成状态。F1 启动时在 `ROADMAP.md` 增加一条指向本方案的跟踪条目（`[ ]`，不改动其他条目状态）；实施后逐阶段登记 E；只有达到业务协议才登记 V/P，并在 `CLAUDE.md` 同步真实模块职责和限制。

## 12. 首个可交付切片

先交付 **“上传 XLSX → 保存原件 → 固定提取工作表与单元格（按行窗口切分入库）→ 文档页预览并按单元格核对来源 → 进入现有要求提取/确认流程”**。确定性固定解析器沿用直接入库，与现有 PDF/DOCX 一致（§7.2）。

这条链验证存储、来源定位和表格切分，且不依赖自主脚本或作业表。第二个切片加入“对已有文档重新解析 → 预览影响范围 → 人工采用修订”，验证 `AdoptTenderParseRevision` 与要求失效粒度。第三个切片（需 F2 关口通过）才加入“固定解析失败后，受限 Python 提取同一份材料”的路径，复用相同的产物校验和采用机制。各路径的正式结果必须遵守同一套业务规则。
