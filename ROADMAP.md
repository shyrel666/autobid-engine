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

---

## 二、 四大核心数据域边界与多租户预留规范

为防止将标书正文、图表或知识库错误混入招标静态要素表（`assets`），全系统严格划分以下 **四大核心数据域**：

1. **域 A — 招标静态要素域（现有 `assets` + `entity_edges` + `tender_documents` + `document_chunks`）**
   - 职责：只存从招标文件中解析出的原始段落与静态要素（`tender`、`lot`、`clause`、`qualification`、`scoring_item`、`commercial_item`）。
2. **域 B — 核验推演与分析结论域（现有 `explorations` + `exploration_nodes` + `findings`）**
   - 职责：存储任务推演过程（`intent`/`fact`/`digest`）及核验审查结论（`findings`）。
3. **域 C — 投标要求清单、标书正文与响应矩阵域（Phase 5 ~ 6 新增表）**
   - 职责：存储经复核的完整投标要求清单（`requirement_items`）、技术标大纲（`proposal_outlines` / `proposal_sections`）、章节多版本正文（`section_revisions`）与点对点应答矩阵（`compliance_matrix`）。
4. **域 D — 企业知识库与成稿附件域（Phase 4 & Phase 6 新增表）**
   - 职责：存储企业历史方案/产品知识库（`knowledge_documents` / `knowledge_chunks`）、标书图表产物（`proposal_charts`）及排版导出记录（`export_jobs`）。

> **提前预留归属与多租户隔离（Phase 4 ~ 6 建表强制规范）**：
> 虽然完整 RBAC 在 Phase 7 落地，但 **Phase 4 ~ 6 新增的所有业务表及对 `company_credentials` / `assets` 的扩展，必须提前包含 `tenant_id TEXT NOT NULL DEFAULT 'default'` 以及操作人审计字段（`created_by`, `updated_by`, `approved_by`）**，且业务唯一索引必须将 `tenant_id` 纳入联合唯一键，避免 Phase 7 推倒重建表结构。

---

## 三、 10 大产品能力缺口与阶段映射总表

| 缺口编号 | 用户侧目标能力 | 当前真实状态 | 目标阶段 | 核心工程落点（PostgreSQL Schema / Tools / API / 状态机） |
| :--- | :--- | :--- | :--- | :--- |
| **#9** | **投标人员使用的网页工作台** | 未实现前端；后端已有核心审查接口，缺少量配套接口 | **Phase 3** | 新增 `web/` 单团队审查工作台（对接真实 `/api/tasks/{id}/*` 路由）；本阶段同步补齐企业资料删除与报告 Markdown 导出等配套能力 |
| **#1** | **上传 PDF、Word、扫描件并准确解析** | `POST /api/tasks/{id}/documents` 仅接收纯文本 `text` | **Phase 4** | 扩展 `ingest/` 与文档上传接口：支持 `multipart/form-data` 上传 `.docx`/`.pdf`/扫描件、OCR 接口、跨页表格结构化还原（Markdown + JSON）及页码/坐标溯源 |
| **#4** | **复用企业方案、历史标书、产品资料** | 仅有结构化 `company_credentials` 表 | **Phase 4** | 在 `db/schema.sql` 新增 PostgreSQL `knowledge_documents` / `knowledge_chunks`（基于 `tsvector` GIN 全文索引 + `pgvector`/向量检索）；新增 `kb_search_*` 工具与管理 API |
| **#2** | **根据评分标准生成技术标目录** | 无要求清单汇总表，无大纲结构与持久化确认流程 | **Phase 5** | 新增 `requirement_items`（可复核要求清单）与 `proposal_outlines` / `proposal_sections` 表；实现版本化大纲持久化审批状态机（非 `intercept` 临时挂起） |
| **#3** | **根据项目需求编写技术方案章节** | 无章节写作工作流，无前置章节依赖门禁 | **Phase 5** | 后端写作入口强制校验大纲已批准版本；实现章节依赖调度门禁；新增 `section_write_draft` / `section_read_context` 工具与长文分段生成机制 |
| **#5** | **管理章节草稿、修订、人工确认** | 无正文版本表、无并发写冲突保护与审批失效规则 | **Phase 5** | 新增 `section_revisions` 表；实现基于 `base_rev_no` 的乐观锁并发校验、人工/AI 写冲突保护、绑定具体版本的章节批准及重写自动撤销批准规则 |
| **#7** | **生成架构图、进度表及其他标书图表** | 未实现图表生成与章节挂载 | **Phase 6** | 新增 `proposal_charts` 表与 `chart_generate` 工具（生成 Mermaid/SVG 架构图、甘特图、组织架构图、设备选型表），绑定至具体章节版本 |
| **#6** | **检查全文前后一致性与逐条响应** | 无点对点应答表，无跨章节一致性核验与失效重检机制 | **Phase 6** | 新增 `compliance_matrix` 表与 `fulltext_consistency_check` 工具；分别验收“大纲章节映射率”与“正文实质响应率”，条款或正文变更自动触发重检 |
| **#8** | **套模板、生成目录页码、导出 DOCX/PDF** | `GET /api/tasks/{id}/report` 仅返回审查 JSON | **Phase 6** | 新增 `export/` 模块与 `export_jobs` 表：锁定已批准章节版本快照，套用 `.docx` 模板，渲染图表、多级标题、TOC 目录域与页码，导出 `.docx`/`.pdf` |
| **#10** | **多企业、多用户和角色权限** | 仅支持 `server/http.go` 可选单一 Bearer Token | **Phase 7** | 新增 `tenants`、`users`、`project_members` 表；启用全表 `tenant_id` 强制隔离与四大角色（投标经理/技术撰写人/商务专员/审核专家）RBAC 鉴权 |

---

## 四、 分阶段实施规范与可执行验收标准 (Phased Execution Plan)

### Phase 3：单团队标书合规审查工作台 (Web Console) — 对应缺口 #9
- **状态**：`[ ] 待实施（下一步由 Claude 执行）`
- **阶段边界定位**：先交付**真实可用的单团队招标审查工作台**，100% 对接 Phase 1 ~ 2 已实现的后端能力，并为 Phase 4 ~ 6（知识库、大纲与写标、成稿导出）预留清晰的导航扩展位（标注规划阶段，不伪造未实现的后端假功能）。
- **接口契约区分**：
  1. **直接对接的现有接口（`server/http.go`）**：
     - 任务与工作槽：`GET/POST /api/tasks`、`GET/DELETE /api/tasks/{id}`、`POST /api/tasks/{id}/{start,pause,resume,revive}`、`GET /api/tasks/{id}/coverage`、`POST /api/tasks/{id}/{goals,hints}`、`POST /api/tasks/{id}/works/{intentID}/{control,steer,message}`。
     - 推演图与结论：`GET /api/tasks/{id}/overview`、`GET /api/tasks/{id}/nodes/{nodeID}`、`GET /api/tasks/{id}/findings`、`GET /api/tasks/{id}/assets`、`GET /api/assets/{assetID}/edges`、`GET /api/tasks/{id}/report`。
     - 文档与企业资信：`POST /api/tasks/{id}/documents`（当前接收 JSON `{tender_no, lot_no, title, text, wake_planner}`，前端可支持粘贴文本或读取本地 `.txt`/`.md` 文件上传）、`GET /api/tasks/{id}/documents?content=1`、`GET/POST /api/company/credentials`。
     - 审批与 LLM 配置：`GET /api/intercepts`、`POST /api/intercepts/{pendingID}/decide`、`GET /api/tasks/{id}/intercept-audit`、`GET/POST/PUT/DELETE /api/llm/profiles...`、`GET /api/llm/health`。
     - 单任务实时事件流：`GET /api/tasks/{id}/events`（开启鉴权时支持 `?token=`；前端解析 `event: activity` 展示工具调用 `tool`、工具错误 `tool_error`、阶段结论文本 `text` 与审批请求 `intercept_request`，并处理 `event: dropped` 与断线重连后重新拉取 `overview`/`intercepts` 状态恢复）。
  2. **本阶段同步补充的轻量后端能力**：
     - 在 `db/documents.go` 与 `server/http.go` 补充 `DELETE /api/company/credentials/{id}`，闭环企业资信库增删改查。
     - 支持审查报告导出为 Markdown（可在 `GET /api/tasks/{id}/report?format=markdown` 或前端基于 `TaskReport` JSON 生成标准 Markdown/JSON 下载）。
     - 在 `cmd/server` 或 `server/` 支持托管 `web/dist` 静态产物（带 SPA History 路由回退）。
- **可执行验收标准**：
  - `cd web && npm run build` 零 TypeScript 与打包错误；
  - 后端新增接口通过 `go vet ./...` 与 `go test ./...`（配置 `AUTOBID_TEST_DATABASE_URL` 时数据库测试与 HTTP 测试全部通过）；
  - 真实联调验证：支持 Bearer Token 登录/鉴权、任务创建与文档导入、SSE 断线重连后状态恢复、`intercept` 审批弹窗放行/拒绝闭环、接口报错时的明确错误提示。

---

### Phase 4：多格式文档深度解析与企业方案知识库 (Ingest & PostgreSQL RAG) — 对应缺口 #1、#4
- **状态**：`[ ] 待实施`
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
  - 准备固定测试样本集（含跨页表格的 `.docx` 与 `.pdf` 样本）：断言解析后章节数、表格行列结构、页码范围及 `★` 条款识别率达到预期；
  - 在配置真实 PostgreSQL（`AUTOBID_TEST_DATABASE_URL`）下执行 `go test -race ./...`，验证知识库入库、去重、混合检索命中及引用元数据完整性。

---

### Phase 5：可复核要求清单、持久化大纲审批与并发安全章节写作 — 对应缺口 #2、#3、#5
- **状态**：`[ ] 待实施`
- **核心架构与状态机设计（解决长周期审批与并发写冲突）**：
  1. **可复核的完整要求清单（`requirement_items`）**：
     - 在生成大纲前，先汇总提取招标文件中的**评分项（`scoring_item`）、强制性条款（`★`/`▲`）、非评分必答项、指定格式与附件要求**，写入 `requirement_items` 表，支持人工补充、修订与确认“要求清单已完整”。
  2. **大纲生成与持久化业务审批状态机（缺口 #2）**：
     - 新增 `proposal_outlines`（字段：`id`, `task_id`, `tenant_id`, `version`, `status IN ('draft','pending_approval','approved','superseded')`, `approved_by`, `approved_at`）与 `proposal_sections` 表（包含与 `requirement_items` 的多对多映射关系及前置依赖章节 `depends_on_section_ids BIGINT[]`）。
     - **不依赖短时工具 `intercept` 挂起**：大纲生成后落库为 `pending_approval` 并暂停写作派发，即使服务重启或等待数天，状态依然持久化在 PostgreSQL 中。
     - 人工调整目录结构或要求清单变更时，自动递增 `outline.version` 并使旧批准失效（置为 `superseded`）。
     - **后端强制门禁**：章节写作调度入口在 DB 层强制校验 `outline.status = 'approved'` 且绑定当前批准的 `outline.version`，未批准版本拒绝启动任何章节写作任务。
  3. **带依赖门禁的章节写作工作流（缺口 #3）**：
     - 扩展调度器支持检查 `proposal_sections.depends_on_section_ids`：仅当前置依赖章节已有初稿/定稿时，才将后续依赖章节的写作任务置为可领取状态。
     - 新增 `section_write_draft`、`section_read_context` 工具与分段生成/摘要衔接机制，防止长标书超出单次上下文窗口。
  4. **乐观锁版本控制与审批失效规则（缺口 #5）**：
     - 新增 `section_revisions` 表（字段：`id`, `section_id`, `rev_no`, `base_rev_no`, `source IN ('ai_initial','ai_rewrite','human_edit')`, `content_md`, `diff_summary`, `status IN ('draft','reviewing','approved','rejected')`, `created_by`, `approved_by`），并在 `proposal_sections` 记录 `current_rev_no` 与 `approved_rev_no`。
     - **并发写入校验**：无论是人工保存还是在途 AI Worker 写回，均须携带 `base_rev_no`。若提交时 `base_rev_no != current_rev_no`（例如人工已修改到第 3 版，基于第 2 版运行的 AI 才返回），严禁覆盖当前主版本，必须拒绝覆盖或存为待合并候选稿。
     - **批准绑定版本与自动撤销**：章节批准严格绑定具体的 `approved_rev_no`；一旦对该章节执行局部 AI 重写或人工编辑产生新版本，自动将章节状态从 `approved` 重置为 `draft/reviewing`，直至新版本再次通过审核。
- **可执行验收标准**：
  - 集成测试覆盖（真實 PostgreSQL + `-race`）：
    1. 未批准大纲时调用写作派发必定被后端拒绝；
    2. 模拟服务重启后，待审批大纲恢复并可在批准后正常派发章节写作；
    3. 模拟“人工先保存 `rev 3`、滞后 AI 携带 `base_rev_no=2` 写回”，验证人工修改绝对不被覆盖；
    4. 验证已批准章节被修改后自动撤销批准状态，且支持一键回滚至历史版本。

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
     - **全文交叉一致性核验（`fulltext_consistency_check`）**：扫描全部章节定稿，比对总工期、质保期、SLA 指标、项目团队人数、核心软硬件型号等关键数值与承诺是否前后一致。
  3. **锁定版本集合的 DOCX/PDF 套模板导出（缺口 #8）**：
     - 新增 `export_jobs` 表与 `export/` 排版模块。
     - **导出版本锁定**：导出前校验各章节状态（默认仅允许导出全部章节均为 `approved_rev_no` 的一致性版本快照，并在 `export_jobs` 中固化本次导出的 `section_id -> rev_no` 映射集合）。
     - 套用 `.docx` 模板，渲染多级标题样式、自动目录（TOC）域、页眉页脚与页码、嵌入图表及《点对点应答及偏离表》，输出 `.docx` / `.pdf`。
- **可执行验收标准**：
  - 构造含人为矛盾（如第一章写工期 90 天、第五章写工期 180 天，以及漏答某条 `★` 条款）的测试标书样本，验证 `fulltext_consistency_check` 100% 检出矛盾与漏项；
  - 验证章节正文更新后，原审核状态自动变为 `stale` 并阻止直接按“全部已审”导出；
  - 验证导出的 `.docx` 文件包含完整多级标题、TOC 目录、表格、架构图与点对点应答表。

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
