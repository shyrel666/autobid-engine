# E 级端到端验收报告

> **结论级别：E（工程实现验收）。** 本报告只证明代码、PostgreSQL 状态机与门禁在固定虚构数据上按设计工作；不代表 V 级业务效果（真实招标文件与人工金标准），也不代表 P 级生产启用（权限、租户隔离、预算、监控与人工复核流程）。

## 1. 结论摘要

| 项目 | 结果 |
| --- | --- |
| 级别 | E（工程实现） |
| 检查结果 | 全部检查通过 |
| 通过 / 失败 / 延后 | 118 / 0 / 8 |
| 主链导出就绪 `ExportReadiness.Ready` | true |
| DOCX 导出任务 | succeeded |
| 两轮归一化一致性 | 一致 |

## 2. 运行环境

| 项 | 值 |
| --- | --- |
| 生成时间 | 2026-10-31T11:58:41+08:00 |
| Git commit | 37b4cc1 |
| Go 版本 | go1.27.0 |
| 数据库 | postgresql（schema_migrated=true；不记录连接串、主机、用户名或口令） |
| 夹具版本 | 1 |
| 运行 ID | E2E-20261030-235837 |

## 3. 主链路阶段表

| 阶段 | 检查 | 预期 | 实际 | 状态 | 说明 |
| --- | --- | --- | --- | --- | --- |
| main.setup | main.setup.create_task | task created | task created | PASS |  |
| main.setup | main.setup.origin_fact | 1 | 1 | PASS |  |
| main.ingest | main.ingest.tender_role | tender | tender | PASS |  |
| main.ingest | main.ingest.tender_chunks | >0 | 21 | PASS |  |
| main.ingest | main.ingest.tender_tables | >0 | 3 | PASS |  |
| main.ingest | main.ingest.tender_published | 2026-10-01T09:00:00+08:00 | 2026-10-01T09:00:00+08:00 | PASS |  |
| main.ingest | main.ingest.correction_role | correction | correction | PASS |  |
| main.ingest | main.ingest.correction_chunks | >0 | 1 | PASS |  |
| main.ingest | main.ingest.correction_later | T2>T1 | 2026-10-10T09:00:00+08:00 | PASS |  |
| main.census | main.census.tender_sections | 20 required codes | missing=0 | PASS |  |
| main.census | main.census.correction_section | 2.3.2 | missing=0 | PASS |  |
| main.census | main.census.table.1.2 | 1 table | table_count=1 | PASS |  |
| main.census | main.census.table.1.3 | 1 table | table_count=1 | PASS |  |
| main.census | main.census.table.3.1 | 1 table | table_count=1 | PASS |  |
| main.extract | golden.veto_total | 11 | 11 | PASS | by_kind=conformity=2,qualification=3,statutory=6 |
| main.extract | golden.veto.qualification | 3 | 3 | PASS |  |
| main.extract | golden.veto.conformity | 2 | 2 | PASS |  |
| main.extract | golden.veto.statutory | 6 | 6 | PASS |  |
| main.extract | golden.scoring_total | 3 | 3 | PASS |  |
| main.extract | golden.scoring_weights | 技术方案=50,投标报价=20,类似项目业绩=30 | 技术方案=50,投标报价=20,类似项目业绩=30 | PASS |  |
| main.extract | golden.mandatory_clause_total | 1 | 1 | PASS |  |
| main.extract | golden.non_scoring_required_total | 8 | 8 | PASS |  |
| main.extract | golden.price_out_of_scope | 1 | 1 | PASS |  |
| main.extract | golden.superseded | 1 | 1 | PASS |  |
| main.extract | golden.bid_deadline | 2026-11-20T09:00:00+08:00 | 2026-11-20T09:00:00+08:00 | PASS |  |
| main.confirm | main.confirm.status | confirmed | confirmed | PASS |  |
| main.confirm | main.confirm.epoch | >0 | 26 | PASS |  |
| main.veto | main.veto.all_passed | 11/11 | 11/11 failed=0 pending=0 | PASS |  |
| main.outline | main.outline.version | 1 | 1 | PASS |  |
| main.outline | main.outline.epoch | manifest epoch | 26 | PASS | 大纲 epoch 必须等于 S5 记录的清单 epoch |
| main.outline | main.outline.sections | 6 | 6 | PASS |  |
| main.outline | main.outline.mapped | 11 | 11/11 | PASS |  |
| main.outline | main.outline.approved | approved | approved | PASS |  |
| main.outline | main.outline.version_stable | 1 | 1 | PASS |  |
| main.outline | main.outline.epoch_stable | 26 | 26 | PASS |  |
| main.write | main.write.1 | applied | rev=1 | PASS |  |
| main.write | main.write.2 | applied | rev=1 | PASS |  |
| main.write | main.write.2.1 | applied | rev=1 | PASS |  |
| main.write | main.write.3 | applied | rev=1 | PASS |  |
| main.write | main.write.4 | applied | rev=1 | PASS |  |
| main.write | main.write.5 | applied | rev=1 | PASS |  |
| main.approve | main.approve.all | 6/6 | 6/6 | PASS |  |
| main.approve | main.approve.bound | 6/6 | 6/6 | PASS | 每章 approved_rev_no == current_rev_no |
| main.matrix | main.matrix.checked | 11 | 11 | PASS |  |
| main.matrix | main.matrix.verified | 11 | 11 | PASS |  |
| main.matrix | main.matrix.failed | 0 | 0 | PASS |  |
| main.matrix | main.matrix.pending | 0 | 0 | PASS |  |
| main.matrix | main.matrix.stale | 0 | 0 | PASS |  |
| main.consistency | main.consistency.contradictions | 0 | 0 | PASS |  |
| main.consistency | main.consistency.omissions | 0 | 0 | PASS |  |
| main.consistency | main.consistency.findings | 0 | 0 | PASS |  |
| main.readiness | main.readiness.ready | true | true | PASS |  |
| main.readiness | main.readiness.blockers | 0 | 0 | PASS |  |
| main.export | main.export.jobs | 1 | 1 | PASS |  |
| main.export | main.export.status | queued | queued | PASS |  |
| main.export | main.export.preflight | ready | true | PASS |  |
| main.export_readback | export.claim | running | running | PASS |  |
| main.export_readback | export.status | succeeded | succeeded | PASS |  |
| main.export_readback | export.artifact_size | matches task record | match | PASS |  |
| main.export_readback | export.artifact_hash | matches task record | match | PASS |  |
| main.export_readback | export.parse | no error | ok | PASS |  |
| main.export_readback | export.headings | 6 | 6 | PASS |  |
| main.export_readback | export.body | present | true | PASS |  |
| main.export_readback | export.toc | refreshed or warned | refreshed=false warnings=toc_not_refreshed | PASS | M1 只导出 DOCX；目录未刷新允许为 false，但必须记录 toc_not_refreshed 警告 |

## 4. 黄金期望核对表

| 期望项 | 黄金值 | 实际 | 状态 |
| --- | --- | --- | --- |
| golden.veto_total | 11 | 11 | PASS |
| golden.veto.qualification | 3 | 3 | PASS |
| golden.veto.conformity | 2 | 2 | PASS |
| golden.veto.statutory | 6 | 6 | PASS |
| golden.scoring_total | 3 | 3 | PASS |
| golden.scoring_weights | 技术方案=50,投标报价=20,类似项目业绩=30 | 技术方案=50,投标报价=20,类似项目业绩=30 | PASS |
| golden.mandatory_clause_total | 1 | 1 | PASS |
| golden.non_scoring_required_total | 8 | 8 | PASS |
| golden.price_out_of_scope | 1 | 1 | PASS |
| golden.superseded | 1 | 1 | PASS |
| golden.bid_deadline | 2026-11-20T09:00:00+08:00 | 2026-11-20T09:00:00+08:00 | PASS |

## 5. 门禁矩阵

| 编号 | 门禁 | 触发阻断 | 观察到的信号 | 修正后放行 | 完整恢复 ready |
| --- | --- | --- | --- | --- | --- |
| G01 | no_approved_outline | true | no_approved_outline | true |  |
| G02 | ErrRequirementsNotConfirmed | true | ErrRequirementsNotConfirmed | true |  |
| G03 | veto_pending | true | veto_pending,section_not_approved,matrix_unverified,consistency_not_run | true |  |
| G04 | veto_failed | true | veto_failed,section_not_approved,matrix_unverified,consistency_not_run | true |  |
| G05 | no_bid_decision | true | no_bid_decision,section_not_approved,matrix_unverified,consistency_not_run | true |  |
| G06 | no_approved_outline (requirement change) | true | no_approved_outline | true | true |
| G07 | section_not_approved | true | section_not_approved,matrix_stale,consistency_stale | true |  |
| G08 | matrix_failed | true | matrix_failed,consistency_stale | true |  |
| G09 | finding_open | true | finding_open | true | true |
| G10 | conflict_candidate | true | conflict_candidate | true |  |
| G11 | no_approved_outline (correction supersedes) | true | no_approved_outline | true | true |
| G12 | unknown_evidence | true | unknown_evidence | true |  |
| G13 | ExportBlockedError | true | ExportBlockedError: veto_pending | true |  |

### G01 检查明细

| 检查 | 预期 | 实际 | 状态 | 说明 |
| --- | --- | --- | --- | --- |
| G01.trigger | no_approved_outline | no_approved_outline | PASS |  |
| G01.remediated | no no_approved_outline | veto_pending,section_not_approved,matrix_unverified,consistency_not_run | PASS |  |

### G02 检查明细

| 检查 | 预期 | 实际 | 状态 | 说明 |
| --- | --- | --- | --- | --- |
| G02.trigger | ErrRequirementsNotConfirmed | ErrRequirementsNotConfirmed | PASS |  |
| G02.remediated | submitted | submitted | PASS |  |

### G03 检查明细

| 检查 | 预期 | 实际 | 状态 | 说明 |
| --- | --- | --- | --- | --- |
| G03.trigger | veto_pending | veto_pending,section_not_approved,matrix_unverified,consistency_not_run | PASS | 全部否决项保持 pending |
| G03.remediated | no veto_pending | section_not_approved,matrix_unverified,consistency_not_run | PASS |  |

### G04 检查明细

| 检查 | 预期 | 实际 | 状态 | 说明 |
| --- | --- | --- | --- | --- |
| G04.trigger | veto_failed | veto_failed,section_not_approved,matrix_unverified,consistency_not_run | PASS |  |
| G04.remediated | no veto_failed | section_not_approved,matrix_unverified,consistency_not_run | PASS |  |

### G05 检查明细

| 检查 | 预期 | 实际 | 状态 | 说明 |
| --- | --- | --- | --- | --- |
| G05.trigger | no_bid_decision | no_bid_decision,section_not_approved,matrix_unverified,consistency_not_run | PASS |  |
| G05.remediated | no no_bid_decision | section_not_approved,matrix_unverified,consistency_not_run | PASS | 已确认无 failed 否决项 |

### G06 检查明细

| 检查 | 预期 | 实际 | 状态 | 说明 |
| --- | --- | --- | --- | --- |
| G06.trigger | no_approved_outline | no_approved_outline | PASS |  |
| G06.old_outline | superseded | superseded | PASS |  |
| G06.new_draft | draft | draft | PASS |  |
| G06.epoch_grew | > | 27>26 | PASS |  |
| G06.unmapped | NEW-1 | true | PASS |  |
| G06.confirmed | confirmed | confirmed | PASS |  |
| G06.mapped_new | true | true | PASS | 编辑自动派生草案，把新要求映射到已有章节 |
| G06.approved | approved | approved | PASS |  |
| G06.coverage | 12/12 | 12/12 | PASS |  |
| G06.sections | 6/6 | written=6 approved=6 | PASS | 新大纲全部章节重新写回并批准 |
| G06.matrix | 12/12 | verified=12 failed=0 pending=0 | PASS |  |
| G06.consistency | 0 findings | findings=0 current=true | PASS |  |
| G06.full_ready | true | true | PASS | 新增要求后完整恢复 |

### G07 检查明细

| 检查 | 预期 | 实际 | 状态 | 说明 |
| --- | --- | --- | --- | --- |
| G07.trigger | section_not_approved | section_not_approved,matrix_stale,consistency_stale | PASS |  |
| G07.remediated | no section_not_approved | matrix_stale,consistency_stale | PASS |  |

### G08 检查明细

| 检查 | 预期 | 实际 | 状态 | 说明 |
| --- | --- | --- | --- | --- |
| G08.matrix_failed | 1 | 1 | PASS | 技术方案@2.1:failed 正文仅有章节名、要求复述或「详见」类引用，没有实质应答 |
| G08.trigger | matrix_failed | matrix_failed,consistency_stale | PASS |  |
| G08.remediated | verified | verified=1 failed=0 | PASS |  |

### G09 检查明细

| 检查 | 预期 | 实际 | 状态 | 说明 |
| --- | --- | --- | --- | --- |
| G09.matrix | 2 verified | verified=2 failed=0 | PASS |  |
| G09.trigger | contradiction>0 | contradictions=1 | PASS |  |
| G09.finding_open | finding_open | finding_open | PASS |  |
| G09.sections | 2/2 | 2/2 | PASS | 重新保存并批准受影响章节 |
| G09.remediated | no open finding | findings=0 | PASS |  |
| G09.full_ready | true | true | PASS | 统一工期承诺后完整恢复 |

### G10 检查明细

| 检查 | 预期 | 实际 | 状态 | 说明 |
| --- | --- | --- | --- | --- |
| G10.trigger | conflict_candidate | conflict_candidate | PASS | current_rev_no=1 |
| G10.remediated | applied | applied | PASS |  |

### G11 检查明细

| 检查 | 预期 | 实际 | 状态 | 说明 |
| --- | --- | --- | --- | --- |
| G11.superseded | 1 | 1 | PASS |  |
| G11.stale_link | old->new link | stale=true link=true | PASS |  |
| G11.trigger | no_approved_outline | no_approved_outline | PASS |  |
| G11.old_outline | superseded | true | PASS |  |
| G11.auto_v2 | maps new, not old | new=true old=false | PASS |  |
| G11.confirmed | confirmed | confirmed | PASS |  |
| G11.approved | approved | approved | PASS | 直接使用自动派生 v2，未再次调用 CreateOutline 或 UpdateOutline |
| G11.sections | 6/6 | written=6 approved=6 | PASS | 新大纲全部章节重新写回并批准 |
| G11.matrix | 11/11 | verified=11 failed=0 pending=0 | PASS |  |
| G11.consistency | 0 findings | findings=0 current=true | PASS |  |
| G11.full_ready | true | true | PASS | 更正覆盖后完整恢复 |

### G12 检查明细

| 检查 | 预期 | 实际 | 状态 | 说明 |
| --- | --- | --- | --- | --- |
| G12.known_kept | 7 | 7 | PASS |  |
| G12.trigger | 1 unknown_evidence gap | 1 | PASS |  |
| G12.remediated | 0 unknown_evidence | 0 | PASS |  |

### G13 检查明细

| 检查 | 预期 | 实际 | 状态 | 说明 |
| --- | --- | --- | --- | --- |
| G13.veto_pending | veto_pending | veto_pending | PASS |  |
| G13.trigger | ExportBlockedError | ExportBlockedError: veto_pending | PASS | veto_pending |
| G13.no_job | 0 | 0 | PASS |  |
| G13.remediated | 1 queued | jobs=1 status=queued | PASS |  |

## 6. 导出产物

| 类型 | 路径 | 大小 | SHA-256 | 说明 |
| --- | --- | --- | --- | --- |
| extraction_census | e2e-artifacts/extraction-census-r1.md | 4149 | `02f9a2b575bf0a934801e04a0fdde86bdc0c03ff919b1bb8ad2567d31149b871` | 分块普查表（编号/标题/内容摘要/提取要求） |
| export_docx | e2e-artifacts/exports/main-r1.docx | 8719 | `f92513760c7d233433435c72fc91a22fb2d32053b248d08285a12286e04d47b7` | 主链路导出的技术标 DOCX（M1 未启用 LibreOffice） |
| tender_docx | e2e-artifacts/fixtures/tender.docx | 3223 | `914a66b484352da06e44b8e9ec0e3cc97f5b3c5471b2496a6344732ee1390877` | 确定性生成的虚构招标 DOCX |
| correction_docx | e2e-artifacts/fixtures/correction.docx | 1914 | `625a9907477c564c2beb702f08eff0679106b4fc2172f1f7a1e65f2132d54941` | 确定性生成的虚构更正公告 DOCX |
| g09_docx | e2e-artifacts/fixtures/g09-consistency.docx | 1902 | `6f372f150a98ec70cdc9baa8685b76c173b6b86af866a775c9c414b7ae6b886a` | G09 一致性冲突专用夹具 DOCX |
| company_profile | eval/bid/fixtures/company/company-profile.json | 349 | `d813d4cae700bac0bf26bd07ab9b5ab1431d5bf21d78d98d586d2c66c571d1cc` | M1 只生成不消费的虚构企业资料 |
| company_credentials | eval/bid/fixtures/company/credentials.json | 457 | `d54eeea81cabe4db55b705ca3a2809829d2049062d9389860daf276c3d6b0250` | M1 只生成不消费的虚构资质资料 |
| company_performance | eval/bid/fixtures/company/performance.json | 440 | `dfe16dda64625b3bd66bc98902d4e9a9657ed5a959af3fc1a9691f8f53e58aec` | M1 只生成不消费的虚构业绩资料 |

- DOCX 回读：PASS（ok）。
- 目录刷新：refreshed=false warnings=toc_not_refreshed。

## 7. 延后项（BLOCKED）

| 能力 | 状态 | 原因 |
| --- | --- | --- |
| D2.evidence | BLOCKED | 证据检索与绑定、过期证书、子公司业绩、无证据业绩：依赖 D2 证据库，M1 只有虚构企业文件且不消费 |
| evidence_binding | BLOCKED | 虚拟企业 JSON 仅在 M1 生成、未导入知识库、未绑定证据，不能宣称证据链路已覆盖 |
| D2.commitment | BLOCKED | 承诺低于评分阈值、报价不一致、报价与商务文件冲突、型号错误与参数低于阈值、承诺台账：依赖 D2/D3，M1 未实现 |
| D3.writing | BLOCKED | 真实大模型写作质量与句子级事实核验：M1 使用确定性替身写作，只验流程 |
| 6B.pdf | BLOCKED | PDF 导出与 LibreOffice 目录页码刷新：M1 只导出 DOCX，目录未刷新仅记警告 |
| V.accuracy | BLOCKED | 真实项目/企业的准确率与召回率：无真实材料与人工标注，不能做 V 级 |
| 4.4.multi_agent | BLOCKED | 多 Agent 检索与写作：无稳定收益证据前保持关闭 |
| API.UI | BLOCKED | HTTP REST 路由、SSE 与浏览器端到端操作：本执行器直接调用 Go 业务接口，API/UI 需另行验收 |

## 8. 复现命令与边界声明

```bash
# 1. 静态检查
go vet ./...

# 2. 真实 PostgreSQL 测试（连接串只从环境变量读取）
#    AUTOBID_TEST_DATABASE_URL 已由运行环境提供
go test -race ./...

# 3. 端到端验收（报告写到项目根目录）
go run ./eval/bid/accept -report-dir . -artifacts-dir ./e2e-artifacts -verify-determinism
```

**边界声明**：本执行器直接调用 `db`、`ingest`、`export` 与 `bidtools`，覆盖 Go 后端主链、PostgreSQL 状态机与核验门禁；不覆盖 HTTP REST 路由、SSE 与浏览器操作。合成数据结果不代表真实投标文件的写作质量，也不代表抽取准确率或召回率已达到业务门槛。执行器只生成本次 E2E 运行证据，不代替外部 `go vet` 与 `go test -race` 结果；这两项须按第 8 节命令在具备真实 PostgreSQL 的环境中单独执行。M1 使用确定性替身写作，不调用真实大模型；证据检索与绑定、承诺台账、报价核验、PDF 导出与多 Agent 均列为 BLOCKED。

## 附录 A：提取普查表

分块普查表随本次运行生成于 `e2e-artifacts/extraction-census-r1.md`，记录每个分块的编号、标题、内容摘要、表格数与提取出的要求编码。

## 附录 B：确定性与清理

- 两轮归一化比较：一致
- 任务清理：removed（26 个任务）。已按 tasks.id 精确删除；未 DROP 表、未删除其他任务、未清理全局资产。推演空间（explorations）不随任务删除，保留为历史记录，不影响下一次运行

> 本报告与生成代码中不包含数据库连接串、真实企业、真实项目或个人信息；全部项目与企业名称均为虚构演示标识。
