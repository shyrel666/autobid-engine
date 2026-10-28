# CLAUDE.md — AutoBid Engine 开发宪章与架构指南

本文件为 AI 编码助手（Claude Code / Codex）的常驻上下文指南。在开始任何开发任务前，请务必遵循本文件与 [`ROADMAP.md`](./ROADMAP.md) 中的架构规范。

---

## 1. 项目定位与当前进度

`autobid-engine` 是一套基于双图协同调度内核（`explorations` 任务图 + `assets` 产物图）的**企业级 AI 技术标书编写与合规审查系统**。

- **已完成阶段**：
  - **Phase 1**：核心双图调度引擎、LLM 连接池（`llmpool`）、录制审计（`llmrec`）、上下文压缩（`internal/harness`）、工作流守卫（`guard`）、人机协同拦截器（`intercept`）与 HTTP/SSE 服务层（`server`）。
  - **Phase 2**：招标文本章节切分（`ingest`）、企业资信业绩库（`company_credentials`）、废标项/评分标准/报价测算业务工具集（`bidtools`）与审查报告生成（`db/report.go`）。
- **待演进阶段（详见 [`ROADMAP.md`](./ROADMAP.md)）**：
  - **Phase 3**：投标工作台 Web 前端（`web/`，对应缺口 #9）
  - **Phase 4**：PDF/DOCX/OCR 深度解析与企业方案知识库 RAG（对应缺口 #1、#4）
  - **Phase 5**：评分驱动的技术标大纲生成、人工确认、章节编写工作流与多版本修订（对应缺口 #2、#3、#5）
  - **Phase 6**：标书图表生成、全文一致性与逐条响应核验、DOCX/PDF 套模板导出（对应缺口 #6、#7、#8）
  - **Phase 7**：多企业租户隔离与多角色权限控制 RBAC（对应缺口 #10）

---

## 2. 目录与模块职责

- `cmd/server/`、`cmd/migrate/`：服务启动入口与数据库迁移 CLI。
- `db/`：SQLite 存储层、双图状态管理（`explorations`、`assets`）、摘要（`digest.go`）、全局视图（`overview.go`）、招标文档（`documents.go`）与报告导出（`report.go`）。所有 Schema 变更必须通过 `db/migrations/` 下的增量 SQL 文件演进。
- `ingest/`：招标文件与知识库文档解析、版面恢复、表格提取与多级章节切分。
- `bidtools/`：面向标书业务的 Worker 工具与领域技能（Skills）。
- `agent/`：Planner（主编审/总控）、Worker（执行单元）、Digest（摘要提取）智能体编排。
- `intercept/`：人机协同门禁（Human-in-the-Loop），用于大纲确认、高风险废标裁决、关键章节定稿等需人工介入的节点。
- `guard/`：工作流完成度守卫，拒绝未完成实质性核验或未覆盖必答条款的草率结束。
- `server/`：REST API 路由、SSE 实时事件流（`/api/events`）与鉴权中间件。
- `web/`：（Phase 3 起）React 19 + TypeScript + Vite + Tailwind CSS 前端工作台。

---

## 3. 核心工程约束（必须遵守）

1. **路线图对齐与状态更新**：
   - 每次实施新阶段或补齐 [`ROADMAP.md`](./ROADMAP.md) 中的 10 项产品能力缺口时，必须先阅读 `ROADMAP.md` 对应的架构映射。
   - 严禁仅通过修改 Prompt 伪造产品能力！每一项能力必须完整落地：**SQLite 表结构（Schema） -> Go 数据层（`db/`） -> Worker 工具与校验规则（`bidtools/` & `guard/`） -> 领域提示词技能（Skills） -> HTTP API（`server/`） -> 前端视图（`web/`）**。
   - 完成某个阶段或子任务后，请同步更新 `ROADMAP.md` 中的 `[x]` 勾选状态。
2. **代码质量与验证闭环**：
   - Go 后端改动必须通过 `go vet ./...` 与 `go test ./...`（无新增失败或竞态问题）。
   - 前端改动（`web/`）必须通过 `npm run build` 零 TypeScript / 打包错误。
3. **术语与领域纯粹性**：
   - 全仓代码、注释、Prompt 与测试数据保持纯正的企业招投标、采购合规与技术标书工程领域术语。
