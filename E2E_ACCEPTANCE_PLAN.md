# E 级端到端验收实施计划

> 目标：先在真实 PostgreSQL 上让现有系统完整跑通「导入招标文件 → 提取要求 → 确认清单 → 核验否决项 → 生成并批准大纲 → 写章节 → 矩阵核验 → 全文一致性 → 导出就绪 → 生成 DOCX」，再用正反例证明每个门禁“该拦时拦住、修正后放行”。
>
> 本计划只做 **E 级（工程验收）**：用固定、虚构、可重复生成的数据，证明代码、数据库和状态机按设计工作。它不是 V 级（真实业务效果验证），也不是 P 级（生产启用）。合成数据通过，不代表真实投标文件写作质量达标。

---

## 1. 为什么先做这一步

当前系统已经具备大部分能力，但缺少一套“从头到尾可重复执行、结果可核对”的验收：

- 导入、切分、要求提取、否决项核对、大纲审批、章节修订、矩阵核验、一致性核验、导出门禁等能力分散在多个包和接口里，平时只被单元测试或局部集成测试覆盖。
- 局部测试通过，不等于这些状态机串起来后仍然正确。
- “门禁正确”不能只看代码，要看实际运行：该阻断时是否阻断、阻断原因是否准确、修正后是否能恢复。
- 没有固定夹具，就无法判断一次改动是修好了还是弄坏了；也无法在以后回归时快速复验。

因此 M1 先交付一套 **E 级端到端验收器 + 虚构数据集 + 根目录报告**。它不改产品功能，只证明现有流程和门禁。

---

## 2. 完成定义（Definition of Done）

满足以下全部条件才算 M1 完成：

1. 一条主链路在真实 PostgreSQL 上跑通，最终 `ExportReadiness.Ready == true`。
2. 主链路至少完成一次 DOCX 导出：任务创建、领取、执行、提交成功，产物文件存在且可被 DOCX 解析器回读。
3. M1 门禁 / 解析校验清单中的每一项都有“触发 → 目标信号 → 修正 → 放行”证据；G06、G09、G11 还必须完整恢复 `ExportReadiness.Ready == true`。无法修正的，必须在报告中明确写 `BLOCKED`，不能写 `PASS`。
4. 报告输出到项目根目录：
   - `E2E_ACCEPTANCE_REPORT.md`
   - `e2e-acceptance-report.json`
   - `e2e-artifacts/`（生成的招标文件、更正公告、导出 DOCX）
5. 同一执行器连续运行两次，归一化后的检查结果完全一致（忽略时间戳、数据库自增 ID、任务 ID）。
6. `go vet ./...` 通过；在 `AUTOBID_TEST_DATABASE_URL` 指向真实 PostgreSQL 时 `go test -race ./...` 通过。
7. 不修改生产默认行为，不放宽核验阈值，不加测试后门，不用 `t.Skip` 掩盖数据库缺失。
8. 报告和代码中不出现数据库连接串、真实企业、真实项目或个人信息。

---

## 3. 验收边界：E / V / P 与 M1 / M2 / M3

### 3.1 E / V / P

| 级别 | 含义 | 本项目当前状态 |
|---|---|---|
| E | 工程实现验收：真实数据库、真实状态机、真实门禁，合成数据 | 本计划覆盖 |
| V | 业务效果验证：真实招标文件、真实企业材料、人工标注金标准、召回率/准确率 | 暂不做，缺真实材料与标注集 |
| P | 生产启用：权限、租户隔离、预算、监控、人工复核流程全部到位 | 暂不做 |

报告中所有结论必须标注 `E`，禁止写成“验收通过”“生产可用”“准确率达标”。

### 3.2 M1 做 / 不做

本计划中的“端到端”仅指 **Go 业务调用 → PostgreSQL 状态机 → 核验 → DOCX 导出与回读**。执行器直接调用 `db`、`ingest`、`export`，不覆盖 HTTP REST 路由、SSE 或浏览器操作；这些需要另做 API / UI 工程验收，不得由本报告推断为已通过。

**M1 做：**

- 招标文件与更正公告导入、角色识别、发布时间先后生效。
- 要求提取：评分项、★实质性条款、普通必答项、资格性/符合性否决项、法定否决项、更正覆盖。
- 清单确认、否决项逐项核对、投标/不投标决策。
- 默认大纲生成、要求映射、提交、批准。
- 确定性“替身写作”（模板生成章节正文，不调用真实大模型）。
- 修订保存、乐观锁冲突、批准。
- 矩阵核验（规则核验器）、全文一致性核验、导出就绪门禁。
- DOCX 导出任务与产物。
- 门禁正反例。

**M1 不做（报告中列为 BLOCKED）：**

- 真实大模型写作质量、句子级事实核验。
- 企业证据库检索与证据绑定、过期证书、子公司业绩、型号错误、无证据业绩。
- 承诺低于评分阈值、报价不一致、报价与商务文件冲突。
- PDF 导出与 LibreOffice 目录页码刷新。
- 真实招标项目/企业材料上的准确率、召回率。
- 多 Agent 检索与写作。

这些依赖 D2/D3 能力或外部依赖，M1 不伪报通过。

---

## 4. 端到端主链路

执行器按以下顺序调用现有 API。每一步都记录“预期状态”和“实际状态”，不满足即失败。

| 步骤 | 操作 | 关键 API | 通过条件 |
|---|---|---|---|
| S1 | 创建隔离任务 | `db.CreateTask` | 任务创建成功，推演空间与 origin Fact 存在 |
| S2 | 导入招标文件 | `ingest.Ingest`（DOCX，role=tender，发布时间 T1） | 文档角色为 tender，分块/表格数 > 0 |
| S3 | 导入更正公告 | `ingest.Ingest`（DOCX，role=correction，发布时间 T2 > T1） | 文档角色为 correction，更正条目被识别 |
| S4 | 提取要求 | `db.ExtractRequirements` | 评分项、否决项、★条款、普通要求、覆盖关系符合黄金期望 |
| S5 | 确认清单 | `db.ConfirmRequirements` | manifest 状态 confirmed，记录本次 epoch，供 S7/S8 核对大纲依据 |
| S6 | 核对否决项 | `db.SetVetoStatus` | 全部否决项 passed，且带核对说明 |
| S7 | 创建默认大纲 | `db.CreateOutline`（空 Sections） | 所有技术标范围内的要求都被映射，价格评分项 out_of_scope；大纲 epoch 等于 S5 记录值 |
| S8 | 提交并批准大纲 | `db.SubmitOutline` → `db.ApproveOutline` | 大纲 approved，版本号=1，epoch 未变化 |
| S9 | 逐章节替身写作 | `db.SaveSectionRevision` | 每章有当前修订，`outcome=applied` |
| S10 | 逐章节批准 | `db.ApproveSectionRevision` | 每章 `approved_rev_no == current_rev_no` |
| S11 | 矩阵核验 | `db.VerifyComplianceMatrix` | 所有映射行 verified，failed=0，pending=0 |
| S12 | 一致性核验 | `db.RunConsistencyCheck` | contradictions=0，omissions=0 |
| S13 | 导出就绪 | `db.ExportReadiness` | ready=true，blockers 为空 |
| S14 | 创建 DOCX 导出任务 | `db.CreateExportJobs` | 返回 1 个 queued 任务，preflight 无阻断 |
| S15 | 执行、提交并回读导出 | `db.ClaimExportJob` → `export.Service.Execute` → `db.CompleteExportJob` → `ingest.Parse` | 任务 succeeded，产物存在且 SHA-256 与任务记录一致；解析器回读无错误，章节标题与正文存在；`TOCRefreshed` 允许为 false 并记录警告 |

### 4.1 章节写作顺序

默认大纲包含无要求的章节（总体方案、评分项索引），这些章节也必须写正文并批准，否则导出就绪会报 `section_not_approved`。

写作顺序按大纲 `seq`：

1. `1 总体技术方案`（无要求，写通用技术路线，禁止写入数值承诺）。
2. `2 技术评分项逐项响应`（无要求，写索引，禁止写入数值承诺）。
3. 其余章节按依赖顺序；AI 来源的写回要求依赖章节已有当前修订。

### 4.2 替身写作规则

M1 不调用大模型。每章正文按映射要求生成：

- 每个要求一个自然段。
- 段落包含：要求标题、要求描述原文、明确的响应句、至少一句不重复要求的实质内容。
- 数值门槛沿用要求原文的数值，避免出现第二个不同数值。
- 通用章节不写任何工期、质保、响应时限、可用性、团队人数、型号。
- 不写“不满足/无法/未取得/负偏离”等否定表述。

这样做是为了让规则核验器可以通过，并让一致性核验不产生噪声。它只证明流程，不代表写作质量。

---

## 5. 虚构数据集设计

所有数据均为虚构，名称统一使用“演示/虚构”标识，不与 `ccgp_2026-10-06/` 或任何真实项目、企业关联。

### 5.1 招标文件（DOCX，确定性生成）

- 项目名称：智慧园区安防系统采购项目（虚构）
- 招标编号：`DEMO-2026-SMART-PARK-001`
- 发布时间：`2026-10-01 09:00 +08:00`
- 结构：

| 章节 / 独立分块 | 内容 | 期望提取结果 |
|---|---|---|
| 第一章 投标人须知 / 1.1 投标人资格要求 | 独立标题 `1.1.1 资质要求`（★）与 `1.1.2 业绩要求` | 1 条 mandatory_clause，1 条 non_scoring_required |
| 1.2 资格审查 | 资格性审查表 3 行（营业执照、专业资质、投标保证金） | 3 条 qualification 否决项 |
| 1.3 符合性审查 | 符合性审查表 2 行（签署盖章、投标有效期） | 2 条 conformity 否决项 |
| 1.4 投标截止时间 | `2026年11月20日9时00分` | manifest.bid_deadline 被抽取 |
| 第二章 技术要求 / 2.1 总体技术要求 | 独立标题 `2.1.1 工期要求`（不超过 90 天）、`2.1.2 质保要求`（不少于 3 年）、`2.1.3 可用性要求`（不低于 99.9%） | 3 条 non_scoring_required |
| 2.2 售后服务要求 | 独立标题 `2.2.1 响应与到场要求`（2 小时响应、4 小时到场）与 `2.2.2 修复要求`（不超过 24 小时） | 2 条 non_scoring_required |
| 2.3 项目团队要求 | 独立标题 `2.3.1 团队要求`（不少于 12 人）与 `2.3.2 驻场服务要求`（被更正的旧条款） | 2 条 non_scoring_required |
| 第三章 评标办法 / 3.1 评分标准 | 技术方案 50 分；类似项目业绩 30 分；投标报价 20 分 | 3 条 scoring_item，价格项不入技术标 |

法定否决项由现有规则自动产出 6 条，不写入 DOCX。

**分块约束：** 每一条预期要求都必须使用独立编号标题，并被 `ingest.SplitLines` 切成独立 `document_chunks`。不得把多个预期要求仅写成同一分块中的分号短句；当前提取器主要按分块生成候选要求，不承担这种原子条款拆分。S1 必须先输出分块普查表，核对每个条款的 `section_code`、标题与内容，再运行要求提取。

### 5.2 更正公告（DOCX，确定性生成）

- 发布时间：`2026-10-10 09:00 +08:00`
- 正文只有一个章节：`2.3.2 驻场服务要求`
- 内容：`原要求“驻场服务人员须持有厂家认证证书”，现更正为“驻场服务人员须持有有效期内的厂家认证证书及身份证明”。`
- 期望：旧 `2.3.2` 被标记 stale 并指向新要求；提取结果 `Superseded = 1`；大纲只映射新要求。

选择文字变更而不是数值变更，是为了避免旧值/新值同时出现在正文里导致一致性误报。数值变更的覆盖逻辑放到 M2 用单独夹具验证。

### 5.3 G09 专用一致性冲突夹具

G09 使用独立任务和独立场景夹具，不修改主链夹具与黄金条数：

- 两条独立要求均允许 120 天：`G09-1 建设工期不超过 120 天`、`G09-2 交付周期不超过 120 天`。
- 自定义两个章节，每个章节只映射其中一条要求；初始正文中一章写“总工期 90 天”，另一章写“总工期 120 天”。两条响应都满足各自矩阵要求，但一致性核验应将同一工期指标的不同承诺记为 contradiction。
- 修正时把 90 天统一为 120 天，重新保存并批准受影响章节，再依次重跑矩阵和一致性核验。

### 5.4 虚构企业资料（M1 只生成，不消费）

在 `eval/bid/fixtures/company/` 放置：

- `company-profile.json`：虚构公司名称、统一社会信用代码（明显假号）、注册地址。
- `credentials.json`：虚构资质证书、有效期。
- `performance.json`：虚构类似项目业绩。

M1 不导入知识库、不绑定证据；这些文件用于 M2，不属于 M1 的通过条件。报告中明确写 `evidence_binding = BLOCKED`，不得因为生成了这些 JSON 文件就宣称证据链路已被覆盖。

### 5.5 黄金期望

`eval/bid/fixtures/golden.json` 记录首轮复核后冻结的期望值，至少包括：

```json
{
  "fixture_version": 1,
  "tender_no": "DEMO-2026-SMART-PARK-001",
  "bid_deadline": "2026-11-20T09:00:00+08:00",
  "veto_total": 11,
  "veto_by_kind": { "qualification": 3, "conformity": 2, "statutory": 6 },
  "scoring_total": 3,
  "scoring_weights": { "技术方案": 50, "类似项目业绩": 30, "投标报价": 20 },
  "price_items_out_of_scope": 1,
  "superseded": 1,
  "mapped_requirements": 11,
  "outline_sections": 6
}
```

注意：首轮实际提取结果不直接写进 golden。先输出“提取普查表”，人工核对是夹具问题还是解析器问题；确认解析器行为正确后再冻结。若解析器确有 bug，修解析器并补单元测试，不放宽期望。

---

## 6. 门禁正反例清单（M1）

每个门禁按“触发 → 阻断 → 修正 → 放行”执行。阻断信号优先使用 `ExportReadiness.Blockers[].Kind`，接口错误使用对应的 sentinel error。“放行”指本行目标错误或阻断消失，不表示其他独立门禁自动消失；要求完整恢复导出时，必须显式断言 `ExportReadiness.Ready == true`。

| 编号 | 门禁 | 触发方式 | 期望阻断信号 / 状态 | 修正方式 | 修正后期望 |
|---|---|---|---|---|---|
| G01 | 无批准大纲 | 确认清单后不建大纲，直接查就绪 | `no_approved_outline` | 创建并批准大纲 | 目标阻断消失 |
| G02 | 清单未确认 | 提取后不确认，直接提交大纲 | `ErrRequirementsNotConfirmed` | `ConfirmRequirements` | 提交成功 |
| G03 | 否决项待核对 | 所有否决项保持 pending | `veto_pending` | 全部 `SetVetoStatus(passed)` | 目标阻断消失 |
| G04 | 否决项不通过 | 任一项置 failed | `veto_failed` | 改为 passed | 目标阻断消失 |
| G05 | 决策不投标 | `SetBidScope(decision=no_bid, decision_note="虚构场景：主动放弃")` | `no_bid_decision` | 确认无 failed 否决项后改为 bid | 目标阻断消失 |
| G06 | 新增要求后的失效与未映射恢复 | 在已批准主链上调用 `db.CreateRequirement` 新增一条在范围内要求 | `no_approved_outline`；旧大纲为 superseded，清单 epoch 增长，系统自动派生草案且 `Coverage.Unmapped` 包含新要求 | 复核并确认清单 → 编辑已有草案映射新要求 → 提交批准 → 全部章节写回并批准 → 全量矩阵 → 一致性 | 新要求已映射，`ExportReadiness.Ready == true` |
| G07 | 章节未批准 | 保存草稿但不批准 | `section_not_approved` | 批准当前修订；需要整体恢复时再重跑矩阵与一致性 | 目标阻断消失 |
| G08 | 矩阵未通过 | 某章节只写“详见招标文件” | `matrix_failed` | 改写实质响应并重新批准 → 重新矩阵核验；需要整体恢复时再重跑一致性 | 目标行 `verified` |
| G09 | 一致性有 open 结论 | 在 5.3 专用任务中，两章分别写“总工期 90 天”和“总工期 120 天” | `finding_open`，且两条矩阵响应均 verified | 统一为 120 天，重新保存并批准受影响章节 → 矩阵 → 一致性 | 最新运行无 open finding，整体 ready |
| G10 | 乐观锁冲突 | 同一章节连续两次用 `base_rev_no=0` 保存 | `outcome=conflict_candidate`，主版本不变 | 用当前版本号重存 | `outcome=applied` |
| G11 | 更正后的失效、自动换绑与恢复 | 使用只导入招标文件的独立任务先达到 ready，再导入更正并重新提取 | `no_approved_outline`；旧要求 stale 并指向新要求，旧大纲 superseded，自动 v2 已换绑新要求 ID | 复核并确认清单 → 直接使用自动 v2 提交批准 → 全部章节写回并批准 → 全量矩阵 → 一致性 | 不再映射旧要求，`ExportReadiness.Ready == true` |
| G12 | 未知证据引用 | `ParseSectionDraft` 引用上下文之外的 evidence_id | `unknown_evidence` gap，ID 被移除 | 重新解析不含未知 ID 的草稿 | 无 `unknown_evidence` gap |
| G13 | 导出被门禁拒绝 | 在 G03 状态下调用 `CreateExportJobs` | `ExportBlockedError`，且不产生 queued 任务 | 修正门禁 | 正常创建任务 |

说明：

- G06 / G11 按正常业务状态机操作后，旧批准大纲已经失效，`ExportReadiness` 会在没有批准大纲时提前返回，因此本计划断言实际可达的 `no_approved_outline` 及详细状态，不强行要求 `requirement_unmapped` / `requirement_superseded`。后两者作为代码中的防御性阻断保留，但不通过测试后门构造。
- 要求实质变更后，系统会通过 `afterRequirementChangeTx` 自动派生草案。验收器不得再次调用 `CreateOutline` 或 `UpdateOutline` 制造重复版本；新大纲的章节均为新记录，必须全部重新写回和批准，不能只处理受影响章节。
- G09 使用 5.3 的专用夹具，保证 90 / 120 都满足矩阵数值门槛；不得把主链“不超过 90 天”的要求改成 120 天响应来制造通过。
- G12 是草稿解析校验，不是 `ExportReadiness` 阻断项，也不证明证据可采性或句子级事实落证。

---

## 7. 验收执行器设计

### 7.1 代码位置

放在 `eval/bid/accept/`，作为独立 `main` 包，不进入 `cmd/`，避免污染生产启动入口。

| 文件 | 职责 |
|---|---|
| `eval/bid/accept/main.go` | 读取环境变量、打开数据库、迁移、按顺序跑场景、写报告、退出码 |
| `eval/bid/accept/dataset.go` | 确定性生成招标 DOCX、更正 DOCX、虚构企业 JSON |
| `eval/bid/accept/draft.go` | 按章节和映射要求生成替身正文 |
| `eval/bid/accept/flows.go` | 主链路与各门禁场景 |
| `eval/bid/accept/checks.go` | 检查项注册、状态（pass/fail/blocked）、断言辅助 |
| `eval/bid/accept/report.go` | JSON + Markdown 报告 |
| `eval/bid/fixtures/golden.json` | 冻结后的黄金期望 |
| `eval/bid/fixtures/README.md` | 虚构数据说明、边界、如何复现 |
| `eval/bid/fixtures/company/*.json` | M1 不消费的虚构企业资料 |

### 7.2 运行参数

```text
go run ./eval/bid/accept \
  -report-dir . \
  -artifacts-dir ./e2e-artifacts \
  -fixture-version 1 \
  -verify-determinism \
  -keep
```

- `-report-dir`：报告目录，默认项目根目录。
- `-artifacts-dir`：生成的 DOCX 与导出产物目录。
- `-verify-determinism`：同一进程内完整执行两轮主链路、G01~G13 和 DOCX 回读；每轮使用相互独立的任务，比较归一化结果。
- `-keep`：保留本次创建的任务与数据；默认在报告写完后清理本次创建的全部任务。
- 数据库连接串只从 `AUTOBID_TEST_DATABASE_URL` 读取；未设置时退出码 2，并提示“需要真实 PostgreSQL”，不跳过、不假装通过。
- 报告中只记录数据库类型与迁移版本，不记录连接串、主机、用户名、密码。

### 7.3 隔离与清理

- 租户：固定 `e2e-autobid`，所有 Go 业务调用显式传该租户。
- 任务：名称带运行 ID 和场景 ID，例如 `E2E-20261030-153000-main`、`E2E-20261030-153000-G11`。
- 场景隔离：主链与每个门禁默认使用独立任务，不复用其他场景的数据库状态。G11 的任务必须先只导入招标文件并达到 ready，再导入更正；不得复用已在 S3 导入更正的主链任务。
- 清理：默认只删除本次运行创建的任务（`tasks.id` 精确列表），依赖 `ON DELETE CASCADE` 清理任务级数据；不 `DROP` 表、不删除其他任务、不清理全局资产。
- 若发现任务级表未级联，则只记录警告并把清理标记为 `BLOCKED`，不做更大范围的删除。
- 测试数据库中的数据残留不应影响下一次运行：文档按 `(tenant, task, tender_no, hash)` 唯一，任务是新的。

### 7.4 超时与失败处理

- 整个执行器默认 5 分钟超时；每个场景单独 60 秒。
- 任何一步失败：记录错误详情到报告，继续执行不依赖该场景后续的门禁场景，最后以非零退出码结束。
- 不接受“只打印错误但退出码 0”。

---

## 8. 报告设计

### 8.1 JSON 结构

```json
{
  "schema": "autobid.e2e.acceptance/v1",
  "level": "E",
  "generated_at": "2026-10-30T15:30:00+08:00",
  "git_commit": "37b4cc1",
  "fixture_version": 1,
  "database": { "kind": "postgresql", "schema_migrated": true },
  "summary": { "passed": 0, "failed": 0, "blocked": 0 },
  "stages": [
    {
      "id": "main.ingest",
      "title": "导入招标文件与更正公告",
      "status": "pass",
      "checks": [
        { "id": "main.ingest.tender_role", "status": "pass", "expected": "tender", "actual": "tender" }
      ]
    }
  ],
  "gates": [
    {
      "id": "G03",
      "gate": "veto_pending",
      "blocked": true,
      "observed": "veto_pending",
      "remediated": true
    }
  ],
  "deferred": [
    { "id": "D2.evidence", "status": "blocked", "reason": "证据检索与绑定属于 D2，M1 未实现" }
  ],
  "artifacts": [
    { "kind": "tender_docx", "path": "e2e-artifacts/fixtures/tender.docx", "size": 12345, "sha256": "..." }
  ]
}
```

### 8.2 Markdown 结构

`E2E_ACCEPTANCE_REPORT.md` 按以下顺序：

1. 结论摘要：E 级、通过/失败/阻断数量、是否可导出、是否生成 DOCX。
2. 运行环境：日期、Git commit、Go 版本、数据库类型（不含连接串）。
3. 主链路阶段表：步骤、预期、实际、状态。
4. 黄金期望核对表：评分项、否决项、覆盖、映射、章节数。
5. 门禁矩阵：阻断信号、修正动作、修正后结果。
6. 导出产物：文件名、大小、SHA-256、DOCX 回读结果、警告（如目录未刷新）。
7. 延后项（BLOCKED）：逐条写原因。
8. 复现命令与边界声明：明确只覆盖 Go 后端主链，HTTP / SSE / UI、证据与真实业务质量均未由本报告验证。

报告中禁止出现：数据库连接串、真实企业/项目/个人信息、“验收通过/生产可用/准确率达标”等超出 E 级的表述。

---

## 9. 确定性与幂等

- 夹具 DOCX 的段落顺序、表格内容、核心属性、发布时间全部固定；不依赖当前时间。
- 不调用真实大模型；替身写作完全由代码模板生成。
- 两轮执行中的主链和每个门禁分别使用独立任务，按场景 ID 一一对应；报告比较以下归一化内容：
  - 每个 `check.id` 的 `status`、`expected`、`actual`（`actual` 中不含数据库 ID）。
  - 每个门禁的 `blocked`、`observed`、`remediated`，以及 G06 / G11 的完整恢复结果。
  - 黄金期望核对结果。
  - 导出任务状态、DOCX 解析回读结论、必要正文存在性和警告 `kind`；不直接比较含时间信息的导出文件字节或 SHA-256。
- 不比较：时间戳、任务 ID、文档 ID、要求 ID、章节 ID、导出任务 ID、产物路径中的任务号。
- 报告中的 ID 只出现在“详细证据”段落，不进入归一化比较。

---

## 10. 实施顺序

| 阶段 | 交付物 | 验证方式 | 预计 |
|---|---|---|---|
| S0 骨架 | 执行器 `main.go`、数据库连接、迁移、检查项与报告骨架 | `go run` 能在空场景下输出“0 项”，退出码符合预期 | 0.5 天 |
| S1 夹具 | `dataset.go`、DOCX 生成、虚构企业 JSON、`fixtures/README.md` | 生成的文件可被 `ingest.Parse` 回读；两次生成字节一致 | 1 天 |
| S2 主链路 | 导入 → 提取 → 确认 → 否决核对 → 大纲 → 写作 → 批准 → 矩阵 → 一致性 → 就绪 | 主链路 ready=true；输出提取普查表 | 1 天 |
| S3 导出 | 创建/领取/执行/提交 DOCX 任务 | 产物存在、可回读、报告记录 SHA-256 | 0.5 天 |
| S4 门禁 | G01~G13 场景与修正动作 | 每个门禁 / 解析校验有目标信号与修正证据，G06 / G09 / G11 完整恢复 ready | 1 天 |
| S5 报告与确定性 | 根目录 JSON/Markdown、两次运行比较、清理 | 两次归一化结果一致；`go vet`、`go test -race` 通过 | 0.5 天 |

总计约 4 个工作日；由 AI 执行时可在一次会话内完成，但必须按阶段验证，不能一次性写完再试。

---

## 11. 风险与应对

| 风险 | 影响 | 应对 |
|---|---|---|
| 要求提取启发式与夹具预期不一致 | 黄金期望对不上 | 先跑“提取普查表”，人工判断是夹具还是解析器问题；解析器 bug 修复并补单测，不放宽期望 |
| 规则核验器覆盖率/实质内容门槛过严 | 主链路矩阵失败 | 替身正文包含要求标题、描述原文和额外实质句；先跑单章验证，不调整生产阈值 |
| 一致性核验误报 | 主链路出现 finding | 通用章节禁止写数值；各要求数值只出现一次；型号/数量不写入 M1 正文 |
| 更正公告产生额外要求 | 大纲映射数量变化 | 更正 DOCX 只保留一个章节；先验证 `Superseded=1` 且无额外候选 |
| DOCX 导出依赖 LibreOffice | 目录页码/PDF 不可用 | M1 只导出 DOCX；目录未刷新记为警告；PDF 标记 BLOCKED |
| 测试库残留数据影响重复运行 | 结果不确定 | 每次新任务；固定租户；默认清理本次任务；不依赖全局资产计数 |
| 报告泄露连接串 | 安全风险 | 只记录数据库类型与迁移状态；代码审查关键词 `postgres://`、`password` |
| 把 E 级写成 V/P | 误导验收 | 报告固定 `level: E`；延后项逐条 BLOCKED；结论措辞模板化 |

---

## 12. 明确延后项（报告中写 BLOCKED）

| 能力 | 原因 |
|---|---|
| 证据检索、证据绑定、过期证书、子公司业绩、无证据业绩 | 依赖 D2 证据库与检索，M1 只有虚构企业文件，不消费 |
| 型号错误、参数低于阈值、报价不一致 | 依赖 D2/D3 的承诺台账与报价校验，M1 未实现 |
| 真实大模型写作质量与句子级断言核验 | M1 用确定性替身写作，只验流程 |
| PDF 导出、LibreOffice 目录页码 | 外部依赖，M1 不保证 |
| 真实项目/企业准确率、召回率 | 无真实材料与人工标注，不能做 V 级 |
| 多 Agent 检索与写作 | 无稳定收益前不启用 |
| HTTP REST 路由、SSE 与浏览器端到端操作 | 本执行器直接调用 Go 业务接口，只覆盖后端主链；API / UI 需另行验收 |

---

## 13. 验收命令

```bash
# 1. 静态检查
go vet ./...

# 2. 真实 PostgreSQL 测试（连接串只从环境变量读取）
#    AUTOBID_TEST_DATABASE_URL 已由运行环境提供
go test -race ./...

# 3. 端到端验收（报告写到项目根目录）
go run ./eval/bid/accept -report-dir . -artifacts-dir ./e2e-artifacts -verify-determinism
```

退出码约定：

- `0`：全部 M1 检查通过，无失败。
- `1`：有检查失败。
- `2`：环境不满足（数据库未配置、迁移失败）。
- `3`：出现未预期的内部错误。

---

## 14. 验收清单

- [ ] 招标 DOCX、更正 DOCX 可确定性生成并被解析器回读。
- [ ] 更正覆盖生效：旧要求 stale，新要求 active，`Superseded=1`。
- [ ] 评分项 3 条、否决项 11 条、价格项不进入技术标范围。
- [ ] 默认大纲映射完整，批准成功。
- [ ] 所有章节有当前修订且已批准。
- [ ] 矩阵核验全部 verified，无 failed/pending。
- [ ] 一致性核验无 contradiction、无 omission。
- [ ] 导出就绪 ready=true，无 blockers。
- [ ] DOCX 导出任务成功，产物存在且可回读。
- [ ] G01~G13 每个门禁 / 解析校验都有触发、目标信号与修正证据；G06、G09、G11 另有完整恢复 ready 的证据。
- [ ] G06 / G11 使用自动派生草案并完成全部新章节的写回、批准、矩阵与一致性核验，未通过测试后门构造防御性 blocker。
- [ ] 未知证据被移除并记录 `unknown_evidence` gap。
- [ ] 主链与各门禁场景任务隔离，两次完整运行的归一化结果一致。
- [ ] `go vet ./...` 通过。
- [ ] `go test -race ./...` 在真实 PostgreSQL 上通过。
- [ ] 根目录报告与 JSON 生成，内容为 E 级表述。
- [ ] 报告中无连接串、真实企业/项目/个人信息。
- [ ] 延后项逐条 BLOCKED，未伪报通过。

---

## 15. 计划确认点

执行前只需确认三件事：

1. **范围**：M1 只证明 Go 后端主链、状态机与门禁，不覆盖 HTTP / SSE / UI，不追求真实写作质量；同意把证据、型号、报价、PDF 等列为 BLOCKED。
2. **数据**：同意使用上述虚构项目/企业命名，不使用真实材料；虚构企业 JSON 在 M1 只生成、不消费，也不作为通过条件。
3. **输出**：报告与验收 JSON 放项目根目录，生成的 DOCX 放根目录 `e2e-artifacts/`，默认清理本次任务数据。

确认后按 S0 → S5 顺序执行，每个阶段先验证再进入下一阶段。
