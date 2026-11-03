# F3a 固定解析闭环与升级恢复验收

日期：2026-10-31。范围：ZIP 子文件页面导入、已有文档重新解析预览与人工采用、独立数据库升级恢复演练。

**2026-11-01 已收口为 `[x] E`。** 两个真实导出专项及真实 PostgreSQL 全量竞态复验通过，`go vet`、前端单测与构建通过；此前浏览器与升级恢复验收仍按下文记录。下文保留 2026-10-31 的验收快照。

## 1. 页面行为

- 开启 `AUTOBID_FILE_PROCESSING_ENABLED=1` 并启动新版本服务后，在“招标包文件”页面使用“材料处理”。未开启时明确显示不可用原因，旧上传行为保持兼容。
- ZIP 上传仍先登记原件与子文件。页面可选择子文件、填写招标编号、标段、文件角色及公告日期，然后“导入所选文件”；不同效力的文件分批导入。成功、失败、重试及处理记录刷新后可重新读取。
- 新文件使用固定解析器直接入库，不需要额外的人工采用；重复请求返回同一记录，相同原件在同一任务/项目/标段下不重复建文档。
- 已有文档可“重新解析并预览”。候选在 PostgreSQL 中保存，预览显示新增/更新/删除分块、前后文本、受影响要求数量与解析警告；差异每页 20 块，单块文本最多 12,000 字符并明确标记截断。
- 用户核对后采用候选，保留文档 ID、原件哈希和旧正文/分块快照。版本过期、角色/日期变化、原件不可用或字节哈希不一致时拒绝采用；重复采用返回原收据。实质变化后提示进入要求清单，重新提取并复核；原有确认/导出门禁继续生效。
- 文件清单每页 200 条，处理记录每页 50 条，页面提供继续加载入口；候选全文不随列表响应返回。SSE 发出状态摘要，重连后以 GET 为准。

## 2. 代码与事务

- `db/file_processing.go`、`db/schema.sql`：新增过程域 `file_processing_records`，保存请求指纹、原件哈希、候选、预览、采用结果与操作人；请求键按租户/任务唯一，文件和目标文档使用同租户/任务复合外键。
- `filework/processing.go`：只调用现有固定解析器；浏览器不能提交正文、分块或 SQL。仅接收留存原件、文本快照及 ZIP 子文件，不允许把通用派生产物或脚本文件绕过复核导入。
- `ingest.PrepareTender` 在事务外解析，`PersistTender` 负责入库；`db.Transaction` 给现有存储方法提供事务内句柄，嵌套 `InTx` 加入同一事务。
- 导入的实体、关系、文档、分块、原件关联、清单失效及成功记录共用一个提交边界。修订的旧快照、分块差量、版本及采用记录也在一个事务提交。每个任务的固定处理提交阶段串行化，解析本身不占用该锁。
- 修复旧迁移回填：`bbox_meta.page_is_physical=false` 的表格分块不再被重复迁移补上虚构页码。

实际注册路由（均经过任务范围与现有 Bearer 鉴权）：

| 路由 | 用途 |
| --- | --- |
| `GET /api/tasks/{id}/files?after=0` | 原件和 ZIP 子文件分页清单、功能开关状态 |
| `POST /api/tasks/{id}/files/{fileID}/import` | 固定解析并导入，包含 `request_key`、招标编号/标段/角色/日期 |
| `POST /api/tasks/{id}/documents/{docID}/reparse` | 创建持久候选，包含 `request_key`、`expected_revision` |
| `GET /api/tasks/{id}/file-processing-records?before=0` | 处理记录分页 |
| `GET /api/tasks/{id}/file-processing-records/{recordID}?offset=0` | 候选差异分页、警告及是否失效 |
| `POST /api/tasks/{id}/file-processing-records/{recordID}/adopt` | 人工采用，包含 `expected_revision`、可选 `review_note` |

状态码：跨任务/不存在 404，版本或请求键冲突 409，解析/来源校验失败 422（解析失败保存记录并返回记录），功能关闭 503。数据库中途故障返回错误且整笔回滚，不留下成功记录或半成品。

## 3. 验证结果

| 验证 | 结果 |
| --- | --- |
| `go vet ./...` | 通过 |
| 真实 PostgreSQL：`go test -race ./server -run '^TestFixed' -count=1 -v` | 通过：导入、复合来源、角色/日期、幂等、请求冲突、跨任务、持久候选、过期拒绝、确认/导出门禁 |
| 导入及采用末尾故障注入 | 通过：实体、范围、文档、分块、快照、版本及成功记录无部分提交 |
| 同键四路并发、原件篡改、非 ZIP 派生产物拒绝 | 通过 |
| 浏览器：`TestFileProcessingBrowser`（真实 API + PostgreSQL + 构建后的 UI） | 3/3 通过：ZIP 上传/子文件导入/刷新；差异/刷新后采用/门禁；过期候选/移动端布局 |
| `npm --prefix web run test:unit` | 15/15 通过 |
| `npm --prefix web run build` | 通过；保留既有 bundle 体积提示 |
| 真实 PostgreSQL：`go test -race ./... -count=1` | **未全绿**：仅 `TestExportEndToEndDOCXAndPDF`、`TestCustomTemplateLayout` 因原导出引擎的本机运行环境故障失败，其余包通过 |

浏览器结果与截图在 `data/file-processing-browser/`；摘要留存于 [`eval/file-processing/reports/f3a-browser.json`](../../eval/file-processing/reports/f3a-browser.json)。


## 4. 独立升级与恢复演练

执行器：`cmd/file-migration-drill/main.go`。使用冻结提交 `e4bc8a510c775562f7be5d693cbd64310af4ea37` 的旧 schema 和代表性合成数据，不使用或修改当前工作库的数据。

```bash
# 使用已配置的 AUTOBID_TEST_DATABASE_URL；连接用户需能创建临时数据库。
go run ./cmd/file-migration-drill -pg-bin 'C:/Program Files/PostgreSQL/16/bin'
```

实际完成：

1. 单独创建旧库，填入原文、分块、已确认要求、清单和已核验证据，使用 `pg_dump` 保存备份。
2. 将备份恢复到另一个独立库，运行当前 `db.Migrate`，核对原文/分块/要求/证据与清单状态不变，旧文档原件关联仍为空、解析修订为 0。
3. 在独立空库执行迁移并重复执行，验证空库和幂等路径。
4. 把升级前备份恢复到独立回退库，核对旧 checksum 与旧字段结构；导出冻结提交的旧源代码，用其实际 `GetTenderDocument` 读取恢复后的旧库。
5. 在升级库导入留存 CSV，强制重放 schema，确认表格仍无物理页码；将当前库和原件目录分别备份、恢复到另一套库/目录，重读原件并验证 SHA-256 及文档关联。

上述 **7 项检查全部通过**。本次生成的 5 个数据库完成后已清理；合成备份、旧版本读取器及报告保留在 `data/file-processing-drill/20261031t095554_25480/`。摘要：[`f3a-recovery.json`](../../eval/file-processing/reports/f3a-recovery.json)。

**边界**：未提供真实历史生产备份，因此这是旧版本结构加代表性数据的恢复演练，不能冒充实际生产恢复或 V/P 验收。未修改生产库，也未做破坏性原地降级。

## 5. 后续阶段

F3a 原交付使用同步固定解析，不引入脚本 Runner。2026-10-31 经用户授权，异步固定处理、流式 `/files` 上传与必需材料门禁拆出至 F3a+，实施与验收见 [`F3A_PLUS_ACCEPTANCE.md`](F3A_PLUS_ACCEPTANCE.md)。Agent 脚本工具、脚本输出审查及其取消/恢复仍归后续 F2/F3b。容器与真实复杂样本关口不变；不宣称完整 F3、F4 或 V/P 已完成。
