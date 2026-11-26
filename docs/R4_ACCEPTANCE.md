# R4 章节上下文与工作量基线

2026-11-22，Windows amd64，Intel Core i7-12700H，Go 1.27.0，本机真实 PostgreSQL。样本由基准测试生成。

## 章节上下文

章节写作入口按 `proposal_sections.current_rev_no` 读取主修订，复用 `(tenant_id, section_id, rev_no)` 唯一索引。大纲版本、章节依赖、当前修订、故事板与写作核验仍在同一数据库快照中读取。HTTP 章节作业与 Agent 工具共用该路径，章节详情与修订历史仍返回全部版本。回归用例验证主修订之后有冲突候选稿时，写作上下文保留正确的主修订与大纲版本，详情可见候选稿，其他租户无法读取该上下文。

以下为三次独立运行的 `ns/op` 和 `B/op` 中位数。每次至少测量 300 毫秒。测试夹具从正常审批流程建立主修订，另加 80 份约 36 KB 的候选正文，候选稿不推进主修订指针。

| 场景 | 改动前 ms/op | 改动后 ms/op | 改动前 B/op | 改动后 B/op |
| --- | ---: | ---: | ---: | ---: |
| 1 份主修订 | 1.33 | 1.23 | 58,449 | 58,481 |
| 1 份主修订 + 80 份候选稿 | 12.96 | 1.31 | 3,478,506 | 58,284 |

## 其他工作量基线

| 工作量 | 合成输入与计算范围 | ms/op | B/op |
| --- | --- | ---: | ---: |
| 原生 DOCX 解析 | 250 段，`ingest.Parse` | 0.63 | 395,897 |
| 原生 PDF 解析 | 20 页，每页 25 行，`ingest.Parse` | 6.31 | 8,767,015 |
| 招标原文翻页 | 长分块、4,096 字节预算，经 `read_chunk` 读取前两页 | 0.97 | 197,423 |
| PostgreSQL 词法召回 | 256 分块，查询并读取 20 条候选 | 4.79 | 54,336 |
| 模型 HTTP 适配 | OpenAI 兼容请求编码、本机替身 HTTP 往返、响应和用量解码 | 0.18 | 88,013 |

PostgreSQL 词法召回测量候选查询，完整的检索审计与证据快照另有开销。模型适配测量的上游是本机替身。扫描件 OCR、真实生成模型、生产规模并发、检索与写作业务收益仍待各自的数据与运行环境验证。本轮对现有查询读取范围做了优化，后续查询索引或模型更换应依据对应工作量和保留集评测决定。

## 工程验收

`go vet ./...` 和真实 PostgreSQL 下的 `go test -race -count=1 -json ./...` 通过，共 529 条测试与子测试通过、0 失败。使用了 WPS、Mermaid CLI 与 PaddleOCR。前端构建与 118 项单测通过。另跑浏览器完整写标流程（34/34）与公开招标文件流程（4/4）。全量日志纳入本地 `data/no-real-material-checks/`。

## 复跑

在独立测试 PostgreSQL 上设置 `AUTOBID_TEST_DATABASE_URL`，从仓库根目录执行。

```bash
go test ./bidtools -run '^TestSectionWritingContextUsesCurrentRevisionWithConflictCandidates$' -bench '^BenchmarkR4' -benchmem -benchtime=300ms -count=3
go test -race -count=1 ./bidtools -run 'Test(SectionWritingContextUsesCurrentRevisionWithConflictCandidates|ReadChunkToolPaginationBudget|EvidenceVerifyAndBindToSectionContext)$'
```

测试会创建并清理自己的项目和词法检索夹具。历史对照以 `506d706` 为基线，仅把基准测试文件复制到该对照工作区运行；基准可与平台原有的检索业务评测独立复跑。本机原始测量日志保存在 `data/r4-checks/`。
