# AutoBid Engine — 产品演进路线图与缺口对齐清单 (ROADMAP)

> **定位说明**：`autobid-engine` 的终极目标是打造一套完整的**企业级 AI 技术标书编写与合规审查系统**。
> 目前系统已完成底层的**双图协同调度内核（Phase 1）**与**招标文件切分、资信库匹配、废标项/评分表提取基座（Phase 2）**。
> 本路线图将完整“AI 写技术标书系统”所需的 **10 项核心产品能力缺口** 系统性地映射到现有调度架构（`explorations` 任务图 + `assets` 产物图 + `bidtools` 业务工具 + `skills` 领域方法论 + `intercept` 人工确认门禁）上，作为后续 Claude 与 Codex (Astra) 持续迭代的唯一对齐基准。

---

## 一、 当前已落地的基座能力 (Completed Baseline)

- [x] **Phase 1：核心双图协同调度与审计引擎**
  - 双图状态存储（`explorations` 探索图 + `assets` 资产图）、细粒度边关系、DAG 依赖门禁与并发控制。
  - LLM 连接池（`llmpool`）、调用审计录制（`llmrec`）、上下文自动压缩（`internal/harness`）、工作流安全守卫（`guard`）与人机协同拦截器（`intercept`）。
  - 统一 HTTP/SSE 服务层（`server`）与 CLI 入口（`cmd/server`、`cmd/migrate`）。
- [x] **Phase 2：招标文本切分、企业资信库与标书初审工具集**
  - 招标文档章节切分器（`ingest/splitter.go`）与 SQLite 全文存储（`db/documents.go`）。
  - 企业资质、历史业绩与核心人员结构化库（`company_credentials`）及自动匹配工具（`bidtools/credentials.go`）。
  - 废标项（`★`）核查、评分标准（`scoring_matrix`）提取、报价与工期风险测算（`bidtools/scoring.go`、`bidtools/pricing.go`）及结构化报告导出（`db/report.go`）。

---

## 二、 10 大产品能力缺口与架构映射总表

> **核心工程原则**：现有调度框架（Planner + Worker + Digest + Interceptor）直接承载后续的大纲生成、章节写作、知识检索与审稿合龙任务；**严禁仅靠修改顶层 Prompt 充当产品功能**，每个阶段必须补齐对应的**数据表结构（Schema）、专用工具（Tools）、领域技能（Skills）、人机确认节点（HITL Gate）与验收测试（Tests）**。

| 缺口编号 | 用户侧目标能力 | 现状诊断 | 目标实施阶段 | 核心架构落点（Schema / Tools / Skills / API） |
| :--- | :--- | :--- | :--- | :--- |
| **#1** | **上传 PDF、Word、扫描件并准确解析** | 仅支持纯文本/Markdown 切分，未实现文件解析、OCR、复杂表格与版面恢复 | **Phase 4** | 扩展 `ingest/`：支持 `.docx`、`.pdf` 原生解析、扫描件 OCR 接口、跨页复杂表格还原（保留结构化 JSON/Markdown Table）及页码/坐标溯源元数据 |
| **#2** | **根据评分标准生成技术标目录** | 未实现专门的大纲生成与人工确认流程 | **Phase 5** | 新增 `proposal_outlines` / `proposal_sections` 表；新增 `outline_generate` 工具与 `outline-architect` Skill（评分项 100% 映射章节）；接入 `intercept` 人工确认门禁 |
| **#3** | **根据项目需求编写技术方案章节** | 未实现专门的章节写作工作流 | **Phase 5** | 基于双图调度将目录节点拆解为章节写作 Worker 任务；新增 `section_write` / `section_read` 工具与 `technical-writer` Skill，融合招标需求+知识库+资信库生成正文 |
| **#4** | **复用企业方案、历史标书、产品资料** | 仅有结构化 `company_credentials`，缺乏完整方案知识库 | **Phase 4** | 新增 `knowledge_documents` / `knowledge_chunks` 表（FTS5 全文检索 + 向量 Embedding 混合检索）；新增 `kb_search_proposals` / `kb_search_products` 工具 |
| **#5** | **管理章节草稿、修订、人工确认** | 未实现正文多版本管理与在线编辑修订流程 | **Phase 5** | 新增 `section_revisions` 表（追踪 AI 初稿、局部重写、人工编辑、Diff 与审批状态 `draft`/`reviewing`/`approved`）；提供章节级修订 REST API |
| **#6** | **检查全文前后一致性与逐条响应** | 未实现针对完整技术标成稿的交叉审核闭环 | **Phase 6** | 新增 `compliance_matrix`（点对点应答/偏离表）与 `consistency_audit` 工具；跨章节核验工期、质保期、技术参数、人员配置前后一致性与 `★`/`▲` 条款 100% 闭环 |
| **#7** | **生成架构图、进度表及其他标书图表** | 未实现标书图表生成与嵌入流程 | **Phase 6** | 新增 `chart_generate` 工具与 `proposal-diagram` Skill（生成系统架构图 Mermaid/SVG、实施进度甘特图、组织架构图、设备选型对比表）并嵌入章节资产 |
| **#8** | **套模板、生成目录页码、导出 DOCX/PDF** | 仅支持导出审查 JSON/Markdown，未实现排版导出 | **Phase 6** | 新增 `export/` 排版引擎：支持企业 `.docx` 母版套用、多级标题样式绑定、自动目录（TOC）与页码生成、图表渲染嵌入，输出标准 `.docx` / `.pdf` |
| **#9** | **投标人员使用的网页工作台** | 未实现前端界面 | **Phase 3** | 新增 `web/`（React 19 + TypeScript + Vite + Tailwind CSS）：覆盖文档上传、企业资料库、实时双图调度看板、人工审批弹窗与成稿预览 |
| **#10** | **多企业、多用户和角色权限** | 仅支持可选的统一 Bearer Token 鉴权 | **Phase 7** | 扩展 `server/auth.go` 与 `db/`：新增 `tenants`、`users`、`roles`、`project_members` 表，实现多企业租户隔离与 RBAC（投标经理/技术撰写人/商务/审核员） |

---

## 三、 分阶段实施计划与验收标准 (Phased Execution Plan)

### Phase 3：投标审查与编写可视化工作台 (Web Console) — 对应缺口 #9
- **状态**：`[ ] 待实施（下一步由 Claude 执行）`
- **核心目标**：在 `web/` 目录下构建现代 B2B 投标工作台前端，打通后端全部 REST API 与 `/api/events` SSE 实时流。
- **交付模块**：
  1. **项目与标书导入中心**：招标文件导入、章节切分预览、项目启停控制。
  2. **实时双图协同监控台**：左侧任务探索树（`explorations`）+ 合规发现资产流（`assets`），中间 Worker 实时推理与工具调用流（SSE），底部 `intercept` 人工审批弹窗。
  3. **企业资信与知识库管理页**：对企业资质、业绩、团队进行增删改查。
  4. **标书审查与报告大屏**：废标红线清单、评分标准偏离矩阵、报价测算与 Markdown/JSON 导出。
- **验收标准**：`npm run build` 零报错通过；支持开发态代理联调与生产态 Go 静态托管。

---

### Phase 4：多格式文档深度解析与企业方案知识库 (Ingest & RAG Knowledge Base) — 对应缺口 #1、#4
- **状态**：`[ ] 待实施`
- **核心目标**：解决“读懂真实复杂标书文件”与“复用企业历史技术积累”两大输入端瓶颈。
- **交付模块**：
  1. **多格式解析与版面还原（缺口 #1）**：
     - 扩展 `ingest/`，支持上传 `.docx`、`.pdf` 及图片扫描件（预留可插拔 OCR Provider 接口与本地解析实现）。
     - 还原复杂跨页表格（转化为带表头语义的 Markdown Table 与结构化 JSON），保留原文档页码（`page_start`, `page_end`）与章节层级。
  2. **企业方案与产品知识库（缺口 #4）**：
     - 新增 SQLite 迁移：创建 `knowledge_documents`（文档元数据：类别如`历史标书`/`技术白皮书`/`产品彩页`/`标准方案`、行业标签）与 `knowledge_chunks`（分块正文、FTS5 全文索引、可选向量 Embedding）。
     - 在 `bidtools/` 新增 `kb_search_proposals`、`kb_search_products`、`kb_get_chunk` 工具，供写作 Worker 按技术关键词检索高质量历史方案段落。
     - 新增 `/api/knowledge/*` 上传、分块与检索接口，并在 `web/` 前端提供知识库管理界面。
- **验收标准**：上传含复杂表格的 DOCX/PDF 能准确切分章节与还原表格；Worker 可通过工具精准召回历史方案片段并附带来源引用。

---

### Phase 5：技术标大纲生成、章节撰写工作流与多版本修订 (Outline & Section Drafting) — 对应缺口 #2、#3、#5
- **状态**：`[ ] 待实施`
- **核心目标**：将系统从“读标审查”升级为“人机协同写标”，实现从评分表到大纲、从大纲到章节初稿与多轮修订的完整闭环。
- **交付模块**：
  1. **基于评分标准的大纲生成与确认（缺口 #2）**：
     - 新增 `proposal_outlines` 与 `proposal_sections` 表（含章节编号、标题、关联的招标评分项 ID、分配字数、写作要求、当前状态）。
     - 新增 `outline_generate`、`outline_update` 工具与 `outline-architect` Skill：强制将 `scoring_matrix` 中的每一个得分点映射到具体章节目录，确保不漏项。
     - 大纲生成后自动触发 `intercept` 挂起，等待投标人员在前端确认或拖拽调整目录结构后再解锁后续章节写作。
  2. **并行/依赖章节写作工作流（缺口 #3）**：
     - 新增 `section_write_draft`、`section_read_context` 工具与 `technical-writer` Skill。
     - 调度器根据确认后的目录树派发章节写作 Worker，自动组装该章节对应的【招标技术需求 + 评分点要求 + 企业知识库检索片段 + 企业资信业绩】生成结构化技术方案正文。
  3. **章节草稿、多版本管理与人工确认（缺口 #5）**：
     - 新增 `section_revisions` 表（存储版本号 `rev_no`、内容 `content_md`、修改来源 `ai_initial`/`ai_rewrite`/`human_edit`、修订说明、审阅状态 `draft`/`reviewing`/`approved`）。
     - 新增 `/api/proposals/sections/*` 接口，支持前端在线编辑正文、选中段落让 AI 按指令局部扩写/改写、版本历史回滚与单章定稿锁定。
- **验收标准**：端到端跑通“评分表提取 -> 生成技术标目录 -> 人工确认目录 -> 自动撰写各章初稿 -> 局部修订并保存新版本 -> 人工定稿”全流程单测与集成测试。

---

### Phase 6：标书图表生成、全文一致性核验与套版导出 (Charts, Audit & DOCX/PDF Export) — 对应缺口 #6、#7、#8
- **状态**：`[ ] 待实施`
- **核心目标**：完成技术标成稿前的专业图表配图、全文交叉审稿合龙，以及符合招投标排版规范的 DOCX/PDF 成品导出。
- **交付模块**：
  1. **标书专用图表生成（缺口 #7）**：
     - 新增 `chart_generate` 工具与 `proposal-diagram` Skill，支持生成总体技术架构图、业务流程图（Mermaid/SVG）、项目实施进度甘特图、项目组织架构图及设备选型参数表，并关联挂载到对应章节。
  2. **全文前后一致性与逐条响应核验（缺口 #6）**：
     - 新增 `compliance_matrix` 表（《技术规范点对点应答及偏离表》：招标条款号、招标要求、投标响应内容、偏离情况 `无偏离`/`正偏离`/`负偏离`、对应技术标章节与页码锚点）。
     - 新增 `fulltext_consistency_check` 工具与 `proposal-auditor` Skill：对全部已定稿章节进行合龙扫描，检查：
       - 所有 `★` 实质性条款与评分点是否在正文中 100% 落位响应；
       - 跨章节关键数字（总工期、质保期、SLA 指标、投入人数、核心产品型号）是否存在前后不一致。
  3. **套模板、自动目录页码与 DOCX/PDF 导出（缺口 #8）**：
     - 新增 `export/` 模块，支持解析企业上传的 `.docx` 样式模板（或内置标准国企/政企标书母版）。
     - 将各章节 Markdown 正文、表格、SVG/PNG 架构图、点对点应答表组装为完整 `.docx` 文档，自动生成多级标题样式、自动目录（TOC）域、页眉页脚与页码，并提供 PDF 转换接口。
- **验收标准**：导出包含目录、多级章节、架构图、表格及点对点应答表的完整 `.docx` 标书文件，且全文一致性核验工具能准确捕获人为注入的跨章节工期/参数矛盾。

---

### Phase 7：多企业租户、多用户协作与角色权限体系 (Multi-Tenant RBAC) — 对应缺口 #10
- **状态**：`[ ] 待实施`
- **核心目标**：将系统从单团队工具升级为支持多企业隔离、多人协同编标的企业级 SaaS/私有化平台。
- **交付模块**：
  1. **多租户与用户体系**：新增 `tenants`（企业租户）、`users`（用户账号与 JWT 会话）、`project_members`（标书项目成员）表，所有业务表（文档、资信库、知识库、标书章节）绑定 `tenant_id` 实现严格数据隔离。
  2. **角色权限控制（RBAC）**：升级 `server/auth.go` 中间件，内置四大标书协作角色：
     - `bid_manager`（投标经理）：创建项目、确认大纲、分配章节、终审导出；
     - `tech_writer`（技术撰写人）：负责所分配章节的 AI 生成、修订与提交审阅；
     - `commercial_specialist`（商务专员）：维护企业资信库、业绩证明与报价测算；
     - `reviewer`（审核专家）：执行全文一致性核验、批注退回与合龙放行。
- **验收标准**：不同租户间数据完全隔离；不同角色仅能操作授权范围内的 API 与章节。
