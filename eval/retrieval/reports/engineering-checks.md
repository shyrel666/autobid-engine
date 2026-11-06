# Phase 4 增强工程检查记录

2026-10-28；Windows amd64，本机真实 PostgreSQL，AUTOBID_TEST_DATABASE_URL 已配置。连接信息不进入报告。

| 检查 | 结果 |
| --- | --- |
| `go test -race ./... -count=1 -json` | 17 个测试包通过，150 个测试/子测试通过，0 失败，0 个测试跳过 |
| `go vet ./...` | 退出码 0 |
| `cd web && npm run build` | TypeScript + Vite 通过；1989 modules |
| 评测命令 `go run ./cmd/retrieval-eval -seed -groups A,B,C,D,E -repeat 3` | 退出码 0；原始报告 synthetic-local.json |

race 使用临时便携 gcc，配置方法与已校验官方 SHA-256 见 ../README.md。测试代码里的 HashEmbedder 和假 HTTP/Reasoner 是协议与状态机测试替身，不代表语义模型质量。前端完成编译和后端契约验证，本次没有浏览器人工交互验收。

## 合成回归结果

| 组 | 观察数 | Recall@20 | nDCG@10 | 完整证据 Recall@20 | P95 ms | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| A | 84 | 1.0000 | 0.9473 | 1.0000 | 2.657 | 旧基线 |
| B | — | — | — | — | — | skipped: semantic model/current index unavailable |
| C | 84 | 1.0000 | 0.9297 | 1.0000 | 6.416 | 词法降级；含证据持久化 |
| D | — | — | — | — | — | skipped: reasoner profile unavailable |
| E | — | — | — | — | — | skipped: reasoner profile unavailable |

28 道合成保留题 × 3 次；其中 21 道有答案题进入召回分母。仅 16 个分块，且带精确型号过滤；Recall=1 没有业务质量代表性。C 的 nDCG 低于 A，未证明增强方案收益；B/D/E 无服务/配置，未执行。未测的人工支持率、任务正确率与成本指标保持 null。

运行环境：Go go1.27.0，逻辑 CPU 20，并发 1；pgvector 可用性：False；当前路径：精确 PostgreSQL 余弦。性能数字来自本机小样本且与测试并发执行，不能作为生产负载或冻结延迟门槛。

## 新增关键测试证据

- `retrieval/TestPagedTablesKeepHeadersAndOriginalMergedCells`：通过。
- `retrieval/TestCacheIsolationAndModelFailure`：通过。
- `retrieval/TestRerankerBudgetCheckedBeforeDispatch`：通过。
- `retrieval/TestResearchIdempotencyAndDisabledMulti`：通过。
- `retrieval/TestConcurrentResearchSharedBudgetAndGates`：通过。
- `retrieval/TestRestartRecoveryAndCancellation`：通过。
- `retrieval/TestSourceRestrictionsAndTableAmbiguity`：通过。
- `retrieval/TestSearchEvidenceAndSourceInvalidation`：通过。
- `retrieval/TestIndexSwitchRollbackAndStaleRejection`：通过。
- `retrieval/TestHTTPModelRejectsMalformedAndHandlesOrder`：通过。
- `retrieval/TestConservativeNumericVerification`：通过。
- `retrieval/TestGlobalBudgetAndFencedJobs`：通过。
- `retrieval/TestBaselineAblationKeepsOriginalFusion`：通过。
- `server/TestHTTPRetrievalEvidenceAndDefaultMultiGate`：通过。

## 本次核心实现文件指纹

用于关联当前工作区与检查结果；不是 Git 提交声明。

| 文件 | SHA-256 |
| --- | --- |
| `agent/retrieval.go` | `9a337f44c0da0c297ae0ad8d8ca5ffb7b2476aa24f3902e63ac8f03022905907` |
| `bidtools/knowledge.go` | `e0112237a2a07e061ef1e59e37a52e7a0b80d9d389a783306e7ea4efa02484ce` |
| `bidtools/retrieval.go` | `5ffd5c9f2789aa748401a3f9ed57a9fbd79272b916fb59d8ef116a9baf61aca7` |
| `cmd/retrieval-eval/main.go` | `51186b7c1d0f13d9d3da15a38cf427778fad5991cd2ff43930fb744f1c23e385` |
| `db/knowledge.go` | `a5bdd4f7c4ca44c1f33897c1a16c4b36f840b16d683cc79450ae672707e7d71a` |
| `db/retrieval.go` | `27d9dc6528eed6f9d0e5bdd642cefa9a025c573cc6feabb07d5f83c1d62422bb` |
| `db/retrieval_candidates.go` | `729fb913bc5a1ced13cd435b911acf57e5c3b1b9f67314c1c537286bb9aefbf9` |
| `db/retrieval_jobs.go` | `cfc6e598dd314b752947053c56cfdaab9a33955523fdf31a0378d329900eb22a` |
| `db/schema.sql` | `30a8314f4571f2b189a4a8aba83250962e4f20f9864db64dd01e0c3299e4d18b` |
| `eval/retrieval/judgments.go` | `c77f382c75e3c05ec49c3e5ee554281426bb44b57361d425e0b78460992f6767` |
| `eval/retrieval/judgments_test.go` | `3e6099d369c057c1bafd3f367fa57bb2c674d4cedcc21d704e35cf0356ad07c9` |
| `eval/retrieval/metrics.go` | `b1fb1c86e6cbfaea462152f7727dd98ea6540d7134b6f970476c0f160afce2e6` |
| `eval/retrieval/metrics_test.go` | `d5d5d2d7fa0526667fbcb832b7d2406ff35a90af1f47a6bbd19680926a20486c` |
| `retrieval/budget.go` | `995caa4e089eeaad12aa8edf417f37250816002b945332b8674af4823602b03f` |
| `retrieval/context.go` | `1acc9f3c1173c6c7010383ebfa97c1acac4d8d37bdec0b040f632d02838ed9e4` |
| `retrieval/context_test.go` | `40fa99385e49168d0f5aff4047877f99bf4edfed1041ef9f2083493429a82d4f` |
| `retrieval/evidence.go` | `be6d5626a29ae35bc54024f2fba5faca4ba2d08568700cc79dac4599845fe4b1` |
| `retrieval/faults_test.go` | `78025bb2de86a6d76bfc57c935306582325f7a462bbf8b5231f828f69b6ecb58` |
| `retrieval/lease.go` | `24a237c254d637bb9c57a1cfe3e5daab3c46423fa825d938be2ce18b8f45dcea` |
| `retrieval/providers.go` | `b81b7c09fddd20c35e98de7505898e0358c25a5a26ba84ae9381d8fb266b9399` |
| `retrieval/research.go` | `4a6c2515d8923d851dc25f3b94b55d2f3d869261ce6d9ecd15963766f945299a` |
| `retrieval/research_test.go` | `bb05d0a43dd862f17adc07e5ac32d1cc945b250adc58f86ec1ec0831cbc9c18f` |
| `retrieval/service.go` | `f67987fe4c5572f0e75bc6c31e39640a0162c64af3fb10b21faac3ccb852bf83` |
| `retrieval/service_test.go` | `b7f1ce645343d4d2abf51be7c7fb9f11600a41f32ac7d3cafd3a686e7ea1b4f6` |
| `retrieval/sources.go` | `8678e3e7ec57e270bdf82fd995efcfe379e24df9ff330356db14dabbda989b72` |
| `server/http.go` | `c52c5990bf75a7526c43d61cd627c0430cea54b7a958be1cb8a13ef945d07c97` |
| `server/retrieval.go` | `b9ec768b7374ae1192bbd1e6475e9e58c2b5fa205b625bb34295980ddfb601d0` |
| `server/task_llm.go` | `2bdabb6b2ba010f020506efb888ba6e0d2fc3e25e72ca83e266e14a4485efb5c` |
| `web/src/components/RetrievalEvidence.tsx` | `b3d77c8dada0d8ca73e3eb8b6ea35e28319c317eb4278117ad4abfe27a018e64` |
| `web/src/pages/task/ResearchTab.tsx` | `90efd8f22e07b855460162957548f386911b3bbe87a86f9a8c2fdb3a1e019178` |

## S6 检索部署检查（2026-11-06）

Windows amd64，本机真实 PostgreSQL，真实 WPS、mmdc、PaddleOCR。连接信息不进入报告。

| 检查 | 结果 |
| --- | --- |
| `go vet ./...` | 退出码 0 |
| `go test -race -count=1 -p 1 ./...` | 467 个测试 / 子测试通过，0 失败，0 竞态报告；4 项可选跳过（文档引擎长稳、真实模型契约、两个浏览器用例） |
| `npm --prefix web run build` / `test:unit` | 通过；94/94 |
| `tools/local-models/test_service.py` | 9/9（真实模型） |
| 真实组件故障演练 | 42 项通过（本地模型 + 3 份真实招标 DOCX，973 个分块） |
| 真实百炼切换与回滚 | 7 项通过，用量约 300 token |
| 旧库升级演练 | 通过；再次迁移幂等 |

模型报告：[`synthetic-local-models.json`](synthetic-local-models.json)。合成集的 nDCG 没有显示本地模型优于词法基线，不构成业务质量证据。默认并行运行时 `TestExportCancelDuringRealRefresh` 存在跨包竞争，见 [`AGENT_TOOL_SKILLS_PLAN.md`](../../../AGENT_TOOL_SKILLS_PLAN.md) 12.10。
