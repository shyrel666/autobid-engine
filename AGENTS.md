# AGENTS.md — AutoBid Engine Agent Collaboration Charter

本文件为 Codex (Astra) 及其他多智能体开发工具的常驻工程记忆文件。详细的产品能力缺口清单（10 项核心缺口）、真实基线边界与分阶段实施规范已固化于 [`ROADMAP.md`](./ROADMAP.md)，目录职责与数据库规范详见 [`CLAUDE.md`](./CLAUDE.md)。

## 协作指令

1. **启动前必读与基线对齐**：
   - 在开始任何分析、重构或功能开发前，请先读取 [`ROADMAP.md`](./ROADMAP.md) 与 [`CLAUDE.md`](./CLAUDE.md)。
   - **数据库与迁移基线**：本项目存储层为 **PostgreSQL（`pgx/v5`）**，通过嵌入 `db/schema.sql` 并在 `db.Migrate` 中校验 `schema_version` 校验和执行幂等迁移（当前不读取 `db/migrations/` 目录）。
   - **四类数据域隔离**：严格区分 `assets`（招标静态要素）、`explorations/findings`（推演过程与审查结论）、标书正文与响应矩阵域（Phase 5~6）、知识库与导出附件域（Phase 4/6），严禁混用。
2. **按功能适用范围扎实落地，禁止仅改 Prompt 充当产品特性**：
   - 推进任一阶段功能时，应根据该特性的实际边界补齐所需的 **PostgreSQL Schema（`db/schema.sql` 幂等 DDL，提前预留 `tenant_id` 与操作人字段）、数据访问与乐观锁/持久化审批状态机（`db/`）、业务工具与技能（`bidtools/`）、HTTP/SSE 路由（`server/`）或前端视图（`web/`）**。
   - 严禁把 `intercept` 短时工具调用拦截直接当作跨重启业务审批，也严禁在未实现 `ExplorationStore.Frontier` 依赖门禁或完成度校验前假定系统会自动按序写标。
3. **真实数据库验证与进度同步**：
   - 后端测试除运行 `go vet ./...` 与 `go test -race ./...` 外，涉及存储层的变更必须设置 `AUTOBID_TEST_DATABASE_URL` 在真实 PostgreSQL 上跑通集成测试（防止被 `t.Skip` 跳过）；前端变更需通过 `npm run build` 并对齐 `server/http.go` 真实路由。
   - 完成某个阶段并达到 [`ROADMAP.md`](./ROADMAP.md) 的可执行验收标准后，请更新对应的状态复选框（`[ ]` -> `[x]`）。
