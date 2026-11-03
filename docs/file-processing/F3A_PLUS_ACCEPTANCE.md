# F3a+ 固定处理增强实施与验收

实施日期：2026-10-31；**2026-11-01 已收口为 `[x] E`**。固定处理专项与浏览器验证已通过，本次原安装 LibreOffice 真实导出、真实 PostgreSQL 全量竞态、`go vet`、前端单测与构建复验通过；[复验结果及可选跳过项](../LIBREOFFICE_VALIDATION.md)。F2/F3b 脚本执行未启动，F4/V/P 没有推进。

## 交付范围

- 必需材料可先登记后上传，保存名称、是否必需、原件绑定、版本、操作人和说明；取消必需标记须填写说明，历史快照写入 `task_material_events`。状态由服务端派生，不接受浏览器声明“完成”。
- `required_material_incomplete` 阻断缺失、不可用、排队/处理、待采用修订、未正式导入的必需材料。非必需失败不一律阻断。ZIP 绑定具体子文件；清单不自动推断招标包的全部应收附件，仍需人工登记与复核。
- 材料变更、固定处理提交与导出创建共用任务行串行化边界；`file_gate_version` 使等待期间旧的 REPEATABLE READ 快照产生重试，防止旧快照漏拦。导出 `gate` 快照包含当时的材料清单及状态。
- 新 `POST /files?filename=...` 接收原始请求体，64 MiB 上限，边写边算 SHA-256，成功登记后返回原件。没有 HTTP 整文件 `io.ReadAll`。固定解析阶段仍有界读入内存；不宣称全链路流式解析。
- 新 `fixed_file_jobs`/`fixed_file_job_events` 是固定处理队列与审计，输入、哈希、请求指纹、截止时间和次数上限不可修改；没有脚本内容或容器执行器。请求重复返回同一作业，不同输入同键返回 409。
- 默认 2 个 worker（`AUTOBID_FILE_WORKERS`，最多 8）；每任务最多 100 个待处理作业；每作业最多 3 次尝试、提交后 15 分钟截止；租约 30 秒、约 10 秒续租。进程重启/租约丢失由轮询恢复，次数及原截止时间不重置。解析失败为终态，显式重试使用新请求键并保留旧记录；不是无限自动重试。
- 文档、实体、分块、候选/处理记录及作业成功状态在同一事务提交；旧令牌拒绝写回。ZIP 展开先校验全部清单，再事务登记子文件；未提交的物理对象按既有孤立对象 GC 清理。
- 暂停持久化取消排队/在途作业；恢复不复活已取消作业。API 暂停/删除在本进程等待 worker 退出，领取与注册间隙有二次租约检查；其他实例依靠数据库栅栏与续租取消。解析器的取消响应受其检查 context 的频率影响，不能将 Go goroutine 当成硬进程隔离。
- 页面有“必需材料清单”“上传原件后处理”“后台处理作业”、失败重试和取消。ZIP 后台展开；子文件导入与重新解析走后台队列，修订仍须人工采用。原同步 `/documents`、`/files/{fileID}/import`、`/documents/{docID}/reparse` 保留兼容。
- GET 为状态权威来源；SSE 刷新提示，页面每 2 秒补查作业/材料状态。原有 `source_epoch`、要求确认与下游失效门禁继续生效。

## 实际新增路由

| 路由 | 行为 |
| --- | --- |
| `POST /api/tasks/{id}/files?filename=...` | 原始字节流上传；201 原件，超限 413 |
| `GET /api/tasks/{id}/materials` | 材料清单及派生状态 |
| `POST /api/tasks/{id}/materials` | 新建或按 `id/version` 更新；操作说明与审计 |
| `POST /api/tasks/{id}/file-jobs` | 固定导入、重新解析或 ZIP 展开；202 作业 |
| `GET /api/tasks/{id}/file-jobs?before=...` | 每页 50 个作业 |
| `GET /api/tasks/{id}/file-jobs/{jobID}` | 任务范围内查询 |
| `POST /api/tasks/{id}/file-jobs/{jobID}/cancel` | 持久化取消，当前令牌失效 |

全部路由沿用现有 Bearer 鉴权与服务端默认租户；不宣称已实现 Phase 7 RBAC。解析输出只能由固定处理器生成，不能从浏览器提交正文替代。

## 验证证据

- 真实 PostgreSQL + `-race`：材料缺失/导入/候选/采用/失效、非必需排除说明、版本冲突与审计、同键重发/冲突、跨任务、租约回收/次数上限、取消/暂停、旧执行者写回拒绝、末尾故障注入整笔回滚、材料变更与导出并发。
- 浏览器真实 API + 独立 PostgreSQL schema：4/4 通过，覆盖原有 ZIP/修订/过期候选，以及新原件上传→ZIP 后台展开→登记/绑定必需材料→后台导入→刷新后门禁状态；含 390px 移动端无页面横向溢出。
- 前端单测 15/15、构建通过（保留既有 bundle 体积提示）。
- 独立旧库升级、空库/重复迁移、备份恢复、旧版本读取器与原件 SHA-256 回读演练通过；`data/file-processing-drill/20261031t160119_24588/report.json`。使用合成代表数据，不是生产恢复演练。
- 全量回归发现早期暂停可能导致首次会话没有历史，此时 `ExecuteWithMessage` 的人工续跑指令没有进入 fresh input；已修复，并增加 `TestEngineMessageWithoutSavedHistory`。两个暂停恢复用例连续三轮通过。
- 本地详细日志：`data/fixed-*.log`、`data/fixed-full-tests-final.jsonl`；浏览器截图与报告：`data/file-processing-browser/`。最终摘要见 `eval/file-processing/reports/f3a-plus.json`。

## LibreOffice 历史阻塞（2026-10-31）

原安装 `F:/LibreOffice/program/python.exe` 仍返回 `0xc0000135`。对同包 CPython 解释器进行隔离加载可成功 `import uno`，但真实导出在创建 `com.sun.star.frame.Desktop` 时出现 `Binary URP bridge disposed during call`，未到打开文档阶段。

另从 Document Foundation 官方镜像清单取得 26.2.6 MSI，核对 SHA-256 `f9877032fd908beb9c0ddf06df4af5c2e85f419c42e14876c4cce5aae5fb2660` 后以 `/a` 在 `data/libreoffice-26.2.6/` 隔离展开；仍复现 UNO 桥断开。尝试私有 profile、TCP/pipe、预初始化与重连未使端到端导出通过。未修改系统安装或永久环境变量，也未把未经验证的启动绕行写入默认配置。最终仅保留 Python traceback 和有界错误尾部，帮助定位失败调用点。

当时的两个阻塞用例为 `TestExportEndToEndDOCXAndPDF` 与 `TestCustomTemplateLayout`，因此未标完整 E。2026-11-01 原安装首次探测即正常，两个用例在专项与全量中均实际通过；没有删除、跳过或放宽用例。此前故障根因未确定，安装包及隔离副本继续保留供追溯。本阶段仅收口 E，不代表 V/P。
