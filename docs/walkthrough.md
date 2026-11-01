# 改造完成记录（Walkthrough）：吸收 DeepSeek Super-agent 与 ARTEX 核心机制

本次改造严格按照已确认的 [implementation_plan.md](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/docs/implementation_plan.md) 将三大核心模块无缝落入现有代码库，并通过全部静态检查、单元测试与真实 PostgreSQL 集成测试。

---

## 一、已落地的三大核心模块与文件变更

### 1. 模块一：Intent Mandate 硬件边界约束 + 确定性 Watchdog 跑偏熔断 + Hint 事件直通
* **[db/constants.go](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/db/constants.go)**：
  * 新增 [`IntentMandate`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/db/constants.go#L121-L152) 结构体（含 `AllowedSectionIDs`、`AllowedRequirementIDs`、`TargetWordsMin`、`TargetWordsMax`、`MaxToolCalls`、`ForbiddenTools`、`RequireWriteback`）及 [`NodeMandate`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/db/constants.go#L140-L152) 解析函数。
* **[agent/abort.go](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/agent/abort.go)、[agent/prompt.go](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/agent/prompt.go) & [agent/tools.go](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/agent/tools.go)**：
  * 新增 [`AbortKilledByWatchdog`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/agent/abort.go#L30) 与 `AbortCause.Is` 方法（支持携带具体熔断原因文本并由 `errors.Is` 精确匹配）；
  * 在 [`addIntentTool`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/agent/tools.go#L280-L334) 中支持结构化 `mandate` 参数并存入 `exploration_nodes.payload.mandate`；
  * 在 [`workerSystem`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/agent/prompt.go#L60-L103) 中向 Worker 渲染 `## 执行边界（Mandate 硬约束）`，并在 [`renderPlannerInput`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/agent/prompt.go#L136-L185) 中新增 `TriggerWatchdogAlert` 熔断告警。
* **[server/watchdog.go](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/server/watchdog.go)**（新建）：
  * 实现 [`WatchdogConfig`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/server/watchdog.go#L22-L50)（默认：重复调用 **3 次自动 Steer / 5 次熔断**；连续报错 **4 次自动 Steer / 7 次熔断**；零写回 **15 次自动 Steer / 25 次熔断**）；
  * 实现 [`watchdogHooks`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/server/watchdog.go#L88-L270)：在 `PreToolUse` 层对越界 `section_id`、`requirement_id`、字数区间、禁用工具与超限只读工具调用做硬件拦截；在 `PostToolUse` 层监控重复调用签名与错误/空转轨迹；在 `Stop` 层执行 `RequireWriteback` 完结门禁。
* **[server/engine.go](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/server/engine.go) & [server/manager.go](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/server/manager.go)**：
  * 在 [`runIntent`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/server/engine.go#L766-L880) 中挂载 `watchdogHooks`，当被 `AbortKilledByWatchdog` 熔断时将 Intent 转为 `stopped` 并投递 `TriggerWatchdogAlert` 唤醒 Planner；
  * 实现 [`RouteHintToRunningWork`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/server/engine.go#L670-L711) 与 [`Manager.AddHint`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/server/manager.go#L254-L268) 事件直通（Passthrough）：当新增 Hint 的 `anchors` 命中在途 running Worker 时，直接排入该 Worker 的 `steerBox` 并标记 `consumed`，绕过全局 Planner 重算。

### 2. 模块二：Inline & Spill 上下文溢出管理与链式召回（Chain of Recall）
* **[bidtools/proposal.go](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/bidtools/proposal.go)**：
  * 在 [`SectionContext`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/bidtools/proposal.go#L125-L150) 中引入 `SpilledEvidence []SpilledEvidenceBrief` 与 `CurrentRevisionSpill *SpilledTextMeta`；
  * 在 [`BuildSectionContext`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/bidtools/proposal.go#L198-L355) 中将本章节直接绑定的证据保留在 `UsableEvidence`（Inline，完整摘录 + Citation），而将未直接绑定到本章节的任务级其余可用证据折叠为 80 字微摘要放入 `SpilledEvidence`（Spill），并在 [`ParseSectionDraft`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/bidtools/proposal.go#L395-L420) 中同时识别两类证据 ID。
* **[bidtools/documents.go](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/bidtools/documents.go)**：
  * 为 [`readChunkTool`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/bidtools/documents.go#L283-L381) 补齐 `text_offset`、`table_index`、`row_offset`、`context_budget` 分页预算控制（复用 `retrieval.TextPage` 与 `retrieval.PageTable`）。
* **[server/llm_runtime.go](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/server/llm_runtime.go)**：
  * 重构 [`prepareContext`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/server/llm_runtime.go#L557-L595)：
    * **Stage 1（[`spillOlderToolResults`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/server/llm_runtime.go#L486-L534)）**：保留最近 2 轮 `BlockToolResult` 原文不动，将更早回合超长 `BlockToolResult` 零成本折叠为 `[Spilled tool_result ... recall_keys={chunk_id=..., evidence_id=...}]`；
    * **Stage 2（[`findCompactionBoundary`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/server/llm_runtime.go#L539-L555)）**：当无第二条普通文本 `RoleUser` 消息时（如 Worker 连续多轮工具调用），自动回退到完整的 `RoleAssistant(tool_use) -> RoleUser(tool_result)` 回合边界执行摘要压缩，彻底根治 `boundary <= 0` 导致的 `413` 崩溃。

### 3. 模块三：Goals（目标分解器 / `BootstrapRFPGoals` 跨四域初始化器）
* **[db/rfp_goals.go](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/db/rfp_goals.go)**（新建）：
  * 实现 [`(*DB).BootstrapRFPGoals`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/db/rfp_goals.go#L47-L202) 四步确定性流水线：
    1. 调用 `ExtractRequirements` 拆解评分表（含分档规则）、资格/符合性审查表与法定否决项、`★` 实质性条款与技术偏离要求；
    2. 将未关联静态实体的要求同步至 `assets` 表（`scoring_item` / `qualification` / `clause`）并回填 `requirement_items.source_asset_id`；
    3. 在 `exploration_nodes` 中按 `goal_code` 幂等初始化四大图谱 Goal 根节点（`G-VETO`、`G-DEVIATION`、`G-SCORING`、`G-OUTLINE`），从 `origin` Fact 连出 `spawns` 边并绑定 `exploration_anchors`；
    4. 当 `initOutline=true` 且任务尚无大纲时，自动生成 `v1 draft` 技术标目录大纲并分配章节要求映射（保留人工确认门禁）。
* **[agent/tools.go](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/agent/tools.go)、[server/proposal.go](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/server/proposal.go) & [server/knowledge.go](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/server/knowledge.go)**：
  * 向 Planner 暴露 [`decompose_rfp_goals`](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/agent/tools.go#L704-L734) 工具；
  * 在 `POST /api/tasks/{id}/requirements/extract`、`POST /api/tasks/{id}/goals/bootstrap` 与 `POST /api/tasks/{id}/documents?bootstrap_goals=true` 中接入 `BootstrapRFPGoals`。

### 4. 模块四：前端工作台（`web/`）同步对齐
* **[web/src/api/types.ts](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/web/src/api/types.ts) & [web/src/api/client.ts](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/web/src/api/client.ts)**：
  * 新增 `IntentMandate`、`RFPGoalBootstrap`、`RFPGoalDecompResult`、`ExtractRequirementsResp` 类型及 `api.bootstrapRFPGoals`、`api.extractRequirements(id, initOutline)` 与文档上传 `bootstrap_goals` 参数。
* **[web/src/features/proposal/documents/DocumentParts.tsx](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/web/src/features/proposal/documents/DocumentParts.tsx) & [web/src/features/proposal/requirements/RequirementsPage.tsx](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/web/src/features/proposal/requirements/RequirementsPage.tsx)**：
  * 文档导入卡片支持勾选「自动拆解招标目标并生成大纲草案」并展示目标分解结果；
  * 要求页支持一键「提取并生成大纲草案」，并在提取摘要横幅中展示同步的招标静态要素数、四大 Goal 根节点（`G-VETO` / `G-DEVIATION` / `G-SCORING` / `G-OUTLINE`）及自动生成的大纲草案跳转链接；
  * [web/src/features/task-workspace/resources.ts](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/web/src/features/task-workspace/resources.ts) 在要求提取/文档导入后同步失效刷新 `overview`。
* **[web/src/features/execution/NodeParts.tsx](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/web/src/features/execution/NodeParts.tsx) & [web/src/features/execution/ExplorationGraph.tsx](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/web/src/features/execution/ExplorationGraph.tsx)**：
  * 推演图新增 `stopped`（已停止/熔断）与 `exhausted` 状态筛选，并在图谱节点标题上直接标识 `[G-VETO]` 等目标编码或 `[Mandate]` 标记；
  * 节点详情弹窗（`NodeDetailModal`）新增结构化的 **「Mandate 执行边界硬约束」** 面板与 **「停止 / Watchdog 熔断原因」** 告警提示。

---

## 二、验证结果

1. **静态分析与前端构建**：
   * 后端 `go vet ./...` 零警告通过；
   * 前端 `npm --prefix web run test:unit`（15/15 通过）与 `npm --prefix web run build`（`tsc -b && vite build` 零错误）通过。
2. **新增专项单元与真实 PostgreSQL 集成测试**：
   * [server/watchdog_test.go](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/server/watchdog_test.go)：`TestWatchdogMandatePreToolUseEnforcement`、`TestWatchdogRepeatCallAutoSteerAndKill`、`TestWatchdogRequireWritebackStopGate`、`TestEngineRouteHintPassthrough` 全部通过；
   * [server/llm_runtime_test.go](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/server/llm_runtime_test.go)：`TestPrepareContextStage1SpillWithoutLLMCall`、`TestPrepareContextStage2PureToolChainCompactionNo413` 全部通过；
   * [bidtools/proposal_test.go](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/bidtools/proposal_test.go)：`TestParseSectionDraftRecognizesInlineAndSpilledEvidence`、`TestReadChunkToolPaginationBudget` 全部通过；
   * [db/rfp_goals_test.go](file:///d:/Pycharm%20Project/Bid_plan/autobid-engine/db/rfp_goals_test.go)：`TestNodeMandateParsing`、`TestBootstrapRFPGoalsIntegration`（真实 PostgreSQL 幂等验证）全部通过。
3. **全量包回归测试**：`go test -p 1 ./agent/... ./bidtools/... ./db/... ./server/... ./guard/... ./intercept/... ./retrieval/...` 全部通过。
