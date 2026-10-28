# AutoBid Engine — 产品演进路线图与工程规范 (ROADMAP)

> **定位说明**：`autobid-engine` 的终极目标是构建一套完整的**企业级 AI 技术标书编写与合规审查系统**。
> 本路线图基于当前真实源码基线（PostgreSQL 双图存储 + Planner/Worker 协同编排 + 招投标初审工具集），将完整“AI 写技术标书系统”所需的 **10 项核心产品能力缺口** 转化为边界清晰、契约明确、可独立验收的阶段性工程规范（Phase 3 ~ Phase 7）。

---

## 一、 当前真实代码基线 (Verified Baseline: Phase 1 & Phase 2)

> **注意**：后续阶段开发必须基于以下**真实已实现能力**，严禁将尚未实现的特性（如前置任务依赖门禁、跨重启长周期业务审批、会话级自动压缩）误判为现成底座。

### 1. 已实现能力
- [x] **Phase 1：PostgreSQL 双图存储与多智能体协同内核**
  - **存储与迁移（`db/`）**：基于 `pgx/v5` 连接 **PostgreSQL**；通过 `//go:embed schema.sql` 与 `schema_version` SHA-256 校验和加 `pg_advisory_xact_lock` 事务锁执行幂等迁移（`db.Migrate`）。
  - **两类核心图结构**：
    - **招标静态要素图（`assets` + `entity_edges`）**：存储跨任务共享的招标文件静态要素（`tender`、`lot`、`clause`、`qualification`、`scoring_item`、`commercial_item`）及其有向关系。
    - **单任务推演图（`explorations` + `exploration_nodes` + `exploration_edges` + `exploration_anchors`）**：管理 `begin`、`goal`、`intent`、`fact`、`finding`、`hint`、`digest` 节点及租约争抢（`ClaimIntentWithLease`）。
  - **结论产出物（`findings`）**：存储结构化审查结论（`compliance`、`risk`、`scoring`、`pricing`、`strategy`）及证据链。
  - **冷节点折叠与有界概览（`db/digest.go`, `db/overview.go`）**：支持推演图冷节点折叠为 `digest` 节点及生成有界全局概览（注：`internal/harness` 负责会话循环与工具调度，尚未实现单 Worker 长会话的自动上下文压缩）。
  - **工具级拦截与审计（`intercept/`, `guard/`）**：`Guard` 挂载于 `PreToolUse` / `PostToolUse`，对启用拦截的**在途工具调用**执行规则/Judge/人工 `ask` 判定并记录 `intercept_pending` / `intercept_audit`。
  - **HTTP / SSE 控制面（`server/http.go`）**：提供任务生命周期、单工作槽控制、推演图查询、LLM 配额链、工具审批及单任务实时活动流 `GET /api/tasks/{id}/events`。
- [x] **Phase 2：招标文本切分、企业资信库与初审工具集**
  - **文本切分与入库（`ingest/splitter.go`, `db/documents.go`）**：接收纯文本/Markdown 招标文件，按章节/条款切分为 `tender_documents` 与 `document_chunks`，识别 `★`/`*`/废标/否决等强制标记。
  - **企业资信库（`company_credentials`）**：存储企业资质、业绩与人员结构化档案，支持按类别、关键词、有效期检索及按 `(category, code)` 幂等更新。
  - **标书核验工具与技能（`bidtools/`）**：
    - 常驻工具：`document_outline`、`search_document`、`read_chunk`；
    - 延迟工具（经 Skill 解锁）：`query_company_credentials`、`simulate_scoring_table`、`verify_commercial_bid`；
  - **结构化推演报告（`db/report.go`）**：汇总覆盖率（`TaskCoverageWithSources`）、招标要素核验状态与证据链（`GET /api/tasks/{id}/report`，返回 JSON）。

### 2. 现有基座的关键边界（后续阶段必须显式补齐）
1. **无自动 DAG 前置依赖门禁**：`ExplorationStore.Frontier` 仅按 `kind='intent' AND state='open'` 及 `priority DESC, id ASC` 选取任务，不检查前置节点是否完成。Phase 5 的依赖章节调度必须在调度层或状态机中显式实现前置条件检查。
2. **`Guard.Stop()` 不做完成度拦截**：`guard/guard.go` 的 `Stop()` 恒返回不干预，仅负责工具调用拦截与计数。实质性条款与必答项的完成度门禁必须在业务状态机、Planner 收敛条件与导出门禁中显式实现。
3. **`intercept` 仅适用于短时工具调用拦截，不可直接充当长周期业务审批**：`intercept_pending` 依赖执行 Context 与 `waiter_until` 心跳，进程重启或等待方取消即被回收为 `denied`。大纲确认、章节定稿等跨小时/跨天业务审批必须设计为**独立的 PostgreSQL 持久化业务状态机**。
4. **Phase 4 完成的是检索功能基线，尚未通过业务检索质量验收**：默认 `HashEmbedder` 是 256 维词袋哈希，不是学习语义的 embedding 模型；`ts_rank_cd` 不是 BM25。现已实现 RRF、可配置 HTTP 语义重排、版本化索引切换、持久化证据与有预算的检索协调；真实模型服务尚未部署，多 Agent 默认关闭。返回 `citation` 不代表引用支持结论，现有样例命中测试也不代表真实业务 Recall 达标。增强工作按下文 Phase 4.1 ~ 4.4 独立验收。

---

## 二、 四大核心数据域边界与多租户预留规范

为防止将标书正文、图表或知识库错误混入招标静态要素表（`assets`），全系统严格划分以下 **四大核心数据域**：

1. **域 A — 招标静态要素域（现有 `assets` + `entity_edges` + `tender_documents` + `document_chunks`）**
   - 职责：只存从招标文件中解析出的原始段落与静态要素（`tender`、`lot`、`clause`、`qualification`、`scoring_item`、`commercial_item`）。
2. **域 B — 核验推演与分析结论域（现有 `explorations` + `exploration_nodes` + `findings`）**
   - 职责：存储任务推演过程（`intent`/`fact`/`digest`）及核验审查结论（`findings`）。
   - Phase 4.2 ~ 4.4 规划：检索运行、子任务状态、证据引用集合与核验结果归此域；证据通过来源标识引用域 A/D 原文及企业资信，不把检索摘要当成新的知识原文。
3. **域 C — 投标要求清单、标书正文与响应矩阵域（Phase 5 ~ 6 新增表）**
   - 职责：存储经复核的完整投标要求清单（`requirement_items`）、技术标大纲（`proposal_outlines` / `proposal_sections`）、章节多版本正文（`section_revisions`）与点对点应答矩阵（`compliance_matrix`）。
4. **域 D — 企业知识库与成稿附件域（Phase 4 & Phase 6 新增表）**
   - 职责：存储企业历史方案/产品知识库（`knowledge_documents` / `knowledge_chunks`）、标书图表产物（`proposal_charts`）及排版导出记录（`export_jobs`）。
   - Phase 4.2 规划：知识库的版本化 embedding、索引构建记录及派生检索上下文归此域；LLM 生成的上下文必须与可引用原文分离。

> **提前预留归属与多租户隔离（Phase 4 ~ 6 建表强制规范）**：
> 虽然完整 RBAC 在 Phase 7 落地，但 **Phase 4 ~ 6 新增的所有业务表及对 `company_credentials` / `assets` 的扩展，必须提前包含 `tenant_id TEXT NOT NULL DEFAULT 'default'` 以及操作人审计字段（`created_by`, `updated_by`, `approved_by`）**，且业务唯一索引必须将 `tenant_id` 纳入联合唯一键，避免 Phase 7 推倒重建表结构。

---

## 三、 10 大产品能力缺口与阶段映射总表

| 缺口编号 | 用户侧目标能力 | 当前真实状态 | 目标阶段 | 核心工程落点（PostgreSQL Schema / Tools / API / 状态机） |
| :--- | :--- | :--- | :--- | :--- |
| **#9** | **投标人员使用的网页工作台** | ✅ 已实现（Phase 3）：`web/` 工作台 + 资信按 ID 更新/删除、报告 Markdown 导出、静态托管 | **Phase 3** | 新增 `web/` 单团队审查工作台（对接真实 `/api/tasks/{id}/*` 路由）；本阶段同步补齐企业资料按 ID 全量更新/删除与报告 Markdown 导出等配套能力 |
| **#1** | **上传 PDF、Word、扫描件并准确解析** | ✅ 已实现（Phase 4）：multipart 上传 `.docx`/`.pdf`/图片/文本，标题层级、页码范围、跨页表格 JSON + Markdown、版面溯源、可插拔 OCR | **Phase 4** | 扩展 `ingest/` 与文档上传接口：支持 `multipart/form-data` 上传 `.docx`/`.pdf`/扫描件、OCR 接口、跨页表格结构化还原（Markdown + JSON）及页码/坐标溯源 |
| **#4** | **复用企业方案、历史标书、产品资料** | ✅ Phase 4 功能基线已实现；增强工程已落地；真实模型部署、业务评测与启用验收待完成 | **Phase 4 / 4.1 ~ 4.4** | 保留 `knowledge_documents` / `knowledge_chunks`、`kb_*` 工具与管理 API；依次补充业务评测、共享混合检索服务、版本化索引、证据核验、有界协作；`pgvector` / HNSW 按部署条件和实测选择 |
| **#2** | **根据评分标准生成技术标目录** | 无要求清单汇总表，无大纲结构与持久化确认流程 | **Phase 5** | 新增 `requirement_items`（可复核要求清单）与 `proposal_outlines` / `proposal_sections` 表；实现版本化大纲持久化审批状态机（非 `intercept` 临时挂起） |
| **#3** | **根据项目需求编写技术方案章节** | 无章节写作工作流，无前置章节依赖门禁 | **Phase 5** | 后端写作派发入口与在途写回双端强制校验大纲已批准版本（`based_on_outline_version`）；实现章节依赖调度门禁；新增 `section_write_draft` / `section_read_context` 工具 |
| **#5** | **管理章节草稿、修订、人工确认** | 无正文版本表、无并发写冲突保护与审批失效规则 | **Phase 5** | 新增 `section_revisions` 表；实现 `base_rev_no` + `based_on_outline_version` 联合并发校验、人工/AI 写冲突保护、绑定具体版本的章节批准及重写自动撤销批准规则 |
| **#7** | **生成架构图、进度表及其他标书图表** | 未实现图表生成与章节挂载 | **Phase 6** | 新增 `proposal_charts` 表与 `chart_generate` 工具（生成 Mermaid/SVG 架构图、甘特图、组织架构图、设备选型表），绑定至具体章节版本 |
| **#6** | **检查全文前后一致性与逐条响应** | 无点对点应答表，无跨章节一致性核验与失效重检机制 | **Phase 6** | 新增 `compliance_matrix` 表与 `fulltext_consistency_check` 工具；分别验收“大纲章节映射率”与“正文实质响应率”，条款或正文变更自动触发重检 |
| **#8** | **套模板、生成目录页码、导出 DOCX/PDF** | `GET /api/tasks/{id}/report` 仅返回审查 JSON | **Phase 6** | 新增 `export/` 模块与 `export_jobs` 表：锁定已批准章节版本快照，套用 `.docx` 模板，渲染图表、多级标题、TOC 目录域与页码，导出 `.docx`/`.pdf` |
| **#10** | **多企业、多用户和角色权限** | 仅支持 `server/http.go` 可选单一 Bearer Token | **Phase 7** | 新增 `tenants`、`users`、`project_members` 表；启用全表 `tenant_id` 强制隔离与四大角色（投标经理/技术撰写人/商务专员/审核专家）RBAC 鉴权 |

---

## 四、 分阶段实施规范与可执行验收标准 (Phased Execution Plan)

### Phase 3：单团队标书合规审查工作台 (Web Console) — 对应缺口 #9
- **状态**：`[x] 已完成（2026-10-28）`
- **交付记录**：
  - 后端：`db.UpdateCompanyCredential` / `db.DeleteCompanyCredential` 与 `PUT/DELETE /api/company/credentials/{id}`（attributes 整体替换；不存在 404，`(category, code)` 冲突 409）；`GET /api/tasks/{id}/report?format=markdown`（`db/report_markdown.go`）；`server/static.go` 托管 `AUTOBID_WEB_DIR`（默认 `web/dist`）并做 SPA 回退（`/api`、`/healthz` 不回退）。
  - 前端：`web/`（React 19 + TypeScript + Vite + Tailwind CSS v4 + lucide-react）：令牌设置与 401 提示、健康状态栏、任务生命周期、文档导入与章节目录树、推演态势与工作槽控制、SSE 活动流（断线重连后重拉 overview / intercepts）、工具审批面板与审计、要素与结论、审查报告（JSON / Markdown 下载）、企业资信库增删改查、LLM 配置/绑定/配额链/用量，以及 Phase 4~6 规划占位页。
  - 验证：`go vet ./...` 与 `go test ./...`（`AUTOBID_TEST_DATABASE_URL` 指向真实 PostgreSQL）全部通过；`cd web && npm run build` 零错误；本地联调覆盖令牌鉴权、任务创建、文档导入、SSE 断线重连恢复与资信原位编辑。注：当前 Windows 环境无 cgo/gcc，`go test -race` 未能执行。
- **阶段边界定位**：先交付**真实可用的单团队招标审查工作台**，100% 对接 Phase 1 ~ 2 已实现的后端能力，并为 Phase 4 ~ 6（知识库、大纲与写标、成稿导出）预留清晰的导航扩展位（标注规划阶段，不伪造未实现的后端假功能）。
- **接口契约区分**：
  1. **直接对接的现有接口（`server/http.go`）**：
     - 任务与工作槽：`GET/POST /api/tasks`、`GET/DELETE /api/tasks/{id}`、`POST /api/tasks/{id}/{start,pause,resume,revive}`、`GET /api/tasks/{id}/coverage`、`POST /api/tasks/{id}/{goals,hints}`、`POST /api/tasks/{id}/works/{intentID}/{control,steer,message}`。
     - 推演图与结论：`GET /api/tasks/{id}/overview`、`GET /api/tasks/{id}/nodes/{nodeID}`、`GET /api/tasks/{id}/findings`、`GET /api/tasks/{id}/assets`、`GET /api/assets/{assetID}/edges`、`GET /api/tasks/{id}/report`。
     - 文档与企业资信：`POST /api/tasks/{id}/documents`（当前接收 JSON `{tender_no, lot_no, title, text, wake_planner}`，前端可支持粘贴文本或读取本地 `.txt`/`.md` 文件上传）、`GET /api/tasks/{id}/documents?content=1`、`GET/POST /api/company/credentials`。
     - 审批与 LLM 配置：`GET /api/intercepts`、`POST /api/intercepts/{pendingID}/decide`、`GET /api/tasks/{id}/intercept-audit`、`GET/POST/PUT/DELETE /api/llm/profiles...`、`GET /api/llm/health`。
     - 单任务实时事件流：`GET /api/tasks/{id}/events`（开启鉴权时支持 `?token=`；前端解析 `event: activity` 展示工具调用 `tool`、工具错误 `tool_error`、阶段结论文本 `text` 与审批请求 `intercept_request`，并处理 `event: dropped` 与断线重连后重新拉取 `overview`/`intercepts` 状态恢复）。
  2. **本阶段同步补充的轻量后端能力**：
     - 在 `db/documents.go` 与 `server/http.go` 补充按主键 ID 的全量更新接口 `PUT /api/company/credentials/{id}`（支持修改任意记录的 `category`、`code`、`title`、`valid_until`，并对 `attributes` 执行完整 JSONB 替换而非增量合并，以支持删除旧属性或编辑无编号记录；若新 `(category, code)` 冲突则返回 409）与删除接口 `DELETE /api/company/credentials/{id}`，真正闭环企业资信库增删改查。
     - 支持审查报告导出为 Markdown（可在 `GET /api/tasks/{id}/report?format=markdown` 或前端基于 `TaskReport` JSON 生成标准 Markdown/JSON 下载）。
     - 在 `cmd/server` 或 `server/` 支持托管 `web/dist` 静态产物（带 SPA History 路由回退）。
- **可执行验收标准**：
  - `cd web && npm run build` 零 TypeScript 与打包错误；
  - 后端新增接口通过 `go vet ./...` 与 `go test ./...`（配置 `AUTOBID_TEST_DATABASE_URL` 时数据库测试与 HTTP 测试全部通过）；
  - 真实联调验证：支持 Bearer Token 登录/鉴权、任务创建与文档导入、SSE 断线重连后状态恢复、`intercept` 审批弹窗放行/拒绝闭环、接口报错时的明确错误提示。

---

### Phase 4：多格式文档深度解析与企业方案知识库 (Ingest & PostgreSQL RAG) — 对应缺口 #1、#4
- **状态**：`[x] 基础交付已完成（2026-10-28）`；下述 Phase 4.1 ~ 4.4 已交付可运行工程，真实业务质量验收待完成；原完成记录不代表新增质量门槛已通过。
- **交付记录**：
  - **Schema（`db/schema.sql` 幂等 DDL）**：`tender_documents` 补 `tenant_id`/`file_name`/`mime_type`/`page_count`/`parse_meta`/`created_by`，幂等唯一键改为 `(tenant_id, task_id, tender_no, lot_no, content_hash)`；`document_chunks` 补 `page_start`/`page_end`/`tables_json`/`bbox_meta`（旧行以 `page_no` 回填）。新增域 D `knowledge_documents`（4 类类别、`tags TEXT[]` GIN、`(tenant_id, content_hash)` 去重、`created_by`/`updated_by`）与 `knowledge_chunks`（`section_path`、页码范围、`tables_json`、`search_tsv` 生成列 + GIN、`embedding float8[]` + `embedding_model`，唯一键 `(tenant_id, doc_id, seq)`）及 `kb_cosine()` 余弦函数。
  - **解析器（`ingest/`）**：`Parse` 按扩展名/MIME/文件头分派；`docx.go`（标题样式/大纲级别 → 层级、`lastRenderedPageBreak`/显式分页计页、`gridSpan`/`vMerge` 合并单元格、`tblHeader` 重复表头、嵌套表格展平）；`pdfobj.go`/`pdffont.go`/`pdf.go` 为自研 PDF 解析（xref 表/流、对象流、Flate/PNG 预测器、xref 损坏时全文扫描重建；Type0 ToUnicode/`/W` 字宽；内容流解释 → 版面行 → 列段 → 表格还原，首列纵向合并、单元格折行、跨页续表合并并去重复表头、页码与页眉页脚剔除，无文本层页面取图像送 OCR）；Markdown `#` 标题与管道表格；非 UTF-8 文本按 GB18030 解码；`ocr.go` 定义 `OCRProvider`（`HTTPOCR` / `TesseractOCR` / `FallbackOCR` / `UnavailableOCR` 兜底，`DefaultOCR()` 读 `AUTOBID_OCR_URL` / `AUTOBID_TESSERACT` / `AUTOBID_OCR_LANG`）；切分核心改为行级 `SplitLines`，段落携带页码范围、表格与坐标。
  - **知识库（`db/knowledge.go`, `db/kbtext.go`）**：入库（同租户内容哈希去重）、列表（类别/标签/标题筛选）、详情、按 ID 删除（分块级联）、分块上下文窗口；混合检索 = `search_tsv` 全文召回（中文单字+双字词元，`ts_rank_cd`）+ 向量召回（默认本地 `HashEmbedder` 哈希词袋向量，`Embedder` 接口可替换为外部模型）+ 关键词命中率，加权融合（0.5/0.3/0.2，参数表加分），产品名作为必含过滤；所有命中附 `KnowledgeCitation`（文档标题、类别、文件名、章节号/路径、页码范围、seq）。
  - **工具（`bidtools/knowledge.go`）**：延迟工具 `kb_search_proposals`（历史标书/标准方案，关键词 + 行业标签）、`kb_search_products`（产品规格书/白皮书，产品名必含 + 指标关键词，表格优先）、`kb_read_chunk`（文档 ID + 分块序号读取上下文窗口与完整表格 JSON），经新技能 `knowledge-reuse` 解锁，返回强制附带 `citation` 与一行 `cite`；`document_outline` / `read_chunk` 同步输出页码范围与表格。
  - **API（`server/knowledge.go`）**：`POST /api/tasks/{id}/documents` 兼容原 JSON 并支持 `multipart/form-data`（64 MiB，`.docx`/`.pdf`/`.txt`/`.md`/图片；不支持格式 415，扫描件无 OCR 422）；`GET/POST /api/knowledge/documents`、`GET/DELETE /api/knowledge/documents/{id}`、`GET /api/knowledge/search`；`X-Autobid-User` 记录为 `created_by`，Phase 7 前固定默认租户。
  - **前端（`web/`）**：新增「企业方案知识库」页面（上传文件/文本、类别/标签/标题筛选、详情含合并单元格表格渲染、删除、混合检索预览与分项得分），导航替换 Phase 4 占位；任务「招标文档」页支持选择本地 `.docx`/`.pdf`/扫描件上传，展示页码范围、跨页表格、解析器与告警。
  - **验证**：固定样本（`ingest/ingesttest` 内存构造含跨页表格、合并单元格与 `★` 条款的 `.docx` / `.pdf` / 扫描件 PDF）单元测试覆盖章节层级、表格 JSON/Markdown、页码范围、xref 重建、OCR 兜底；真实 PostgreSQL 集成测试覆盖入库去重、混合检索命中（全文/标签/产品必含/向量召回）、引用完整性、租户隔离与 HTTP 契约。`go vet ./...`、`go test ./...`（`AUTOBID_TEST_DATABASE_URL`）与 `cd web && npm run build` 全部通过，并在本地服务上完成知识库上传/检索与任务文档上传的浏览器联调。注：当前 Windows 环境无 cgo/gcc，`go test -race` 未能执行；本环境未安装 tesseract，OCR 以接口桩测试验证，真实识别需配置 `AUTOBID_OCR_URL` 或安装 tesseract（`chi_sim`）。
- **交付模块**：
  1. **数据库迁移机制演进或 `db/schema.sql` 幂等扩展**：
     - 沿用 PostgreSQL（`pgx`），在 `db/schema.sql` 中以幂等 DDL 新增表（或先升级 `db.Migrate` 支持版本化 SQL + `schema.sql` 兼容），所有新表包含 `tenant_id TEXT NOT NULL DEFAULT 'default'`。
  2. **多格式文件解析与版面还原（缺口 #1）**：
     - 升级 `POST /api/tasks/{id}/documents` 支持 `multipart/form-data` 上传原始 `.docx`、`.pdf` 及图片扫描件（同时向下兼容 JSON `text` 上传）。
     - 扩展 `ingest/`：提取多级章节标题、原始页码范围（`page_start`, `page_end`），将跨页复杂表格还原为结构化 JSON + Markdown Table，扫描件接入可插拔 OCR 接口，在 `document_chunks` 中保存完整溯源坐标。
  3. **PostgreSQL 企业方案与产品知识库（缺口 #4）**：
     - 新增 `knowledge_documents`（类别：`historical_bid` / `whitepaper` / `product_spec` / `standard_solution`，含 `tenant_id`）与 `knowledge_chunks` 表。
     - 基于 PostgreSQL 原生 `tsvector` + `GIN` 索引（及可选 `pgvector` / 向量余弦相似度）实现混合检索。
     - 在 `bidtools/` 新增 `kb_search_proposals`、`kb_search_products`、`kb_read_chunk` 工具，返回结果强制附带文档名、章节号与页码引用。
- **可执行验收标准**：
  - [x] 准备固定测试样本集（含跨页表格的 `.docx` 与 `.pdf` 样本）：断言解析后章节数、表格行列结构、页码范围及 `★` 条款识别率达到预期；
  - [x] 在配置真实 PostgreSQL（`AUTOBID_TEST_DATABASE_URL`）下执行 `go test ./...`，验证知识库入库、去重、混合检索命中及引用元数据完整性（基础阶段记录；增强阶段已补跑 `-race`，见下文工程检查）。

---

### Phase 4.1 ~ 4.4：检索质量、证据核验与按需多 Agent 协作 — 对应缺口 #4，并为 Phase 5 ~ 6 提供证据底座

- **状态**：`[ ] 工程已交付，真实业务验收待完成（2026-10-28）`
- **架构决策**：共享混合检索服务 + 单 Worker 默认路径 + 按需多 Agent 搜集独立证据 + 集中核验。保留 PostgreSQL、现有 Planner/Worker 和 `kb_*` 工具接口；不以增加 Agent 数量替代召回与排序质量，也不为此重写现有编排框架。
- **实施顺序**：4.1 业务评测基线 → 4.2 语义召回与排序 → 4.3 证据核验与单 Agent 有界补查 → 4.4 选择性多 Agent 实验与上线判定。先证明每层收益，再增加复杂度。Phase 5 的表结构与审批设计可独立推进；启用基于知识库的自动写标前须完成 4.3，4.4 不是写标的强制前置条件。
- **能力边界**：Agentic Retrieval 可以由单 Agent 或确定性流程完成；并行 SQL/HTTP 子查询不等于多个 Agent。现有双图不是 GraphRAG 的知识图谱与社区索引。GraphRAG 暂不列入实施范围，待跨历史项目整体分析场景出现后另行评测。

#### 增强工程交付记录（2026-10-28）

- **实现记录与操作说明**：[eval/retrieval/README.md](eval/retrieval/README.md)；**本项目实测原始报告**：[synthetic-local.json](eval/retrieval/reports/synthetic-local.json)；**工程检查**：[engineering-checks.md](eval/retrieval/reports/engineering-checks.md)。
- [x] 4.1 工程：版本化评测清单、家族隔离校验、等价证据指标、A/B/C/D/E 执行器及人工复核指标接口；32 题合成回归明确标记 synthetic。
- [x] 4.2 工程：共享检索、HTTP 模型契约、RRF/重排、精确向量检索、索引版本/切换/回滚、降级、缓存、原文及长表格分页。
- [x] 4.3 工程：持久化运行/证据快照、限定范围的数值核验、来源 stale 传播、最多两轮补查，以及 API/知识库与任务页面。
- [x] 4.4 工程：复用 Planner/Worker 模型，最多三个独立证据 Worker，共享预算、20 秒续租、同键重启恢复、终态门禁、取消和默认关闭开关。
- [ ] **业务验收**：真实至少 200 题、人工复核、语义/重排服务、B/C/D/E 对比及目标负载/成本预算尚缺失。当前 C 为明确标识的词法降级，其合成排序指标未优于 A；不能宣称质量提升。
- **状态解释**：上面的工程复选框只表示列明的可运行能力；下面各阶段原业务验收条件继续独立判定。复杂语义仍待复核，完整性检查不替代 Phase 5/6 的全量要求清单与导出门禁。多 Agent 生产路由保持关闭。

#### Phase 4.1：真实业务评测与可复现基线

- **交付内容**：
  - 建立至少 200 条人工标注问题，来源为可使用的真实业务文档，覆盖同义表达、型号/标准号、表格/单位、否定与适用条件、跨文档多证据、过期版本、无答案场景；固定合成样例仅作回归补充，不能替代业务评测。
  - 按项目/文档家族隔离调参与保留测试集，避免同一标书及其修订版跨集合泄漏。记录语料快照、内容哈希、来源许可/可用范围、标注版本及问题类别；标注最小充分证据集合、允许的等价出处与期望判断，对争议样本复核。缺少真实语料时记录缺口，不伪称完成。
  - 新增评测入口与报告产物（建议 `eval/retrieval/`），运行真实检索链路；保存参数、模型修订、索引版本、查询轨迹、候选排名、证据 ID、耗时和成本。报告须区分解析漏失、召回漏失、排序错误与核验错误。
  - 冻结对比条件：相同语料快照、生成模型、问题集合、工具权限和总预算上限；Agent 路径每题至少重复 3 次并报告波动/置信区间。同步报告实际 token、工具调用与模型费用；吞吐/P95 测试固定硬件、文档规模、并发、冷热缓存条件。
- **消融实验**：

  | 实验组 | 配置 | 要回答的问题 |
  | :--- | :--- | :--- |
  | A | 当前全文 + HashEmbedder + 固定权重 | 真实功能与质量基线 |
  | B | A 仅替换为语义 embedding，并重建对应索引 | 语义模型是否减少漏召回 |
  | C | B + RRF + reranker + 原文/表格上下文读取 | 检索底座的总体收益；必要时逐项移除以定位贡献 |
  | D | C + 证据核验 + 单 Agent 有界补查 | 迭代检索能否补齐证据且控制误判 |
  | E | D + 按独立证据范围派发多 Agent | 额外覆盖收益是否值得协调成本 |

- **指标定义与上线目标**（目标不是当前成绩）：
  - 证据 `Recall@20`：每题前 20 个去重原始分块覆盖的已标注相关证据比例，按题宏平均；等价出处按同一证据组计数。另报多证据问题“全部必要证据均召回”的题目占比，不能用单个命中代替完整召回。
  - `nDCG@10` 使用预先标注的分级相关性；引用支持率按逐项事实主张与原文的支持关系计数；另报无答案识别的 precision/recall、完整正确任务率，避免全部拒答获得虚假的低误判率。
  - 保留测试集的关键证据 Recall@20 目标 ≥95%；强制条款“证据不足却判满足”及跨租户泄漏在验收样本中均为 0。按问题类别分别报告，注明样本量；有限测试中的 0 不代表线上永久零错误。
  - P95 延迟、吞吐、每个正确完成任务的成本必须实测。4.1 报告在调优前冻结部署硬件、目标规模/并发及数值化延迟/成本预算；条件未确定时不得宣称性能验收通过。
- **验收**：
  - [ ] 数据集、标注与保留集划分完成；基线 A 及评测运行命令可复现，且已冻结质量/性能/成本门槛。
  - [ ] 交付分层错误分析；外部研究数字与本项目实测分开记录。

#### Phase 4.2：共享混合检索与版本化索引

- **检索流水线**：服务端租户/权限/来源过滤 → 精确词法与语义向量独立召回 → RRF 融合去重 → reranker → 按预算读取章节上下文与完整表格。型号、标准号、数值等精确约束不得被语义改写丢失。
- **模型与查询**：
  - 复用 `Embedder` 接口，通过 HTTP 接入语义模型；首轮可评测 `Qwen3-Embedding-0.6B` 与 `Qwen3-Reranker-0.6B`，版本固定且可配置。模型是可部署候选，不预先认定其质量或速度最优。
  - 保留中文词元全文检索作为基线，评测领域词典、同义词/缩写与查询改写；不得把未经切词的中文直接送入 PostgreSQL 默认分词，也不得把现有 `ts_rank_cd` 写成 BM25。
  - RRF 按各路排名融合，不能把融合分或 reranker 分直接解释为合规概率。初始候选参数为每路 50 条、融合后重排最多 100 条、读取 8~12 条；RRF 常数可从 60 起步，均通过保留集以外的数据调优。明确评测 Top-20 候选与最终上下文集合是不同阶段。
  - 保留现有文档标题/章节路径上下文，补充表头、型号、版本与适用范围；完整表格按 token 预算读取，过大时按行分批并保留表头与单元格出处。LLM 派生上下文单独存储，不能当作原文引用。
- **存储、性能与故障处理**：
  - 域 D 建议新增 `knowledge_indexes` / `knowledge_embeddings`，记录模型修订、维度、预处理版本、内容哈希、构建状态与激活版本；同一分块可保留新旧索引，分批重建、校验后原子切换，支持回滚。禁止仅替换默认模型导致旧文档静默退出向量召回。
  - 沿用 PostgreSQL；先检查 `pgvector` 部署可用性，小规模保留精确向量检索。只有实测延迟/吞吐需要时启用 HNSW，并与精确查询对比租户/类别过滤下的召回率、候选不足和资源占用。扩展不可用时明确采用精确路径，不默认为已有 ANN 索引。
  - embedding / reranker 超时或失败时按可用渠道降级，返回 `degraded` 与具体原因；校验模型响应维度、数量与数值有效性。没有充分证据时仍返回不足，不能因降级放宽合规判断。
  - 共享缓存与并发请求去重的键包含租户、权限范围、规范化查询、硬过滤条件、模型/索引及检索策略版本；文档删除、版本切换、权限变化使相关缓存失效。Phase 7 前仍固定默认租户，不接受调用者任意指定其他租户。
- **工程交付**：新增 `retrieval/` 负责统一策略；`db/` 负责持久化与来源隔离；域 B 的 `retrieval_runs` 从本阶段记录查询、使用的索引快照、各路耗时、状态与降级原因，供 4.3/4.4 扩展。`bidtools/knowledge.go` 和 `server/knowledge.go` 调用同一服务，保持现有 `kb_*` / `/api/knowledge/search` 契约兼容。
- **验收**：
  - [ ] B/C 对比 A 的业务结果已记录，达到冻结门槛；语义召回包含同义表达而非仅词序变化样例。
  - [x] 真实 PostgreSQL 验证索引重建/切换/回滚、同模型空间约束、租户过滤、删除失效；故障注入验证降级与响应标识。
  - [ ] 完成目标规模/并发的性能与成本报告；若使用 ANN，单独报告相对精确检索的召回损失。

#### Phase 4.3：结构化证据、集中核验与单 Agent 有界补查

- **数据与返回契约**：域 B 建议新增 `retrieval_evidence`，关联 `retrieval_runs`，保存问题/查询标识、来源域、文档/分块 ID、版本或内容哈希、原文摘录、页码及其来源/可靠性、表格单元格位置、型号/适用条件/有效期，以及 `supports` / `contradicts` / `insufficient` 证据关系和核验状态。不得把匹配分等同于核验状态。
- **来源隔离**：招标要求从域 A 获取，知识库材料从域 D 获取，资信从企业资信表获取；工具边界保留各自的 task/tenant 过滤。历史标书中的承诺不能自动证明当前产品能力或当前有效资质。来源存在但不能支持结论时，仍判证据不足。
- **核验规则**：
  - 代码验证引用可访问性、内容哈希/版本、型号与文档适用范围；对已可靠提取的数值、单位及 `≥/≤` 条件执行确定性比较。否定、附加条件和复杂语义由核验模型或人工处理；无法可靠解析的条件返回不足。
  - 例：要求“恢复时间 ≤30 秒”，材料只写“快速恢复”，不得判满足；型号不同、版本过期、单位无法换算、表格列归属不清或来源冲突时必须显式报告。
  - 记录被核验的具体主张及依据；文档删除/修订、要求变更导致既有核验结果 `stale`。审计记录保留来源标识与必要的受权限控制的证据快照，失效证据不得作为当前有效依据继续使用。
  - 单 Worker 先检索并核验，仅针对缺口补查；由代码统一限制补查轮次、工具调用、token 和总时间。初始最多 2 轮补查，其他预算从 4.1 冻结配置读取；预算耗尽返回明确缺口，不声称完成。
- **跨阶段边界**：4.3 使用稳定查询 ID 与版本化原文定位，不提前假定 `requirement_items` 已存在。Phase 5 再绑定正式 `requirement_item_id`；Phase 6 绑定章节版本及响应矩阵。完整性检查遍历全部经复核要求，Top-K 仅为每条要求寻找证据，不能作为完整要求清单。
- **API / UI**：在保留旧接口的基础上，规划查询运行详情、证据读取接口（路径在实施时写入 `server/http.go` 并验证契约），返回 `run_id`、证据 ID、缺口、降级/预算状态；知识库与任务页展示原文、冲突和失败原因，不只显示一个分数。现有实现与真实路由清单见 `eval/retrieval/README.md`。
- **验收**：
  - [ ] D 对比 C 的收益与代价可复现；引用支持率、无答案识别及完整正确任务率均有报告。
  - [ ] 真实 PostgreSQL 验证证据关联、版本失效、隔离与审计；固定反例覆盖错型号、过期资料、阈值/单位、否定条件、表格歧义与无答案，达到冻结门槛。
  - [ ] 预算耗尽、模型超时和来源删除均有明确状态；API/UI 可回溯至原文，旧工具调用兼容。

#### Phase 4.4：按需多 Agent 协作与上线判定

- **路由与分工**：简单型号/参数查询默认单 Worker；只有跨来源、上下文可独立的问题才由现有 Planner 派发。方案、产品、资信 Worker 为按需角色，各自完成“检索—读原文—整理证据”，只传递结构化证据引用、必要摘录和缺口，不靠连续传话转述建立事实。
- **编排与状态**：初始最多 3 个并行 Worker、最多 2 轮补查；总预算属于整次运行，不能每个 Worker 各自扩出一份。域 B 持久化子任务领取/租约、成功/失败/取消/预算耗尽状态，复用现有调度能力并补充必要状态机；重启后按幂等键恢复，禁止重复汇总和重复计费任务无界重试。
- **汇总门禁**：不能依赖现有 `Frontier` 的优先级排序假定前置任务已完成。集中核验只能在所需证据任务到达终态后执行；部分失败时允许输出带缺口的部分结果，但禁止标记完整通过。核验器直接读取证据原文，代码承担可确定的检查，必要时再调用独立核验 Agent。
- **实验与开关**：E 与 D 使用相同底座、生成模型和总预算上限；在预先定义的复杂问题子集上比较完整正确任务率、召回、波动、延迟及每个正确任务成本。只有收益稳定且满足 4.1 冻结预算的场景才开启多 Agent 路由；简单查询保持单 Worker。收益不足则交付实验结论并保持该路由关闭，不为完成路线图强行启用。
- **验收**：
  - [x] 路由、全局预算、并发去重、租约恢复、幂等汇总与前置任务门禁通过真实 PostgreSQL 集成测试。
  - [ ] E/D 对比、适用问题类别与开关决策已归档；明确区分“功能实现”“评测完成”“生产启用”状态。

#### 各增强阶段的共同工程要求

- 新业务表均包含 `tenant_id TEXT NOT NULL DEFAULT 'default'` 与 `created_by` / `updated_by` / `approved_by`，业务唯一键纳入租户，跨表关联校验归属；审批字段预留不表示已经建立审批流程。DDL 沿用 `db/schema.sql` 幂等迁移，不假定 `db/migrations/` 生效。
- Go 实现须通过 `go vet ./...`、`go test -race ./...`；存储改动配置真实 `AUTOBID_TEST_DATABASE_URL`，不能以跳过测试验收。当前环境若不能执行 race，需在具备 cgo/编译器的环境补测后才满足该项。前端改动须 `npm run build` 并验证真实路由。
- 效果门槛、示例参数和外部论文成绩必须分别标注；完成对应交付及验收后才勾选子阶段。仅调整 Prompt、填入引用字段或通过固定样例测试，不等于完成检索质量与证据核验。

#### 决策证据与适用边界（2026-10-28 分析记录）

- **本项目源码与实测**：`db/kbtext.go` 的 `DefaultEmbedder()` 为 256 维词袋哈希；直接运行该实现，文本对“视频监控平台/监控视频平台”余弦相似度为 0.6，“容灾/灾难恢复”“并发用户/同时在线人数”“质保期/保修期限”均为 0。此结果仅说明该组件的词面特征，不能当作端到端 Recall。执行 `go test ./db -run '^TestKnowledge' -count=1 -v`，两个知识库测试通过，真实 PostgreSQL 集成测试未跳过；本次局部验证不代表完整测试套件或新质量门槛通过。
- **多查询与重排序**：[Azure Agentic Retrieval](https://learn.microsoft.com/en-us/azure/search/agentic-retrieval-overview) 提供问题分解、并行子查询、语义重排序与来源保留的设计参考；LLM 查询规划在查阅版本中标为 preview。借鉴流程，不以采用该云服务为前提。[Azure RRF](https://learn.microsoft.com/en-us/azure/search/hybrid-search-ranking) 说明基于排名的融合；[PostgreSQL 文档](https://www.postgresql.org/docs/17/functions-textsearch.html) 明确 `ts_rank_cd` 使用 cover-density 算法。
- **检索底座效果依据**：[Anthropic Contextual Retrieval](https://www.anthropic.com/research/contextual-retrieval) 的特定实验中，Top-20 检索失败率由 5.7% 降至上下文 embedding + BM25 的 2.9%，加 reranker 后为 1.9%。这些外部结果支持评测该组合，不能直接推定本项目获得相同收益；本项目初始词法通道仍是 PostgreSQL 全文检索。
- **多 Agent 的收益与成本边界**：[Anthropic Research 工程报告](https://www.anthropic.com/research/multi-agent-research-system) 报告内部研究任务相对单 Agent 提升 90.2%，同时报告多 Agent 约消耗普通聊天 15 倍 token（参照是聊天，非单 Agent）。[2026 年使用指南](https://claude.com/resources/articles/building-multi-agent-systems-when-and-how-to-use-them) 建议按可独立上下文拆分任务。[Scaling Agent Systems v3](https://arxiv.org/abs/2512.08296v3) 在 260 种配置/6 个基准中报告相对效果从 +80.8% 到 −70.0%，说明需要匹配任务结构；均非招投标数据上的直接证明。
- **可部署组件与延后项**：[Qwen3 Embedding](https://huggingface.co/Qwen/Qwen3-Embedding-0.6B)、[Qwen3 Reranker](https://huggingface.co/Qwen/Qwen3-Reranker-0.6B) 提供候选模型与使用示例；[HNSW 文档](https://supabase.com/docs/guides/ai/vector-indexes/hnsw-indexes) 说明近似索引的使用；[GraphRAG 查询文档](https://microsoft.github.io/graphrag/query/overview/) 区分实体局部查询与社区摘要全局查询。它们证明组件/模式可用，不证明其在本项目最优。

---

### Phase 5：可复核要求清单、持久化大纲审批与并发安全章节写作 — 对应缺口 #2、#3、#5
- **状态**：`[x] 工程验收通过（2026-10-28）`；提取质量与 AI 写作质量的业务评测未做，见下方交付记录的边界说明。
- **核心架构与状态机设计（解决长周期审批与并发写冲突）**：
  1. **可复核的完整要求清单（`requirement_items`）**：
     - 在生成大纲前，先汇总提取招标文件中的**评分项（`scoring_item`）、强制性条款（`★`/`▲`）、非评分必答项、指定格式与附件要求**，写入 `requirement_items` 表，支持人工补充、修订与确认“要求清单已完整”。
     - 完整清单来自逐文档/章节遍历与复核，不能以检索 Top-K 代替。每条要求绑定版本化原文来源，并与 Phase 4.3 的查询/证据记录关联；查无证据必须记录缺口，不能从待答清单中删除。
  2. **大纲生成与持久化业务审批状态机（缺口 #2）**：
     - 新增 `proposal_outlines`（字段：`id`, `task_id`, `tenant_id`, `version`, `status IN ('draft','pending_approval','approved','superseded')`, `approved_by`, `approved_at`）与 `proposal_sections` 表（包含与 `requirement_items` 的多对多映射关系及前置依赖章节 `depends_on_section_ids BIGINT[]`）。
     - **不依赖短时工具 `intercept` 挂起**：大纲生成后落库为 `pending_approval` 并暂停写作派发，即使服务重启或等待数天，状态依然持久化在 PostgreSQL 中。
     - 人工调整目录结构或要求清单变更时，自动递增 `outline.version` 并使旧批准失效（置为 `superseded`）。
     - **后端双端强制门禁（派发入口 + 在途写回）**：
       - **派发前校验**：章节写作调度入口在 DB 层强制校验 `outline.status = 'approved'` 且将当前批准的 `outline.version` 作为任务的 `based_on_outline_version` 传入，未批准版本拒绝启动任何章节写作任务。
       - **写回时二次校验（防在途大纲过期）**：若 Worker 运行期间人工修改了要求清单或目录，导致大纲升级为新版本且原批准失效，即使该章节的 `current_rev_no` 尚未变化，在途 Worker 返回结果时 DB 事务也会校验 `based_on_outline_version == current_approved_outline.version`。若大纲版本已过期或未处于 `approved` 状态，严禁将该写回直接设为当前有效主版本（必须拒绝升为主稿，仅以 `status = 'stale_outline'` 候选记录归档留存，待新大纲批准后重新调度）。
  3. **带依赖门禁的章节写作工作流（缺口 #3）**：
     - 扩展调度器支持检查 `proposal_sections.depends_on_section_ids`：仅当前置依赖章节已有初稿/定稿时，才将后续依赖章节的写作任务置为可领取状态。
     - 新增 `section_write_draft`、`section_read_context` 工具与分段生成/摘要衔接机制，防止长标书超出单次上下文窗口。
     - 基于知识库生成事实性内容时使用 Phase 4.3 的有效证据集合，将引用绑定至本次章节版本；缺失、冲突或失效证据不得被改写成已证实承诺。Phase 4.4 的多 Agent 路由为可选能力，不作为写作审批的替代。
  4. **乐观锁版本控制与审批失效规则（缺口 #5）**：
     - 新增 `section_revisions` 表（字段：`id`, `section_id`, `rev_no`, `base_rev_no`, `based_on_outline_version`, `source IN ('ai_initial','ai_rewrite','human_edit')`, `content_md`, `diff_summary`, `status IN ('draft','reviewing','approved','rejected','stale_outline')`, `created_by`, `approved_by`），并在 `proposal_sections` 记录 `current_rev_no` 与 `approved_rev_no`。
     - **并发写入校验**：无论是人工保存还是在途 AI Worker 写回，均须同时校验 `base_rev_no == current_rev_no` 与 `based_on_outline_version` 有效性。若提交时 `base_rev_no != current_rev_no`（例如人工已修改到第 3 版，基于第 2 版运行的 AI 才返回），严禁覆盖当前主版本，必须拒绝覆盖或存为待合并候选稿。
     - **批准绑定版本与自动撤销**：章节批准严格绑定具体的 `approved_rev_no`；一旦对该章节执行局部 AI 重写或人工编辑产生新版本，自动将章节状态从 `approved` 重置为 `draft/reviewing`，直至新版本再次通过审核。
- **可执行验收标准**：
  - 集成测试覆盖（真实 PostgreSQL + `-race`）：
    1. 未批准大纲时调用写作派发必定被后端拒绝；
    2. 模拟服务重启后，待审批大纲恢复并可在批准后正常派发章节写作；
    3. **模拟 Worker 运行期间大纲批准失效**：大纲 `v1` 批准后启动章节 Worker -> 人工修改要求使大纲升至 `v2`（`v1` 失效）且章节 `current_rev_no` 未变 -> 旧 Worker 返回写回时被 `based_on_outline_version` 门禁拦截，不会成为新大纲下的有效主版本；
    4. 模拟“人工先保存 `rev 3`、滞后 AI 携带 `base_rev_no=2` 写回”，验证人工修改绝对不被覆盖；
    5. 验证已批准章节被修改后自动撤销批准状态，且支持一键回滚至历史版本。
- **交付记录（2026-10-28）**：
  - [x] **Schema（域 C）**：`requirement_manifests`、`requirement_items`、`proposal_outlines`（部分唯一索引：每任务至多一个 approved、至多一个 draft/pending）、`proposal_sections`、`proposal_section_requirements`、`section_revisions`；全部含 `tenant_id` 与 `created_by` / `updated_by` / `approved_by`，业务唯一键含租户，跨表引用为复合外键。触发器：要求 / 原文分块变更 → epoch 递增 + 大纲 superseded；大纲 superseded → 章节 `stale_outline`、租约作废；`retrieval_evidence` → `stale` 时修订 / 章节 `stale_evidence` 并撤销批准、要求记录缺口；修订正文不可篡改；依赖只能指向同一大纲。
  - [x] **状态机（`db/requirements.go`、`db/proposal.go`）**：幂等提取（逐分块遍历 + 已入图招标要素，保留 `source_chunk_id` / `source_hash` / 页码与表格坐标；原文变更标 `needs_revision`）；清单确认带 epoch 乐观校验；提交 / 批准要求清单已确认且 epoch 一致、全部要求已映射、依赖无环；`ClaimSectionWrite`（批准大纲 + 所属版本 + 前置依赖有效稿 + 20~60s 租约）；`SaveSectionRevision` 在同一事务锁「批准大纲（FOR SHARE）→ 章节（FOR UPDATE）」，大纲版本不符归档 `stale_outline`、`base_rev_no` 不符归档 `conflict_candidate`，均不推进 `current_rev_no`；AI 写回同样校验依赖；批准绑定 `rev_no` 并拒绝 stale / contradicts 证据；任何新有效修订清空 `approved_rev_no`；回滚 / 采用候选稿生成 `rollback` 修订。未核验、证据不足、冲突与失效证据自动记入修订 `gaps`。读取接口使用 REPEATABLE READ 快照，避免章节与修订撕裂读。
  - [x] **工具与 API**：`proposal-writing` 技能解锁 `section_read_context` / `section_write_draft`（直接走 DB 门禁）；`server/proposal.go` 注册要求清单 / 大纲 / 章节 17 个路由，409 返回归档的候选稿，422 返回前置条件未满足，派发 202 后异步撰写并按 TTL/3 续租。
  - [x] **前端**：任务页「技术标大纲与写作」三个子视图（要求清单复核、大纲版本与审批、章节写作与修订历史 / 对比 / 批准 / 回滚 / 采用候选稿）及侧栏 `/proposal` 入口；`/roadmap/phase5` 重定向到该入口。
  - [x] **验证**：`db/proposal_test.go` 覆盖场景 1~5 与证据 stale 联动（场景 2 关闭并重建连接池）；`server/proposal_test.go` 经 HTTP 覆盖派发门禁、API 实例重建后的审批恢复、在途 AI 写回遇人工编辑 / 目录升版的归档、批准撤销与回滚；`ingest/ccgp_proposal_integration_test.go` 对 `ccgp_2026-10-06/` 10 份真实招标文件跑通解析 → 提取（评分 / ★ / 格式均非空）→ 幂等重提 → 确认 → 默认大纲全覆盖 → 提交批准 → 依赖门禁（目录不存在时跳过）。`go vet ./...`、`go test -race ./... -count=1`（w64devkit cgo，172 项通过、0 跳过）、`npm run build` 通过。
  - **未验收边界**：要求提取为启发式规则，真实语料上 PDF 跨页评分表会产生行碎片、部分评分项以类别列（如「商务部分」）为标题，召回 / 准确率未经人工标注评测，清单须人工复核；默认大纲为规则生成，非 LLM 规划；AI 派发为单次模型调用，测试使用替身 Writer，真实模型写作质量未评估；知识库证据质量仍以 Phase 4.1~4.3 的验收为准。

---

### Phase 6：标书图表生成、双层覆盖率与一致性核验、锁定版本套版导出 — 对应缺口 #6、#7、#8
- **状态**：`[ ] 待实施`
- **交付模块**：
  1. **标书专用图表生成（缺口 #7）**：
     - 新增 `proposal_charts` 表与 `chart_generate` 工具，支持生成技术架构图（Mermaid/SVG）、实施甘特图、组织架构图及设备选型表，关联至具体 `section_rev_id`。
  2. **双层响应核验与全文前后一致性检查（缺口 #6）**：
     - 新增 `compliance_matrix` 表（关联 `requirement_item_id`、`section_id`、`section_rev_id`、段落锚点、响应摘要、偏离状态 `none`/`positive`/`negative`、核验状态 `verified`/`stale`/`failed`）。
     - **区分两层验收**：
       - 第一层（大纲映射验收）：100% 的 `requirement_items` 已分配到 `proposal_sections`；
       - 第二层（正文实质响应验收）：核验工具逐条核查对应章节正文中是否包含实质性应答内容与证据，而非仅挂章节名。
     - **变更失效机制**：当 `requirement_items` 更新或某章节产生新 `section_rev_id` 时，所有关联的 `compliance_matrix` 记录与一致性审核结论自动标记为 `stale`（待重检）。
     - **证据变更传播**：Phase 4.3 的来源修订/删除、版本或适用条件变化使证据失效时，关联章节事实核验与 `compliance_matrix` 同步标记 `stale`；导出门禁检查证据有效性。相关性高或存在 citation 不足以判定正文实质响应通过。
     - **全文交叉一致性核验（`fulltext_consistency_check`）**：扫描全部章节定稿，比对总工期、质保期、SLA 指标、项目团队人数、核心软硬件型号等关键数值与承诺是否前后一致。
  3. **锁定版本集合的 DOCX/PDF 套模板导出（缺口 #8）**：
     - 新增 `export_jobs` 表与 `export/` 排版模块。
     - **导出版本锁定**：导出前校验各章节状态（默认仅允许导出全部章节均为 `approved_rev_no` 的一致性版本快照，并在 `export_jobs` 中固化本次导出的 `section_id -> rev_no` 映射集合）。
     - 套用 `.docx` 模板，渲染多级标题样式、自动目录（TOC）域、页眉页脚与页码、嵌入图表及《点对点应答及偏离表》，输出 `.docx` / `.pdf`。
- **可执行验收标准**：
  - 构造含人为矛盾（如第一章写工期 90 天、第五章写工期 180 天，以及漏答某条 `★` 条款）的测试标书样本，验证 `fulltext_consistency_check` 100% 检出矛盾与漏项；
  - 验证章节正文更新后，原审核状态自动变为 `stale` 并阻止直接按“全部已审”导出；
  - **DOCX 与 PDF 双格式排版渲染验收**：
    1. 验证导出的 `.docx` 与 `.pdf` 均能成功生成、文件结构合法且可正常打开解析；
    2. 验证渲染后的 `.pdf` / `.docx` 中自动目录（TOC）页码已实际刷新，且目录条目页码与各章节标题在成稿中的真实落位页码完全一致；
    3. 验证宽表格、系统架构图、甘特图及《点对点应答及偏离表》在页面版芯内自适应排版，无横向截断、元素重叠遮挡或异常空白分页。

---

### Phase 7：多企业租户、多用户协作与角色权限体系 (Multi-Tenant RBAC) — 对应缺口 #10
- **状态**：`[ ] 待实施`
- **交付模块**：
  1. **多租户与账号体系**：新增 `tenants`、`users`、`project_members` 表；将 Phase 4 ~ 6 预留的 `tenant_id`、`created_by`、`approved_by` 与真实登录会话全面绑定。
  2. **角色权限控制（RBAC）**：升级 `server/http.go` 鉴权中间件，按角色强制校验操作权限：
     - `bid_manager`（投标经理）：创建项目、确认要求清单、批准大纲、分配章节、锁定终稿导出；
     - `tech_writer`（技术撰写人）：负责所分配章节的 AI 生成、修订与提交审阅；
     - `commercial_specialist`（商务专员）：维护企业资信库、业绩证明与报价测算；
     - `reviewer`（审核专家）：执行全文一致性核验、章节批准/退回与偏离表复核。
- **可执行验收标准**：
  - 集成测试验证跨租户查询/修改 100% 返回 `403/404`；
  - 验证 `tech_writer` 无法越权批准大纲或修改他人负责的锁定章节。
