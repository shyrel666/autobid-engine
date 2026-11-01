# CLAUDE.md — AutoBid Engine 开发宪章与架构指南

本文件为 AI 编码助手（Claude Code / Codex）的常驻工程记忆文件。在开始任何分析、重构或功能开发前，请务必遵循本文件与 [`ROADMAP.md`](./ROADMAP.md) 中的真实代码基线与架构规范。

---

## 1. 项目定位与当前真实基线

`autobid-engine` 是一套基于 **PostgreSQL 双图存储**（`assets` 招标静态要素图 + `explorations` 任务推演图）的**企业级 AI 技术标书编写与合规审查系统**。

- **已完成基线（详见 [`ROADMAP.md`](./ROADMAP.md) 第一节）**：
  - **Phase 1**：PostgreSQL 存储与幂等迁移（`db/schema.sql`）、双图状态与租约争抢（`db/exploration.go`, `db/assets.go`）、冷节点折叠与有界概览（`db/digest.go`, `db/overview.go`）、LLM 连接池与审计录制（`llmpool`, `llmrec`）、工具调用级拦截与计数（`intercept`, `guard`）及 HTTP/SSE 服务层（`server/http.go`）。
  - **Phase 2**：纯文本/Markdown 招标文档章节切分（`ingest/splitter.go`）、企业资信库（`company_credentials`）、标书核验工具与技能（`bidtools/`：`document_outline`、`search_document`、`read_chunk`、`query_company_credentials`、`simulate_scoring_table`、`verify_commercial_bid`）及结构化审查报告（`db/report.go`）。
  - **Phase 3**：单团队标书合规审查工作台前端（`web/`，对应缺口 #9）+ 轻量配套后端接口（`PUT/DELETE /api/company/credentials/{id}`、`GET /api/tasks/{id}/report?format=markdown`、`server/static.go` 托管 `web/dist` 并做 SPA 回退）。
  - [x] **Phase 4**：多格式文档深度解析（`ingest/`：DOCX、自研 PDF 解析、Markdown、GB18030 文本、可插拔 `OCRProvider`；页码范围、跨页表格 JSON + Markdown、版面溯源）与 PostgreSQL 企业方案知识库（`knowledge_documents` / `knowledge_chunks`、`db/knowledge.go` 全文 + 向量 + 关键词混合检索、`kb_search_proposals` / `kb_search_products` / `kb_read_chunk` + `knowledge-reuse` 技能、`/api/knowledge/*`、multipart 文档上传、「企业方案知识库」页面），对应缺口 #1、#4。
  - **Phase 4 的完成边界**：上述为功能基线。默认 `HashEmbedder` 是 256 维词袋哈希；`ts_rank_cd` 不是 BM25。HTTP 语义 embedding / rerank 支持 `tei` / `compatible` / `dashscope` 三种格式。2026-10-29 起接入阿里云百炼 `qwen3.7-text-embedding-flash` / `qwen3.7-text-rerank`（开发用临时授权，各 1M token，2026-11-29 到期，配置见 `eval/retrieval/README.md`），只跑过合成回归。已实现 RRF/reranker 接口、版本化索引切换、持久化证据和预算/租约协调；多 Agent 默认关闭。现有引用字段与样例命中测试不能证明业务检索质量达标。
  - [x] **Phase 5**（2026-10-28 工程验收）：域 C 要求清单与大纲 / 章节状态机（对应缺口 #2、#3、#5）。`db/schema.sql` 新增 `requirement_manifests`、`requirement_items`、`proposal_outlines`、`proposal_sections`、`proposal_section_requirements`、`section_revisions` 及失效触发器；`db/requirements.go`（遍历原文分块 + 招标要素幂等提取、人工增删改、清单确认 epoch）与 `db/proposal.go`（大纲 draft→pending_approval→approved→superseded、`ClaimSectionWrite` 派发门禁、`SaveSectionRevision` 写回二次门禁、绑定版本批准 / 自动撤销 / 回滚）；`bidtools/proposal.go`（`proposal-writing` 技能解锁 `section_read_context` / `section_write_draft`）；`server/proposal.go`（`/api/tasks/{id}/requirements|outlines|sections/*`，派发异步 AI 撰写）；前端任务页「技术标大纲与写作」与 `/proposal` 入口。
  - **Phase 5 的完成边界**：状态机、门禁与证据失效联动经真实 PostgreSQL + `-race` 验证；`ccgp_2026-10-06/` 10 份真实招标文件（5 DOCX + 5 PDF）全链路无解析 / 状态机报错。要求提取是启发式规则，召回与准确率未做人工标注评测，清单必须人工复核后确认。评分表已改为结构化解析（`db/scoring.go`：列角色识别、合并 / 续行 / 跨页、分包、技术 / 商务 / 价格分组 `requirement_items.score_group` 与合计校验；PDF 表格由 `ingest/pdfruled.go` 按框线还原单元格），10 份真实招标 16 个分包评分合计均为 100 分；价格评分项不进入技术标大纲、映射门禁与响应覆盖（`RequirementItem.InTechnicalScope`）；多分包尚无「投标分包」选择。AI 派发为单次 LLM 调用，测试使用替身 Writer，未在真实模型上评估写作质量；知识库证据仍受 Phase 4.3 未验收边界约束，未核验证据只会被记为缺口。
  - [x] **Phase 6A**（2026-10-28 工程验收）：双层响应核验矩阵与全文一致性核验（缺口 #6）。`db/schema.sql` 新增 `compliance_matrix`、`consistency_runs`、`consistency_findings` 及写入守卫与四条失效触发器；纯规则包 `compliance/`（`Verifier` 接口、`RuleVerifier`、可选 `LLMVerifier`、关键承诺抽取 `Extract` / `Compare`）；`db/compliance.go`（`VerifyComplianceMatrix`、矩阵查询、两层覆盖率）、`db/consistency.go`（`RunConsistencyCheck`、结论乐观锁处理）、`db/export_readiness.go`（`ExportReadiness` / `exportReadinessTx`，6B 导出门禁复用）；`bidtools/review.go`（`proposal-review` 技能：`compliance_verify`、`fulltext_consistency_check`）；`server/compliance.go`；前端「④ 响应矩阵」「⑤ 一致性核验」与导出就绪度卡片。
  - **Phase 6A 的完成边界**：规则抽取 / 规则核验的召回率与准确率未经人工标注评测，验收样本是合成的；LLM 核验只有接口与测试替身，未接入生产；一致性只比较同一指标数值，不理解分阶段 / 分子系统语境；导出就绪度只判定、不导出。
  - [x] **Phase 6B**（2026-10-28 工程验收，Phase 6 整体 `[x]`）：标书图表与锁定版本 DOCX/PDF 套版导出（缺口 #7、#8）。`db/schema.sql` 新增 `proposal_charts`（绑定 `section_rev_id` + `rev_no`，查询时判定过期）、`export_templates`、`export_jobs`（`rev_snapshot` / 图表 / 偏离表 / 文档骨架快照不可修改，`lease_token` 围栏令牌）；`db/charts.go`、`db/exports.go`（`CreateExportJobs` 在同一 REPEATABLE READ 事务内加锁 + `exportReadinessTx` + 固化快照，未就绪返回 `*ExportBlockedError` → 422）；`export/`（结构化规格 → Mermaid / 设备表 JSON、mmdc 渲染、自研 WordprocessingML 套版、LibreOffice UNO 刷新目录并转 PDF、`Runner` 异步执行与回收重试、`CheckPDF` / `CheckDOCX` 回读自检）；`bidtools/charts.go`（`chart_generate`，经 `proposal-writing` 解锁）；`server/export.go`；前端「⑥ 导出成稿」与章节图表卡片；`db/dbtest` 构造可导出标书供跨包集成测试。
  - **Phase 6B 的完成边界**：本机 LibreOffice 26 上目录页码与 PDF 版式经回读验证；mermaid-cli 未安装、未验证（图表以源码占位导出，版式测试用测试替身 PNG）；模板占位符须在同一 `w:t` 内、标题为字面编号；真实招标 / 企业投标模板未逐份适配。
- **增强工程与待验收阶段（详见 [`ROADMAP.md`](./ROADMAP.md) 第四节）**：
  - **深化阶段 D0 ~ D5（2026-10-29 起的关键路径）**：D0 原始素材与评测协议 → D1 招标包效力 / 否决项 / 得分点 → D2 证据可采性与承诺台账 → D3 故事板与句子级落证 → D4 评分索引 / 全文件一致性 / 防雷同 → D5 回测与试点。状态分 E（工程实现）/ V（外部验证）/ P（生产启用）三级，上述 Phase 1 ~ 6B 的 `[x]` 均只到 E；合成样本、替身模型与自测不能支撑 V / P。旧版路线图（含 Phase 3 ~ 6B 交付记录）存档于 [`docs/archive/ROADMAP-2026-10-28.md`](./docs/archive/ROADMAP-2026-10-28.md)。
  - **D2 现状（2026-10-31，E 级）**：企业证据（租户级 `company_credentials` / `company_performance` / `company_evidence_originals`）与 `evidence/` 内容寻址原件；任务域 C 的 `requirement_evidence_bindings`、`admissibility_runs/items`（要求级聚合与逐证据明细，`manual` 人工复核生成新版本）、`objective_score_runs`、`commitment_ledger`、`bid_quotes/bid_quote_items`、`section_revision_evidence` 与 `section_revisions.commitment_ids`。规则纯函数在 `compliance/admissibility.go`；db 层负责加载、加锁、持久化与失效传播；`ExportReadiness` 新增 `admissibility_*`、`commitment_stale/unconfirmed`、`objective_*`、`quote_*` 阻断。验收执行器 `eval/bid/d2/` 覆盖 S1~S16 与 D2-G01~G14；报告与 `d2-artifacts/` 只保留本地。V/P 仍 BLOCKED。
  - **F0 / F1 复杂文件固定处理（2026-10-31，E 级）**：按 [`docs/file-processing/PLAN.md`](docs/file-processing/PLAN.md) 完成基线与原件/固定处理阶段，验收见 [`docs/file-processing/F1_ACCEPTANCE.md`](docs/file-processing/F1_ACCEPTANCE.md)。
    - `filework/` 内容寻址原件存储（流式 SHA-256、原子移动、读取校验、孤立对象清理）；`task_file_objects` 按任务登记原件/脚本/派生/文本快照，含包内路径、父文件、格式与处理状态；`tender_documents.original_file_id / parse_revision`。旧数据无原件明确显示“原件未留存”。
    - `ingest/spreadsheet.go`：XLSX（excelize v2.9.1，保留合并/隐藏行列与工作表/公式缓存/原始值/显示值/日期）、CSV/TSV（编码、分隔符、前导零、长编号、金额）；表格按工作表 → 行窗口切分（600 token / 40 数据行，重复表头），`tender_documents.page_count` 与分块 `page_start/page_end` 为 NULL，`bbox_meta.page_is_physical=false`。
    - `ingest/archive.go`：`.zip` 扩展名优先；DOCX 嗅探读 ZIP 中央目录；ZIP 路径穿越/盘符/反斜杠/链接/重复路径/嵌套/高压缩比/条目与字节上限拒绝；子文件只登记，不自动成为正式文档。
    - `db/tender_revisions.go`：`AdoptTenderParseRevision` 先写不可变快照，再按 seq 差量 UPDATE/INSERT/DELETE；保持文档 ID、分块 ID 与 `content_hash`（原始上传字节哈希）不变。
    - `requirement_manifests.source_epoch / extracted_source_epoch`：新文档、实质分块变化或采用修订推进来源 epoch；`source_epoch > extracted_source_epoch` 时 `ConfirmRequirements` 拒绝确认，`ExportReadiness` 返回独立 `requirements_source_changed` 阻断，即使清单状态被改回 confirmed。
    - 检索失效触发器改为按内容列（文档 `raw_text/title/tender_no/lot_no/doc_role/published_at`；分块 `content/section_code/section_title/page_start/page_end/tables_json`）或 DELETE 触发；关联原件、`parse_revision`、`page_count`、`parse_meta`、`token_estimate`、`bbox_meta` 不使证据失效。
    - 默认 `AUTOBID_FILE_PROCESSING_ENABLED=false`；`AUTOBID_FILEWORK_DIR` 默认 `data/filework`。F2 脚本执行因容器后端与真实复杂样本关口未通过而未启动。
  - **D0 / D1 现状（2026-10-29，E 级）**：
    - `eval/bid/` 存放公开语料清单 `corpus.json` 与评测协议草案 `PROTOCOL.md`（待用户冻结）；其中 10 个项目在编写规则时已被查看，只能作开发集。
    - D1 的数据：`tender_documents.doc_role / published_at`（后发布的文件优先）；`requirement_manifests.bid_deadline / bid_deadline_source / bid_lots`；`requirement_items` 新增 `veto_item` 类别及 `veto_kind / veto_status / lot / out_of_bid`。
    - D1 的规则：`db/veto.go`（审查表解析、法定否决项、截止时间抽取、`SetVetoStatus`、`SetBidScope`）。否决项不进入技术标大纲；待核对或不通过时导出门禁返回 `veto_pending` / `veto_failed`。
    - D1 的路由：`PUT /api/tasks/{id}/requirements/{reqID}/veto` 与 `PUT /api/tasks/{id}/requirements/bid-scope`。
    - 重新提取时，原文与规则产出都相同才算"未变"。
  - **Phase 4.1**：真实业务评测集、可复现基线与冻结的质量/性能/成本门槛。
  - **Phase 4.2**：共享混合检索服务、语义 embedding、RRF、reranker、版本化索引与降级；按部署条件和实测选择 pgvector/HNSW。
  - **Phase 4.3**：结构化证据、原文/型号/版本/数值核验、证据失效传播与单 Agent 有界补查。
  - **Phase 4.4**：按独立证据范围派发多 Agent，补齐汇总门禁与全局预算，通过对比评测决定启用范围。
  - **Phase 7**：多企业租户隔离与多角色权限控制 RBAC（对应缺口 #10）

---

## 2. 目录与模块真实职责

**2026-11-01 ARTEX 推演与长文上下文增强（E 级）**：按 [`docs/ARTEX架构技术标系统解析.md`](docs/ARTEX架构技术标系统解析.md) 与 [`docs/implementation_plan.md`](docs/implementation_plan.md) 吸收 DeepSeek Super-agent 与 ARTEX 核心运行机制，验收记录见 [`docs/walkthrough.md`](docs/walkthrough.md)：
1. **Goals 目标分解器（`BootstrapRFPGoals`）**：`db/rfp_goals.go` 串联 `ExtractRequirements` → 同步 `assets` 静态要素并回填 `requirement_items.source_asset_id` → 在 `exploration_nodes` 幂等创建四大 `kind='goal'` 根节点（`G-VETO` 资格/否决项、`G-DEVIATION` 实质性条款与技术偏离、`G-SCORING` 评分标准拆解、`G-OUTLINE` 技术标目录大纲）及 `exploration_anchors` → 可选生成 `v1 draft` 技术标目录大纲；暴露 `POST /api/tasks/{id}/goals/bootstrap`、Planner 工具 `decompose_rfp_goals` 及 `POST /api/tasks/{id}/documents?bootstrap_goals=true`。
2. **Intent Mandate 硬件门禁 + 确定性 Watchdog + Hint 事件直通**：`db/constants.go` 新增 `IntentMandate`（`AllowedSectionIDs`、`AllowedRequirementIDs`、`TargetWordsMin/Max`、`MaxToolCalls`、`ForbiddenTools`、`RequireWriteback`）；`server/watchdog.go` 在 `PreToolUse` 层拦截越界章节/要求/字数与禁用工具，在 `PostToolUse` 层对重复调用、连续报错与空转轨迹自动投递纠偏 Steer 并触发 `AbortKilledByWatchdog` 熔断（唤醒 Planner `TriggerWatchdogAlert`），在 `Stop` 层执行 `RequireWriteback` 完结门禁；`Engine.RouteHintToRunningWork` 将命中在途 Worker 锚点的 `AddHint` 直接推入 `steerBox` 并标记 `consumed`。
3. **Inline & Spill 上下文溢出管理与链式召回**：`bidtools/proposal.go` 将本章节直接绑定证据保留在 `UsableEvidence`（Inline），其余任务级可用证据折叠为 `SpilledEvidence` 微摘要（Spill）；`bidtools/documents.go` 为 `read_chunk` 补齐 `text_offset / table_index / row_offset / context_budget` 分页；`server/llm_runtime.go` 的 `prepareContext` 引入 Stage 1 旧 `BlockToolResult` 零成本 Spill 折叠（附 `recall_keys`）与 Stage 2 工具回合边界安全摘要压缩（修复连续工具调用无第二条文本 User 消息时的 `413` 崩溃）。

**2026-10-31 F3a+ 增量**：`db/materials.go` 的 `task_materials/task_material_events` 支持上传前应收材料清单、版本和排除说明；`required_material_incomplete` 接入导出事务与 gate 快照，任务 `file_gate_version` 防止并发旧快照漏拦。`server/fixed_jobs.go` 的 `POST /files?filename=...` 原始流上传与 `/file-jobs` 固定队列，由 `filework/jobs.go` + `db/fixed_jobs.go` 执行；`fixed_file_jobs/fixed_file_job_events` 保留不可变输入、租约围栏、尝试/截止时间、取消与审计。暂停/删除阻止在途写回；后台成功状态与正式入库同事务。`FixedWorkflow.tsx` 提供材料清单和后台处理页面；旧同步路由兼容。默认仍须 `AUTOBID_FILE_PROCESSING_ENABLED=1`，worker 数由 `AUTOBID_FILE_WORKERS` 配置（默认 2，上限 8）。这不是 F2 脚本 Runner；F2 未启动。专项、浏览器 4/4、升级恢复通过，完整 E 仍待 LibreOffice 导出。详见 [`F3A_PLUS_ACCEPTANCE.md`](docs/file-processing/F3A_PLUS_ACCEPTANCE.md)。

**2026-10-31 F3a 增量**：ZIP 子文件已可在文档页“材料处理”中选择导入；已有文档支持持久化重新解析候选、分页差异预览和人工采用。入口在 `server/file_processing.go`，编排在 `filework/processing.go`，记录表为过程域 `file_processing_records`；实际 API 与验收见 [`F3A_ACCEPTANCE.md`](docs/file-processing/F3A_ACCEPTANCE.md)。`ingest.PrepareTender/PersistTender` 与 `db.Transaction` 将实体、文档、分块、来源关联、采用记录及失效状态合并提交；事务句柄只能在回调内使用，调用存储层的 Context 方法并向外传播错误，不得通过内嵌 `.DB` 绕开事务。迁移重放不得回填表格的物理页码。独立旧库升级/备份恢复工具在 `cmd/file-migration-drill/`，已用冻结旧 schema + 代表性合成数据实跑；不是生产备份演练。F3a 专项通过，完整 E 尚受两个既有 LibreOffice 环境失败阻塞；F2/F3b 脚本能力仍未开放。

- `cmd/server/`、`cmd/migrate/`：服务启动入口与数据库迁移 CLI。
- `db/`：**PostgreSQL（`pgx/v5`）存储层**。
  - **迁移机制**：当前 `db.Migrate(ctx)` 通过 `//go:embed schema.sql` 读取 `db/schema.sql`，对比 `schema_version` 表中的 SHA-256 校验和并在 `pg_advisory_xact_lock` 下执行幂等 DDL（注意：当前**不读取** `db/migrations/` 目录；新增表或字段必须以幂等语法写入 `db/schema.sql`，或先显式改造迁移器）。
  - **数据域划分**：
    1. `assets` + `entity_edges`：仅存储招标文件静态要素（`tender`、`lot`、`clause`、`qualification`、`scoring_item`、`commercial_item`）；
    2. `explorations` + `exploration_nodes` + `findings`：存储任务推演过程节点与核验结论产出物；
    3. `tender_documents` + `document_chunks` + `company_credentials`：存储招标原文分块（含 `page_start`/`page_end`、`tables_json`、`bbox_meta` 与来源文件元数据）与企业资信档案；F1 另有 `task_file_objects`（任务原件/脚本/派生/文本快照登记，内容寻址物理对象按租户共享）与 `tender_parse_revisions`（正式解析修订的不可变旧正文/分块快照），均属域 A 溯源层，不写入企业证据表；
    4. `knowledge_documents` + `knowledge_chunks`（域 D）：企业方案/产品知识库，按 `tenant_id` 隔离，`search_tsv` 为由 Go 层中文双字词元生成的 tsvector 列（PostgreSQL 内置分词不切中文，勿改为直接 `to_tsvector(content)`），`embedding` 只与同 `embedding_model` 向量比较；
    6. 导出附件域（Phase 6B）：`export_templates` + `export_jobs`（快照列不可修改，执行用租约 + 围栏令牌；产物文件在 `AUTOBID_EXPORT_DIR`，默认 `data/exports`，按租户 / 任务隔离且不入 git）；章节图表 `proposal_charts` 属域 C（绑定章节修订）。
    5. 域 C（Phase 5 已建）：`requirement_manifests`（任务级清单 epoch / 确认）、`requirement_items`、`proposal_outlines`、`proposal_sections`、`proposal_section_requirements`（复合外键保证同租户同任务）、`section_revisions`（正文不可篡改触发器）。要求实质变更或原文分块变更 → 清单 epoch 递增并把 approved / pending_approval 大纲置为 superseded（Go 层随即派生 version+1 草案）；`retrieval_evidence` 变 `stale` → 引用它的修订 / 章节降级为 `stale_evidence` 并撤销批准。锁顺序统一为「大纲行 → 章节行」。Phase 6A 的 `compliance_matrix`（绑定 `section_rev_id` + `rev_no` + 段落锚点，只能挂在已有要求映射上）与 `consistency_runs` / `consistency_findings`（与 `explorations/findings` 隔离）同属本域：要求变更、章节 `current_rev_no` 变化、证据 stale、大纲 superseded 由触发器置 stale，写入守卫拒绝把旧正文结论写到新修订；核验同样按「批准大纲 FOR SHARE → 章节 FOR SHARE」加锁。图表与导出记录使用独立业务表（`proposal_charts` / `export_jobs`），严禁混入 `assets` 表。
  - **检索增强存储（已建表）**：域 D 的 `knowledge_indexes` / `knowledge_embeddings` 保存版本化索引与派生上下文；域 B 的 `retrieval_runs` / `retrieval_jobs` / `retrieval_evidence` 保存检索运行、子任务状态、证据引用及核验结果。正式要求清单与章节引用绑定仍归域 C。索引由 `knowledge_catalogs.epoch` 管理来源失效；模型切换必须重建后激活。
- `ingest/`：`Parse` 多格式解析（`docx.go`、`pdf*.go` 自研 PDF 对象层/字体/版面表格还原（有框线表格按框线网格还原单元格，见 `pdfruled.go`；无框线表格按文字列对齐推断）、`ocr.go` 可插拔 OCR、`spreadsheet.go` XLSX/CSV/TSV、`archive.go` ZIP 清单与边界校验）+ 行级切分核心 `SplitLines`（纯文本 `Split` 规则不变）+ 入库 `Ingest` / `IngestKnowledge`；表格类文档 `Sections()` 按工作表 → 行窗口切分，来源定位在 `Table.SourceLocator`；DOCX 另读出 TOC 域缓存条目（`Parsed.TOC`），`ReadPDFLayout` 逐页返回原始版面行与图片外接框（导出版式自检用）；`ingest/ingesttest` 提供内存 `.docx` / `.pdf` 样本构造器供测试复用。
- `bidtools/`：面向招投标业务的 Worker 常驻工具、延迟工具（Deferred Tools）与领域技能（Skills）；知识库工具 `kb_*` 经 `knowledge-reuse` 技能解锁，结果必须携带 `citation`；`proposal-review` 技能解锁 `compliance_verify` / `fulltext_consistency_check`。
- `filework/`：F1 任务招标原件内容寻址存储（`Store.Put/PutBytes/Read/Cleanup`）；物理对象按租户共享，登记行按任务；`relative_path` 只由服务端生成，读取时重算 SHA-256；不访问企业证据目录。
- `export/`：Phase 6B 图表与导出（访问 `db`，不直接写 SQL）：`chart.go` 结构化规格 → Mermaid / 设备表 JSON；`render.go` mmdc 渲染、`office.go` + `office_refresh.py` LibreOffice 探测与 UNO 刷新（独立 profile、并发上限）、`proc.go` 超时杀进程树；`template.go` / `default_template.go` / `templates/default.docx` 套版模板；`docx.go` WordprocessingML 生成（修改样式时注意 `firstLineChars` 优先于 `firstLine`，pPr 子元素须按 schema 排序）；`service.go` 图表生成 / 渲染、`exec.go` 导出执行（`Execute`）、`runner.go` 异步执行（`Runner`）；`verify.go` 回读自检。环境变量：`AUTOBID_EXPORT_DIR`、`AUTOBID_SOFFICE_PATH`（缺省查 PATH 与 `F:\LibreOffice\program\soffice.exe`）、`AUTOBID_SOFFICE_PYTHON`、`AUTOBID_SOFFICE_CONCURRENCY`、`AUTOBID_SOFFICE_TIMEOUT_SECONDS`、`AUTOBID_MMDC_PATH`、`AUTOBID_EXPORT_WORKERS`。
- `compliance/`：Phase 6A 纯规则层（不访问数据库）：`Verifier` 接口与 `RuleVerifier` / 可选 `LLMVerifier`，数值 / 中文数字 / 单位归一，关键承诺抽取与比对；加锁、落库与失效门禁全部在 `db/`。
- `agent/`：Planner（主编审）与 Worker（执行单元）编排（注：Digest 是推演图冷节点折叠机制，非独立常驻 Agent；Worker 的 `activityEmitter` 目前推送 `tool`、`tool_error`、`text` 最终摘要三类事件）。
- `intercept/` & `guard/`：**工具调用级**拦截器与 Hook 适配层。
  - `Guard` 仅拦截 `PreToolUse` / `PostToolUse`，`Guard.Stop()` 恒返回不干预；
  - `intercept_pending` 受执行 Context 与 `waiter_until` 心跳约束，仅用于短时在途工具审批，**不可替代跨重启、跨天的大纲/章节持久化业务审批状态机**。
- `server/`：REST API 路由（`server/http.go`；Phase 4 的 multipart 文档上传与 `/api/knowledge/*` 在 `server/knowledge.go`；F1 的原件留存、ZIP 清单/子文件登记在 `server/knowledge.go` 与 `server/file_processing.go`；Phase 5 要求 / 大纲 / 章节在 `server/proposal.go`；Phase 6A 的 `/api/tasks/{id}/compliance/*`、`/consistency/*`、`/export-readiness` 在 `server/compliance.go`；Phase 6B 的 `/api/tasks/{id}/charts/*` 与 `/api/export/status` 在 `server/charts.go`，`/exports/*` 与 `/api/export/templates*` 在 `server/export.go`，均由 `server/http.go` 注册）、单任务 SSE 实时流（`GET /api/tasks/{id}/events`）与 Bearer Token 鉴权中间件；`APIConfig.OCR` / `APIConfig.Embedder` 由 `cmd/server` 以 `ingest.DefaultOCR()` / `db.DefaultEmbedder()` 注入；`APIConfig.FileStore` 在 `AUTOBID_FILE_PROCESSING_ENABLED=1` 时由 `cmd/server` 创建（`AUTOBID_FILEWORK_DIR`，默认 `data/filework`），为 nil 时保持旧上传行为。旧 `POST /api/tasks/{id}/documents` 对 `.zip` 返回 202 清单并登记子文件；解析失败响应带 `original_file_id`。
- `web/`：React 19 + TypeScript + Vite + Tailwind CSS v4 前端工作台（`npm run build` 产物 `web/dist` 由服务端按 `AUTOBID_WEB_DIR` 托管；开发时 `npm run dev` 代理 `/api` 到 `127.0.0.1:8080`）。F1 文档页显示原件留存/修订、表格工作表与区域，要求页显示表格来源区域和“原文已变更 · 待重新提取”状态；ZIP 上传结果只展示清单与子文件登记，不伪装成正式文档。
  - 2026-10-29 前端重构（E 级）：页面地址集中在 `web/src/lib/routes.ts`（纯函数，含旧地址兼容）与 `lib/router.ts`（离开保护 `registerLeaveGuard`）；项目页为 `/tasks/:id/{overview,documents,requirements,research,outline,writing[/:sectionId],matrix,consistency,report,export,monitor,elements,approvals,llm}`。框架在 `components/layout/AppShell.tsx`，公共组件与设计变量在 `components/ui.tsx` / `index.css`；任务上下文、SSE 与资源失效矩阵在 `features/task-workspace/`，业务页面在 `features/proposal/*` 与 `features/execution/`。纯逻辑单测：`npm --prefix web run test:unit`（Node 内置 `node --test`，测试文件用 `.ts` 后缀导入）。视觉 fixture 只在开发服务器的 `/visual.html` 提供，不进入 `web/dist`。
- **检索增强模块**：`retrieval/` 已统一召回、融合、重排、原文分页、预算、缓存、降级与研究协调；`eval/retrieval/` / `cmd/retrieval-eval/` 保存评测清单、执行器和实测报告。真实运行/证据/索引/任务检索 API 在 `server/retrieval.go` 实现，详情及限制见 [eval/retrieval/README.md](eval/retrieval/README.md)。

---

## 3. 核心工程约束（必须遵守）

模型设置按 [`docs/MODEL_SETTINGS_PLAN.md`](./docs/MODEL_SETTINGS_PLAN.md) 实施。生成模型凭据支持环境变量或服务端 AES-256-GCM 加密保存；主密钥留在进程环境/被忽略的本地文件，接口只回显脱敏提示。此项替代旧“生成模型只能填写环境变量名”的限制；embedding/reranker 仍按第 4 节管理。

1. **按功能适用层级扎实落地，严禁仅改 Prompt 充当产品特性**：
   - 实施 [`ROADMAP.md`](./ROADMAP.md) 中的任一阶段时，必须按该功能的实际架构落点补齐对应的 PostgreSQL 表结构（`db/schema.sql`）、数据访问层（`db/`）、业务状态机/工具（`bidtools/`）、API 路由（`server/`）或前端视图（`web/`），切勿仅靠修改 Prompt 宣称功能完成，也无需为纯展示功能强行堆砌无用的表或拦截规则。
2. **多租户与并发安全前置设计**：
   - Phase 4 ~ 6 新增的所有业务表必须预留 `tenant_id TEXT NOT NULL DEFAULT 'default'` 及操作人字段（`created_by`, `updated_by`, `approved_by`），唯一索引须包含 `tenant_id`。
   - 涉及大纲审批与章节正文修订时，必须实现 PostgreSQL 持久化审批状态机，并在派发入口与在途 Worker 写回双端同时校验 `based_on_outline_version` 与 `base_rev_no` 乐观锁。
3. **可执行验证与进度同步**：
   - Go 后端改动必须通过 `go vet ./...` 与 `go test -race ./...`；涉及数据库层的改动必须配置 `AUTOBID_TEST_DATABASE_URL` 连接真实 PostgreSQL 执行集成测试（避免 `openTestDB` 因未设置环境变量而 `t.Skip`）。
   - 前端改动（`web/`）必须通过 `npm run build` 零错误且与真实后端路由契约严格对齐。
   - 完成某个阶段并验收通过后，同步更新 [`ROADMAP.md`](./ROADMAP.md) 中的 `[x]` 状态。
4. **招标原文溯源与解析修订（F1 起）**：
   - 表格类文档不得伪造物理页码：`tender_documents.page_count` 与分块 `page_start/page_end` 为 NULL，`bbox_meta.page_is_physical=false`；来源写入 `{kind:"spreadsheet", file_id, sha256, sheet, range}`，不得把占位页码当证明材料页码核验。
   - 检索失效触发器只能对内容列或 DELETE 触发；关联原件、`parse_revision`、`page_count`、`parse_meta`、`token_estimate`、`bbox_meta` 更新不得使证据失效。
   - 新文档、内容实质变化或采用解析修订必须推进 `requirement_manifests.source_epoch`；`source_epoch > extracted_source_epoch` 时确认与导出均阻断，不得用清单状态覆盖该门禁。
   - `AdoptTenderParseRevision` 保持文档 ID、分块 ID 与原始 `content_hash` 不变；快照写入后不可修改；禁止用新文档行替代旧行。

## 4. 检索增强实施约束（Phase 4.1 ~ 4.4）

1. **先评测，再增强，再决定协作范围**：依次落实业务基线、语义混合检索、证据核验/单 Agent 补查、多 Agent 对比。默认复用现有 PostgreSQL 和 Planner/Worker；不要为每个检索步骤创建一个 Agent，也不要把现有双图当成 GraphRAG 索引。详细交付、证据来源、消融组与验收清单以 `ROADMAP.md` 第四节及其引用的 [`docs/archive/ROADMAP-2026-10-28.md`](./docs/archive/ROADMAP-2026-10-28.md) 为准；业务评测样本取自 D0 语料。
2. **模型与索引一致性**：通过既有 `Embedder` 接口接入服务，记录模型修订、维度、预处理和索引版本。新旧索引并存，重建校验后切换，允许回滚；不得只修改 `DefaultEmbedder()` 使旧数据静默退出召回。缓存/并发去重包含租户、权限、过滤条件与索引/策略版本，来源或权限变化须失效。Phase 7 前默认租户仍由服务端固定。托管模型（如百炼）无法固定权重修订：`REVISION` 填配置日期，怀疑服务端更新了模型时修改 `REVISION` 并重建索引。非对称模型的检索查询走 `db.EmbedQuery`。API Key 只放在被 Git 忽略的 `.env.local.bat` 或进程环境中，不写进仓库、测试或报告。
3. **检索相关性与业务正确性分开**：RRF/reranker 用于候选排序；原文引用、产品型号/版本/有效期、单位/阈值与适用条件必须另行核验。LLM 派生上下文与原文分离，历史承诺不等于当前能力。模型故障返回带原因的降级状态；缺失、冲突和不可可靠解析的证据不得判满足。
4. **完整性依赖全量要求**：Phase 5 从文档遍历及复核形成完整要求清单，检索只为每条要求找证据；不能用 Top-K 代替清单。4.3 先使用查询 ID 和版本化原文定位，5/6 再绑定要求、章节修订及响应矩阵；来源失效须传播为 `stale` 并参与导出门禁。
5. **预算与依赖通过代码和持久化状态实现**：单 Agent 与多 Agent 共用整次运行的预算，初始最多 2 轮补查，多 Agent 初始最多 3 个并行 Worker；具体值受评测配置约束。`Frontier` 不检查前置依赖，必须补齐证据子任务终态检查、幂等汇总和重启恢复；部分失败只能产生明确缺口的部分结果，不能宣称完整通过。不得把 `intercept` 当作上述业务状态机。
6. **门槛与成绩分开记录**：至少 200 条人工标注业务问题，按项目/文档家族隔离调参与保留集；真实链路报告 Recall@20、nDCG@10、完整证据召回、引用支持、无答案识别、完整正确任务率、P95 和每个正确任务成本。95% 关键证据召回与验收样本中 0 强制条款误判/0 跨租户泄漏是目标，非已达成结果。外部研究的提升幅度不得写成本项目成绩。
7. **阶段依赖与启用边界**：Phase 5 表结构/审批设计可独立推进，基于知识库的自动写标启用前须完成 4.3；4.4 可保持关闭。分别记录功能实现、评测完成和生产启用，只有证据表明多 Agent 在目标复杂问题上稳定受益且成本达标才启用相应路由。

## 5. Phase 4 增强工程验收边界（2026-10-28）

已实现工程与测试记录见 `eval/retrieval/reports/engineering-checks.md`。用户确认没有真实语料；语义模型只有百炼托管服务的临时额度（2026-10-29 起，不是自部署服务），因此业务质量、生产规模/成本、E/D 优势未验收。自动核验只覆盖明确型号的标量属性；条件/复杂语义、招标与资信证据须复核。研究恢复是重发原请求/幂等键，20 秒租约过期后接管，并非后台自动恢复；原截止时间和总预算不延长。生产多 Agent 开关 `AUTOBID_RETRIEVAL_MULTI_AGENT` 默认 false。
