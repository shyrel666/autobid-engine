# CLAUDE.md — AutoBid Engine 开发宪章与架构指南

本文件为 AI 编码助手（Claude Code / Codex）的常驻工程记忆文件。在开始任何分析、重构或功能开发前，请务必遵循本文件与 [`ROADMAP.md`](./ROADMAP.md) 中的真实代码基线与架构规范。

---

## 1. 项目定位与当前真实基线

`autobid-engine` 是一套基于 **PostgreSQL 双图存储**（`assets` 招标静态要素图 + `explorations` 任务推演图）的**企业级 AI 技术标书编写与合规审查系统**。

- **已完成基线（详见 [`ROADMAP.md`](./ROADMAP.md) 第一节）**：
  - **Phase 1**：PostgreSQL 存储与幂等迁移（`db/schema.sql`）、双图状态与租约争抢（`db/exploration.go`, `db/assets.go`）、冷节点折叠与有界概览（`db/digest.go`, `db/overview.go`）、LLM 连接池与审计录制（`llmpool`, `llmrec`）、工具调用级拦截与计数（`intercept`, `guard`）及 HTTP/SSE 服务层（`server/http.go`）。
  - **Phase 2**：纯文本/Markdown 招标文档章节切分（`ingest/splitter.go`）、企业资信库（`company_credentials`）、标书核验工具与技能（`bidtools/`：`document_outline`、`search_document`、`read_chunk`、`query_company_credentials`、`simulate_scoring_table`、`verify_commercial_bid`）及结构化审查报告（`db/report.go`）。
- **待演进阶段（详见 [`ROADMAP.md`](./ROADMAP.md) 第四节）**：
  - **Phase 3**：单团队标书合规审查工作台前端（`web/`，对应缺口 #9）+ 轻量配套后端接口（资信按 ID 全量更新与删除、报告 Markdown 导出、静态资源托管）
  - **Phase 4**：PDF/DOCX/扫描件 OCR 深度解析与 PostgreSQL 企业方案知识库 RAG（对应缺口 #1、#4）
  - **Phase 5**：可复核要求清单、持久化大纲审批状态机、带依赖门禁的章节写作与乐观锁多版本修订（对应缺口 #2、#3、#5）
  - **Phase 6**：标书图表生成、双层覆盖率与全文一致性核验、锁定版本集合的 DOCX/PDF 套版导出（对应缺口 #6、#7、#8）
  - **Phase 7**：多企业租户隔离与多角色权限控制 RBAC（对应缺口 #10）

---

## 2. 目录与模块真实职责

- `cmd/server/`、`cmd/migrate/`：服务启动入口与数据库迁移 CLI。
- `db/`：**PostgreSQL（`pgx/v5`）存储层**。
  - **迁移机制**：当前 `db.Migrate(ctx)` 通过 `//go:embed schema.sql` 读取 `db/schema.sql`，对比 `schema_version` 表中的 SHA-256 校验和并在 `pg_advisory_xact_lock` 下执行幂等 DDL（注意：当前**不读取** `db/migrations/` 目录；新增表或字段必须以幂等语法写入 `db/schema.sql`，或先显式改造迁移器）。
  - **数据域划分**：
    1. `assets` + `entity_edges`：仅存储招标文件静态要素（`tender`、`lot`、`clause`、`qualification`、`scoring_item`、`commercial_item`）；
    2. `explorations` + `exploration_nodes` + `findings`：存储任务推演过程节点与核验结论产出物；
    3. `tender_documents` + `document_chunks` + `company_credentials`：存储招标原文分块与企业资信档案；
    4. 后续新增的投标要求清单、标书大纲、章节版本正文、图表、知识库与导出记录必须使用独立业务表，严禁混入 `assets` 表。
- `ingest/`：当前实现纯文本章节切分与强制条款识别；Phase 4 扩展 DOCX/PDF/OCR 版面与跨页表格还原。
- `bidtools/`：面向招投标业务的 Worker 常驻工具、延迟工具（Deferred Tools）与领域技能（Skills）。
- `agent/`：Planner（主编审）与 Worker（执行单元）编排（注：Digest 是推演图冷节点折叠机制，非独立常驻 Agent；Worker 的 `activityEmitter` 目前推送 `tool`、`tool_error`、`text` 最终摘要三类事件）。
- `intercept/` & `guard/`：**工具调用级**拦截器与 Hook 适配层。
  - `Guard` 仅拦截 `PreToolUse` / `PostToolUse`，`Guard.Stop()` 恒返回不干预；
  - `intercept_pending` 受执行 Context 与 `waiter_until` 心跳约束，仅用于短时在途工具审批，**不可替代跨重启、跨天的大纲/章节持久化业务审批状态机**。
- `server/`：REST API 路由（`server/http.go`）、单任务 SSE 实时流（`GET /api/tasks/{id}/events`）与 Bearer Token 鉴权中间件。
- `web/`：（Phase 3 起）React 19 + TypeScript + Vite + Tailwind CSS 前端工作台。

---

## 3. 核心工程约束（必须遵守）

1. **按功能适用层级扎实落地，严禁仅改 Prompt 充当产品特性**：
   - 实施 [`ROADMAP.md`](./ROADMAP.md) 中的任一阶段时，必须按该功能的实际架构落点补齐对应的 PostgreSQL 表结构（`db/schema.sql`）、数据访问层（`db/`）、业务状态机/工具（`bidtools/`）、API 路由（`server/`）或前端视图（`web/`），切勿仅靠修改 Prompt 宣称功能完成，也无需为纯展示功能强行堆砌无用的表或拦截规则。
2. **多租户与并发安全前置设计**：
   - Phase 4 ~ 6 新增的所有业务表必须预留 `tenant_id TEXT NOT NULL DEFAULT 'default'` 及操作人字段（`created_by`, `updated_by`, `approved_by`），唯一索引须包含 `tenant_id`。
   - 涉及大纲审批与章节正文修订时，必须实现 PostgreSQL 持久化审批状态机，并在派发入口与在途 Worker 写回双端同时校验 `based_on_outline_version` 与 `base_rev_no` 乐观锁。
3. **可执行验证与进度同步**：
   - Go 后端改动必须通过 `go vet ./...` 与 `go test -race ./...`；涉及数据库层的改动必须配置 `AUTOBID_TEST_DATABASE_URL` 连接真实 PostgreSQL 执行集成测试（避免 `openTestDB` 因未设置环境变量而 `t.Skip`）。
   - 前端改动（`web/`）必须通过 `npm run build` 零错误且与真实后端路由契约严格对齐。
   - 完成某个阶段并验收通过后，同步更新 [`ROADMAP.md`](./ROADMAP.md) 中的 `[x]` 状态。
