# Phase 4.1～4.4 实施与验证记录

2026-10-28。工程能力已落地；**真实业务验收未完成，生产多 Agent 默认关闭**。用户确认尚无真实人工标注语料、embedding 或 reranker 服务。合成回归不能替代业务验收。

## 已实现

| 阶段 | 交付 | 未满足的业务条件 |
| --- | --- | --- |
| 4.1 | 数据集校验、家族隔离、等价证据分组、A/B/C/D/E 执行器、分层召回与排序指标、延迟、人工复核指标接口 | 至少 200 条真实人工标注题、独立复核、冻结部署规模/并发/质量/成本预算 |
| 4.2 | 共享 PostgreSQL 词法/精确向量检索、RRF、HTTP embedding/reranker、索引构建/切换/回滚、缓存与降级 | 真实模型部署、B/C 对 A 的业务评测、领域同义词实验、目标负载测试 |
| 4.3 | 持久化证据、原文快照、来源失效、限定范围的数值核验、单 Worker 最多两轮补查、API 和页面回溯、长表格分页 | 复杂语义人工复核、D/C 的业务质量与成本对比 |
| 4.4 | Planner/Worker 模型复用、独立调用上下文、最多 3 个证据 Worker、共享预算、持久化租约和幂等恢复、终态门禁、取消 | E/D 复杂问题集多次实验；有稳定收益前不开启 |

工程入口：`retrieval/`、`db/retrieval*.go`、`agent/retrieval.go`、`bidtools/retrieval.go`、`server/retrieval.go`、`cmd/retrieval-eval/`。DDL 仅经 `db/schema.sql` 和 `db.Migrate` 执行。

## 复现实测

仅使用专用测试 PostgreSQL 的 `AUTOBID_TEST_DATABASE_URL`，不读取生产连接串。下列命令会向新建的随机 `eval-*` 租户写入合成文档，结束后清理该租户的数据。报告保留每题排名、错误、降级与指标；其中 run ID 为临时审计标识，清理后不能再通过 API 读取。

```bash
go run ./cmd/retrieval-eval -seed -groups A,B,C,D,E -repeat 3 \
  -out eval/retrieval/reports/synthetic-local.json
```

数据集 `synthetic.json` 共 32 题、8 个文档家族、16 个分块。7 个家族的 28 题为保留集，剩余 4 题为调参集；每次执行默认只跑保留集。**这是极小的合成工程回归集，型号过滤常将候选缩减至两个分块，因此 Recall@20 很容易达到 1，不构成语义质量证据。**

已归档结果：[`reports/synthetic-local.json`](reports/synthetic-local.json)。未配置模型时 B 明确 skipped；D/E 未配置 `-profile-id` 时明确 skipped；C 记录 `semantic_model_unconfigured` / `reranker_unconfigured`，实质是词法降级路径。A 与 C 的耗时口径不同：C 包含运行和证据持久化。C 的排序指标未优于 A，不能据此声称方案已提升业务质量。不能以检索返回零条来自动推导无答案识别得分。

真实语料可使用相同清单结构，标明权限依据、不可变快照及 `synthetic:false`；证据 ID 为 `document.key:seq`，使用既有隔离租户时为 `doc_id:seq`。语料有 `documents` 时校验 gold 的家族归属。已有租户通过 `-tenant <非default租户>` 执行；不使用 `-seed` 时不清理其数据。报告记录实际分块指纹集合哈希，便于比较是否使用了同一语料快照。

部署模型并创建现有 LLM Profile 后，使用 `-profile-id <id> -groups D,E`。D/E 共享 24 次调用、48000 预留 token、90 秒预算；E 的实验授权仅限本次 CLI，不改变生产开关。D/E 返回证据集合按首次捕获排序，不能把其 nDCG 当成与 B/C 同义的最终排序；任务正确性必须复核原文和缺口。所有消融组应使用同一冻结语料、模型及执行配置。 B 单独使用与 A 相同的 `title-path-body-v1` 预处理、候选池和 0.5/0.3/0.2 加权，只更换 embedding；C 使用增强预处理与 RRF/rerank。执行器会构建两个版本索引，避免将纯向量检索冒充 B。

人工复核可通过 `-judgments labels.json` 输入 JSON 数组。每条必须含 `group`、`case_id`、`repeat`、`reviewer`、`note`；可选字段为 `citations`/`supported_citations`、`predicted_unanswerable`、`fully_correct`、`mandatory_false_positive`、`cost_usd`。没有标注的指标输出 null；报告显示已复核观察数。每个正确任务成本只有在全部执行均有正确性及成本记录时计算，分子包含失败任务成本。不得用预留 token 当作实际账单。

## 模型接入与版本切换

服务器读取以下环境变量；URL 是完整接口路径。下面是配置示例，不代表已经部署了服务。

```bash
export AUTOBID_EMBEDDING_URL=http://127.0.0.1:8081/embed
export AUTOBID_EMBEDDING_MODEL=your-embedding-model
export AUTOBID_EMBEDDING_REVISION=immutable-model-revision
export AUTOBID_EMBEDDING_DIMENSION=1024
export AUTOBID_EMBEDDING_FORMAT=tei
# AUTOBID_EMBEDDING_API_KEY 通过环境安全注入，不写进仓库
export AUTOBID_RERANKER_URL=http://127.0.0.1:8082/rerank
export AUTOBID_RERANKER_MODEL=your-cross-encoder-model
export AUTOBID_RERANKER_REVISION=immutable-model-revision
export AUTOBID_RERANKER_FORMAT=tei
# AUTOBID_RERANKER_API_KEY 同上
# AUTOBID_RETRIEVAL_MULTI_AGENT 默认为 false，业务对比通过后才设为 true
```

`tei` 请求格式依据 [Hugging Face TEI 官方文档](https://huggingface.co/docs/text-embeddings-inference/quick_tour)。也支持 `compatible`：embedding 的 `data[{index,embedding}]`、rerank 的 `results[{index,relevance_score}]`。服务必须提供对应契约，不能把任意模型的生成接口当作 rerank。模型名称和 revision 为配置声明，部署方须固定实际权重修订；接口不验证服务器权重文件。响应校验数量、索引、维度、有限数及非零向量；HTTP 默认 20 秒、无自动重试/重定向。

1. 配好模型，启动服务器，知识库页面 →「语义索引」→ 按模型实际维度构建。
2. 检查 `ready` 后激活。构建与激活分开；文档 epoch、模型、维度、预处理和 embedding 覆盖率须一致。
3. 修改模型修订/维度/预处理时重新构建；同一文档 epoch 的旧索引可再次激活回滚。旧文档快照的索引拒绝激活。
4. 构建为同步分页任务；中断留下的 building 记录不参与检索，可重新构建。它不是后台自动续建服务。

预处理版本 `title-path-metadata-body-v2` 包含标题、章节路径、产品/版本元数据及正文，均为原始数据，无 LLM 派生上下文。保留旧 HashEmbedder 仅用于原有入库/消融 A；语义通道不会静默使用 HashEmbedder 冒充模型。当前使用 PostgreSQL `float8[]` 精确余弦；评测报告记录 pgvector 可用性，不创建 HNSW，也不宣称 ANN 性能。

## 运行、预算与核验边界

- 搜索每路召回 50，RRF 常数 60，融合最多 100 条重排；搜索最多返回 50 条，捕获前 12 条证据。评测 Top-20 排名与最终证据集合明确分开。表格偏好只作通道内同分排序，不覆盖相关性。
- 搜索预算 4 次调用、64000 预留 token、45 秒；research 预算如上。调用包括逻辑源检索、embedding、rerank、规划/补查模型。模型调用按 UTF-8 输入字节的保守 token 上界预留，reasoner 另预留 2048 输出及 framing；失败不退额度。已知 reasoner 用量写入 `actual_tokens`，embedding/rerank 实际账单尚不统一采集。
- 单次预算内的查询由 singleflight 去重；跨运行仅共享不可变成功缓存，避免把一个运行的付费调用归到另一个预算。缓存键包含租户、过滤器、索引 epoch、模型和策略，TTL 1 分钟，上限 128 条；返回深拷贝，降级结果不缓存。调用取消会传递至模型 HTTP。
- research 子任务租约 20 秒并续租，最多 2 次领取；崩溃后重发同一 `request_key` 和原请求恢复。已完成子任务不重跑；最多再尝试一次丢失响应的外部调用，费用已预留且不退款，不能保证第三方请求恰好一次。需要重发请求才接管，没有后台轮询自动重启模型调用。原截止时间不延长。
- 来源分域存储。知识库强制 tenant 过滤；招标按 task 过滤；Phase 7 前资信/招标仅允许 default 租户。HTTP 固定 default，当前仍是单团队 Bearer Token 鉴权，不代表已实现 RBAC。
- `verified` 仅指明确型号的产品规格/白皮书标量比较：支持指定单位换算和 `<=`/`>=`/`=`。历史承诺、错型号、过期/缺少指定有效期、否定/条件语境、模糊值、跨列或合并单元格均不自动判满足。复杂语义、招标/资信关系保留待复核，不提供可绕过核验的“人工通过”开关。
- 自由文本问题及多范围结果保留 partial。`completed` 仅允许单范围、问题与显式结构化主张文本相同、全部证据支持且无缺口；它不证明整份标书合规。来源更新/删除会将证据标 stale，并撤销已完成 research；知识库读取再次核对来源指纹，资信读取检查可访问性和过期。
- `kb_read_chunk` 保持小分块旧格式；大文本返回 UTF-8 字节 `next_text_offset`，大表格返回 `table_page` 和原始 `row_offset/next_row`，保留表头、跨页位置与合并单元格原坐标。继续分页应携带 `expected_hash`；单行超预算报错，不截断数值冒充完整表格。

## 已登记真实路由

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET/POST | `/api/knowledge/indexes` | 列表/同步构建 |
| POST | `/api/knowledge/indexes/{indexID}/activate` | 激活/回滚 |
| GET | `/api/retrieval/runs/{runID}` | 请求、预算与结果快照 |
| GET | `/api/retrieval/runs/{runID}/evidence` | 当前证据有效性 |
| POST | `/api/retrieval/runs/{runID}/evidence/{evidenceID}/verify` | 结构化指标核验 |
| POST | `/api/retrieval/runs/{runID}/cancel` | 持久化取消、租约作废 |
| GET | `/api/tasks/{id}/retrieval-runs` | 最近 100 个任务检索记录 |
| POST | `/api/tasks/{id}/research` | 有界检索/同键恢复 |

Agent 使用 `kb_read_evidence`、`kb_verify_evidence`、`kb_research`；旧 `kb_search_*` 调用共享服务。知识库页展示降级、原文和指标核验；任务页提供检索、记录、继续和取消。

## 工程验证

真实 PostgreSQL 上执行：索引构建/切换/回滚/过期拒绝，来源删除失效、错型号/单位/条件/表格歧义、租户隔离、缓存拷贝、模型故障降级、预分配预算、并发协调、幂等重放、重启租约接管、旧持有者写回拒绝、终态门禁和取消。HTTP 契约测试覆盖证据核验、来源删除和多 Agent 默认拒绝。

检查命令：`go vet ./...`、`go test -race ./... -count=1`、`cd web && npm run build`。Windows cgo 编译器使用官方 [w64devkit v2.10.0](https://github.com/skeeto/w64devkit/releases/tag/v2.10.0) 解压到忽略的 `tmp/phase4-toolchain`，已核对 GitHub release 的 SHA-256 `18d0a4c71a166f8401ab6305781bec5882b40b5e06ba9807c61cb5f3b3c6325e`；仅本次进程设置 `CGO_ENABLED=1` 与 `CC`，未修改全局 PATH。最终结果见同目录 `reports/engineering-checks.md`。
