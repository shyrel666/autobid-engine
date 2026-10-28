# AGENTS.md — AutoBid Engine Agent Collaboration Charter

本文件为 Codex (Astra) 及其他多智能体开发工具的常驻工程记忆文件。详细的产品能力缺口清单（10 项核心缺口）与分阶段实施规划已固化于 [`ROADMAP.md`](./ROADMAP.md)，架构规范与模块职责详见 [`CLAUDE.md`](./CLAUDE.md)。

## 协作指令

1. **启动前必读**：在开始任何分析、重构或功能开发前，请先读取 [`ROADMAP.md`](./ROADMAP.md) 了解当前所处的实施阶段（Phase 3 ~ Phase 7）以及 10 大产品能力缺口对应的 Schema、Tools、Skills 与验收要求。
2. **禁止仅改 Prompt 充当产品特性**：现有双图协同调度内核（`explorations` + `assets`）负责编排章节写作、材料补全与审稿任务，但推进任一阶段功能时，必须补齐对应的**数据库迁移脚本（`db/migrations/`）、结构化数据访问层（`db/`）、业务工具（`bidtools/`）、领域技能（Skills）、人机确认节点（`intercept/`）、验收规则（`guard/`）及自动化测试**。
3. **进度同步**：每当完成 `ROADMAP.md` 中的某个阶段或缺口模块并通过 `go test ./...` / `npm run build` 验证后，请直接更新 [`ROADMAP.md`](./ROADMAP.md) 中对应的状态复选框（`[ ]` -> `[x]`）。
