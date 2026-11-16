# R2 可信身份与项目角色验收记录（E 级，本机）

验收日期：2026-11-16。对应 [RELIABILITY_PLAN.md](RELIABILITY_PLAN.md) 的 R2，与 [ROADMAP.md](../ROADMAP.md) Phase 7 中单租户内的那一部分。运维说明见 [OPERATIONS.md](OPERATIONS.md) 第 10 节。

本记录只证明工程实现（E）。样本是合成的，浏览器验收的模型是脚本替身（`web/e2e/stub_llm.py`）。没有做渗透测试、没有第三方安全评审，也没有在多人真实协作中试用（V / P 未开始）。

## 1. 声明范围

R2 之前，系统里所有“谁做的”都来自请求头 `X-Autobid-User`，页面上叫“当前操作人”，由使用者自己填写；粉队 / 红队的作者与批准人分离、提交检查清单的确认人、解析候选的采用与拒绝、索引的启用与回滚都按这个字符串判定。R2 之后：

- **身份只来自服务端核验过的会话。** 账号用口令登录，服务端在 PostgreSQL 里登记会话（只存令牌的 SHA-256），会话 Cookie 为 HttpOnly、SameSite=Strict。请求头、请求体、查询参数都不能指定或改变“我是谁”；`X-Autobid-User` 不再有任何含义，`AUTOBID_API_TOKEN` 已废弃（设置时只在启动日志告警）。运行时没有“信任请求头”或匿名模式的开关。
- **没有匿名模式，也没有默认账号。** `/api` 下除登录接口外全部要求会话；健康检查与前端静态页面除外。第一个管理员只能由有本机访问权的人用 `ops.bat user create-admin` 创建。
- **记录里写的是账号标识。** 各 `*_by` 列写入 `users.login`：创建后不可修改（数据库触发器冻结）、租户内唯一。显示名单独保存，可以修改。
- **职责分离按账号判定。** 粉队批准、红队定稿、承诺会签都比较账号。AI 代写的正文（作者 `ai:<派发人>`）算派发人写的。升级前留下的声明式姓名被标记出来，不能当作“另一个人”，除非作者重新保存，或管理员显式映射到账号。
- **项目成员与角色。** 每个项目有成员表，四类角色（投标经理、技术撰写人、商务专员、审核专家）可以兼任；另有账号级的管理员。全部 208 条路由在一张声明式的权限表里写明所需权限，默认拒绝；非成员访问项目返回 404，角色不足返回 403 并带 `code`。
- **质量策略显式化。** 任务带版本化的质量策略 `grounded/v1` / `legacy/v1`，写作门禁只读它，不再从“有没有故事板记录”推断；修改由投标经理填写原因并留痕。
- **变更只追加留痕。** 账号、成员、角色、质量策略、遗留姓名映射的每次变更写入只追加的事件表，数据库拒绝修改和删除。
- **Agent 不继承任何人的角色。** Planner / Worker 仍只受 `IntentMandate` 的对象范围约束，没有新增任何审批类工具。`AUTOBID_AGENT_EXPORT_ENABLED` 仍默认关闭，不做按任务授权。

不在范围内：

- 多租户隔离（`tenants` 表、`tasks.tenant_id`、跨租户 403 / 404）。所有账号属于 `default` 租户，Phase 7 保持未完成。
- 单点登录、LDAP、OAuth，短信或邮件找回口令。忘记口令由本机管理员用 `ops.bat` 重置。
- V / P 状态不变。

## 2. 设计要点

### 2.1 会话

| 项目 | 做法 |
| --- | --- |
| 口令 | argon2id（`golang.org/x/crypto/argon2`，64 MiB、3 轮、并行 2），PHC 编码串；长度 10~256 字符且不能等于账号。账号不存在时也做一次同等代价的校验，避免按响应时间探测账号 |
| 会话令牌 | 32 字节随机数，Cookie 里是原文，数据库 `auth_sessions.token_hash` 只存 SHA-256 |
| 有效期 | 空闲 8 小时（使用中顺延）、绝对 72 小时，可配置；登出、停用账号、修改 / 重置口令时，该账号的会话在同一事务内吊销 |
| Cookie | `autobid_session`，HttpOnly、SameSite=Strict、Path=/，不设 Expires；HTTPS 下加 Secure |
| 防跨站请求伪造 | 非 GET 请求须带 `X-Autobid-CSRF`，值为会话令牌的 HMAC-SHA256（服务端按 Cookie 重算，不落库）；浏览器带了 `Origin` 时还须与 Host 一致 |
| SSE | 走同一 Cookie，URL 不再带任何令牌 |
| 登录限速 | 账号连续失败 5 次后锁定 30 秒，之后每次翻倍、最长 15 分钟（`users.failed_logins / locked_until`）；同一来源 5 分钟内失败 20 次也暂时拒绝（进程内计数）；运维重置口令同时解锁 |
| 明文传输 | `AUTOBID_TLS_CERT` / `AUTOBID_TLS_KEY` 开启 HTTPS；监听非回环地址而未配置时启动告警 |
| 共享令牌 | `AUTOBID_API_TOKEN` 删除：设置时只告警。持有它不能代表任何账号，也不能绕过登录 |

改动面：鉴权中间件把已核验的身份放进请求上下文，`operator(r)` 从上下文读账号标识，70 多处调用不变；存储层继续接收 `actor string`，`eval/bid/*` 直接调用存储层不受影响。

### 2.2 遗留姓名

升级前的“当前操作人”是使用者随手填的字符串。参与职责分离判定的只有三张表的作者类列：故事板的创建人与提交人、章节修订的作者、承诺台账的创建人。迁移给这三张表加 `actor_legacy` 列，已有行填 `true`、之后默认 `false`；数据库触发器不允许把已清除的标记重新设回。

判定函数 `separationPrincipalQ` 把记录还原为“是谁”：`ai:<派发人>` 还原为派发人；`actor_legacy` 的记录只有在管理员通过 `legacy_actor_mappings` 把那个姓名映射到账号后才算数，否则返回 `ErrLegacyActor`（HTTP 409 `legacy_actor`）。这意味着：

- 张三升级前以“张三”提交了故事板，升级后用账号 `zhangsan` 登录。映射之前，`zhangsan` 和其他任何人都不能批准这份故事板；张三重新保存后，记录变成账号 `zhangsan` 写的，按账号判定。
- 即使升级前填的名字恰好与某个账号相同（例如也叫 `zhangsan`），也不算数，必须映射。
- 映射与撤销都写 `user_events`。升级前已经作出的批准不回溯撤销。

### 2.3 质量策略

`tasks.quality_policy` 取代布尔列 `grounded_writing_required`：

| 策略 | 含义 |
| --- | --- |
| `grounded/v1` | 故事板、粉队 / 红队、句子级写作核验强制生效（新任务默认） |
| `legacy/v1` | 不启用 D3 门禁；不能新建故事板或写作核验记录（`ErrLegacyPolicyActive`，HTTP 409 `quality_policy_legacy`）；正文里的受控引用仍展开并核验，但不持久化核验记录 |

迁移：原 `grounded_writing_required = true`、或已有故事板 / 写作核验记录的任务映射为 `grounded/v1`，其余为 `legacy/v1`，与升级前的实际行为一致。旧模式任务里新建故事板的处理定为“拒绝，要求投标经理先切换策略”。已有 D3 记录的任务不能降回 `legacy/v1`，因此 `legacy/v1` 下永远没有 D3 记录，门禁不必再看记录。每次修改写 `quality_policy_events`（原策略、新策略、原因、操作人）。

### 2.4 后台作业

章节写作、图表、导出、解析作业在提交时检查权限并记下提交人，之后按记录的身份执行完毕，即使提交人已登出或被停用。AI 稿的作者记为 `ai:<提交人>`。

## 3. 最终权限矩阵

角色：投标经理（M）、技术撰写人（W）、商务专员（C）、审核专家（R）；“成员”指本项目任一角色。表格由 `server/permissions.go` 的 `routePerms` 整理，测试 `TestEveryRouteHasPermission` 保证它与实际注册的路由一一对应。

按业务归纳：

| 业务 | 读 | 写 / 操作 |
| --- | --- | --- |
| 项目与执行 | 成员 | 创建项目：任何登录账号（自动成为 M）；启动 / 暂停 / 恢复 / 删除、加目标：M；提示、单工作槽控制：M、W；模型链：M |
| 成员与质量策略 | 成员（管理员也可读成员） | 成员：M 或管理员；质量策略：M |
| 招标文件与解析 | 成员 | 上传、重新解析、导入子文件、后台解析作业：M、W；**采用 / 拒绝解析候选：M**；材料清单：M、W、C |
| 要求清单与否决项 | 成员 | 提取、确认清单、目标分解、否决项、投标范围、投标决策：M；条目增删改：M、W |
| 大纲与章节 | 成员 | 大纲起草 / 提交：M、W；**批准大纲：M**；派发 AI、保存修订、回滚：W；**红队定稿：R**；取消写作作业：M、W |
| 故事板与写作核验 | 成员 | 编写 / 提交 / 受控引用渲染：W；**粉队批准 / 退回：R**；运行写作核验：W、R |
| 证据、承诺与报价 | 成员 | 证据绑定：W、C；可采性评估：W、C、R；**可采性人工复核：R**；承诺录入：C；承诺确认：M、C；**承诺会签：M、R**；报价录入 / 校验：C；报价确认：M、C；客观分：M、W、C |
| 响应矩阵与一致性 | 成员 | 核验与运行：W、R；**一致性结论处理：R** |
| 图表 | 成员 | W |
| 导出与提交 | 成员 | 预览稿：M、W；**最终稿：M**（处理函数内附加判定）；取消作业：M、W；取消请求组：M；**提交检查清单确认 / 撤回：M** |
| 企业资信库（租户级） | 任何登录账号 | 在任一项目中担任 C，或管理员 |
| 企业方案知识库（租户级） | 任何登录账号 | 文档上传 / 删除：在任一项目中担任 M 或 W，或管理员；**索引构建 / 启用 / 回滚 / 删除：管理员** |
| 导出模板（租户级） | 任何登录账号 | 在任一项目中担任 M，或管理员 |
| 检索运行 | 属于项目时按项目成员 | 取消：M、W；证据核验：M、W、R |
| 工具调用拦截 | 本人所属项目的待审批 | 审批：所属项目的 M；规则与配置：管理员 |
| 模型配置、模型录制、账号管理、遗留姓名映射 | 管理员 | 管理员 |
| 运行能力、版本、模型健康、系统目录 | 任何登录账号 | — |

要点：

- 管理员身份本身不能读项目内容（不是成员时项目路由一律 404）；它能给任何项目管理成员，这是唯一的例外，操作留痕。
- 作者与批准人分离仍按每个对象判定。兼任审核专家的撰写人不能批准自己的故事板或正文；AI 代写的正文，派发人不能定稿。

逐条路由见附录 A。

## 4. 环境与方法

| 项目 | 版本 / 配置 |
| --- | --- |
| 代码 | 基线 master `4cac800`（D4），R2 改动在分支 `r2-identity` |
| 系统 | Windows 11 Pro 26300，已登录桌面会话 |
| 数据库 | PostgreSQL 16.15；浏览器套件与升级演练各自新建并删除隔离库 |
| 文档引擎 | WPS（`AUTOBID_OFFICE_ENGINE=auto`） |
| 图表 / OCR | Mermaid-CLI 11.17.0 + Google Chrome；PaddleOCR 3.7.0（PP-OCRv5 mobile，CPU） |
| 模型 | 脚本替身，不是真实模型 |
| 工具链 | Go 1.27.0、Node.js 22.22.1 |

方法：

- 验收性质的测试都用生产的会话鉴权在真实 PostgreSQL 上跑：`server/auth_test.go` 里的服务端用例真实登录、带 Cookie 与防跨站令牌发请求；存储层用例直接验证事件表只追加、会话吊销、遗留姓名判定。
- 其他与登录无关的服务端测试用测试替身鉴权（`server/testauth_test.go` 的 `testAPI`，身份取测试专用请求头 `X-Test-Actor`）。替身只存在于 `_test.go`，不编译进服务程序；生产的 `NewAPI` 只会装配 `sessionAuth`，`TestR2SessionIdentity` 对此做了断言。
- 浏览器套件由 Go 测试启动真实服务进程：先用 `autobid-ops user create-admin` 建管理员，再由管理员接口建三个账号，各账号在各自的浏览器上下文里真实登录。切换“谁在操作”就是换一个已登录的页面。
- 升级演练用 `8b48a4b` 的真实旧程序造历史数据（含一份以“张三”声明提交的故事板），由当前程序升级后走一遍“建第一个管理员 → 建账号 → 加成员 → 处理遗留姓名”。

## 5. 结果

### 5.1 必须成立的性质与对应测试

全部在真实 PostgreSQL 上运行，结果为“跑出来的”。

| # | 性质 | 测试 | 结果 |
| --- | --- | --- | --- |
| 1 | 身份只来自服务端核验的凭据 | `server.TestR2SessionIdentity`：带 `X-Autobid-User`、`X-Test-Actor`、`X-Forwarded-User`、`Remote-User` 与 `?user=` / `?operator=` 冒充管理员，创建的项目成员、质量策略事件的操作人与会话身份都是登录账号；请求体里的 `created_by` 被拒绝（未知字段）。`cmd/upgrade-drill` 里 `zhangsan` 带 `X-Autobid-User: lisi` 批准自己的故事板仍被判为自批 | 通过 |
| 2 | 没有匿名模式与默认账号 | `TestR2SessionIdentity`：`/api/tasks`、`/api/auth/session`、`/api/runtime/version`、活动流、`/api/llm/health` 及未登录的写请求一律 401；旧共享令牌、身份请求头、`?token=` 都不能通过；12 组常见默认账号口令登录失败；`schema.sql` 不含任何 `INSERT INTO users`。升级演练：升级后 `users` 表为空，第一个管理员只能由 `autobid-ops user create-admin` 创建 | 通过 |
| 3 | 运行时没有信任请求头的开关 | 代码：`NewAPI` 只装配 `sessionAuth`，替身在 `_test.go`；`TestR2SessionIdentity` 断言生产构造的鉴权类型；没有任何环境变量改变鉴权方式 | 通过（含读代码） |
| 4 | `*_by` 写不可变、租户内唯一的账号标识 | `db.TestR2AccountsSessionsAndEvents`：改 `users.login` 被触发器拒绝、改显示名不影响登录名；大小写不同的同名账号冲突；非法登录名（含中文、空格）被拒。`TestR2BackgroundJobKeepsSubmitter`、`s7-flow` 断言修订作者 / 定稿人 / 清单确认人都是账号标识 | 通过 |
| 5 | 职责分离按账号判定，遗留姓名不算“另一个人” | `db.TestR2SeparationPrincipals`、`db.TestR2SeparationOfDutiesInStorage`、`db.TestD3StoryboardPinkReviewAndWritingVerification`：AI 稿派发人不能红队定稿；未映射的声明式作者让粉队批准、红队定稿、承诺会签都返回 `ErrLegacyActor`；与账号同名的旧声明也不算数；映射后本人不能批准、他人可以；撤销映射后重新不算数。升级演练用真实旧程序数据复验 | 通过 |
| 6 | 权限默认拒绝、逐路由声明，非成员 404、角色不足 403 + code | `server.TestEveryRouteHasPermission`：208 条注册路由全部在表里，表里没有失效条目，项目路由都是项目权限，只有 3 条公开路由；未声明的路由恒返回 403。`server.TestR2RolePermissions`：非成员 404（含列表过滤、来源项目），角色不足 403 `forbidden_role` 并列出所需角色，管理员非成员读不到项目但能管成员，最终稿只给投标经理，租户级资料库按“任一项目的角色”放行，最后一名投标经理不能移除 | 通过 |
| 7 | 会话存库、只存哈希、过期与立即失效、Cookie 与防跨站、SSE 同会话 | `db.TestR2AccountsSessionsAndEvents`：数据库里找不到令牌原文；过期、登出、改口令、停用后会话失效，停用账号不能再建会话。`TestR2SessionIdentity`：Cookie 为 HttpOnly、SameSite=Strict、明文 HTTP 下无 Secure；缺令牌、令牌错误、别的会话的令牌、跨站 Origin 都 403；SSE 用 Cookie 连通；登出后保留旧 Cookie 重放 401；改口令后其他会话立即 401、本次请求换发新会话、旧口令不能登录；停用后会话 401 | 通过 |
| 8 | argon2id、登录限速、口令与令牌不外泄 | `TestR2LoginThrottle`：连续失败后锁定，即使口令正确也 429 并带 `Retry-After`；运维重置口令解锁；同一来源大量失败后 429。错误响应不回显口令；`user_events` 里没有口令、口令哈希或 argon2 字样。口令从不进入模型请求，`llmrec` 录制的只有模型调用（读代码） | 通过（录制部分为读代码） |
| 9 | 账号、成员、角色、质量策略变更写只追加事件 | `TestR2AccountsSessionsAndEvents`、`TestR2ProjectMembers`：创建、改显示名、改口令、停用、映射 / 撤销都有事件；`user_events`、`project_member_events` 的 UPDATE / DELETE 被触发器拒绝；`quality_policy_events` 记录创建时与每次修改 | 通过 |
| 10 | Agent 不继承角色、没有审批类工具 | `bidtools.TestR2NoApprovalToolsForAgents`：全部常驻、延迟与章节撰写工具名中没有批准、会签、确认、采用 / 拒绝、启用、回滚、成员、角色、策略、账号类动作 | 通过 |
| — | 后台作业按提交时的身份跑完 | `server.TestR2BackgroundJobKeepsSubmitter`：撰写人派发 AI 写作后被管理员停用（其会话随即 401），作业照常完成，修订作者为 `ai:<撰写人>` | 通过 |

### 5.2 工程回归

| 检查 | 结果 |
| --- | --- |
| `go vet ./...` | 通过 |
| 真实 PostgreSQL `go test -race -count=1 ./...`（真实 WPS、mmdc、PaddleOCR） | 516 条测试 / 子测试通过，0 失败，0 竞态报告，30 个包通过（D4 时为 512 条）。增减：新增 10 条（存储层 4、服务端 5、Agent 工具 1），删除 `TestOperatorHeader`（1 条 + 5 个子测试，它测的是已删除的请求头身份）。13 项跳过与 D4 相同：9 项是单独运行的浏览器套件与升级演练（见 5.3、5.4）、`TestOfficeLargeDocumentAndSoak` 与 `TestLiveModelContract` 为需显式开启的可选用例、`TestHelperHoldLock` 是文件锁子进程辅助、`TestCCGPRealTendersRequirementsAndOutline` 因工作树没有被 Git 忽略的语料目录而跳过（R2 未改动解析，本次没有补跑） |
| R2 性质用例 `go test ./db ./server ./bidtools -run 'R2\|EveryRoute'` | 全部通过（见 5.1） |
| 前端 `npm --prefix web run build` | 通过（保留既有 bundle 大于 500 kB 提示） |
| 前端 `npm --prefix web run test:unit` | 118/118 通过（D4 时 114；新增成员页纯逻辑 3 条、遗留姓名与旧策略提示 1 条，改写评审人分离提示 1 条） |
| D2 验收执行器 `go run ./eval/bid/d2 -verify-determinism` | 通过 95，失败 0，主链 ready=true |
| D3 验收执行器 `go run ./eval/bid/d3 -verify-determinism` | 通过 95，失败 0，主链 ready=true |
| M1 验收执行器 `go run ./eval/bid/accept` | 通过 118，失败 0 |

三个验收执行器直接调用存储层、传入任意 `actor` 字符串，不经过登录；结果与 D4 相同，说明存储层签名与判定在非升级数据上没有变。

### 5.3 浏览器验收

`go test ./e2e`（每个套件新建隔离库，构建并启动真实服务进程，先用 `autobid-ops user create-admin` 建管理员、再由管理员接口建三个账号；真实 WPS / Mermaid / PaddleOCR，模型为脚本替身；截图在 `data/s7-checks/<套件>/`）：

| 套件 | 结果 | 与 R2 有关的内容 |
| --- | --- | --- |
| 全流程 `s7-flow` | 34/34 通过 | 新增第 3 步，改写 9 步，见下 |
| 真实招标 `s7-real` | 通过 | 投标经理一人完成上传到批准大纲 |
| 既有场景与实时运行 `scenarios`（6/6）、`live-run`（6/6） | 通过 | “令牌失效”场景改为“会话失效”：编辑正文时会话在别处被登出，保存得到 401，弹出重新登录，草稿保留，登录后保存成功且作者为登录账号；模型配置改用管理员 |
| Agent `s7-agent` | 通过 | 脚本修正见 5.5 第 3 项 |
| 关闭能力 `s7-capabilities` | 通过 | “令牌”一步改为：未登录只显示登录页、错误口令提示、无会话时带 `X-Autobid-User` 或任意 Bearer 令牌的接口与活动流都 401 |
| 崩溃恢复 `s7-recovery` | 通过 | 杀进程重启后会话仍有效（存在数据库里）；索引构建改由管理员提交 |
| 阻断种类契约 `TestEveryExportBlockerHasAPageAction` | 通过 | R2 没有新增阻断种类 |

全流程由三个账号在各自的浏览器上下文里完成：投标经理丙（创建项目，只有投标经理角色）、撰写人甲（技术撰写人、商务专员、审核专家）、评审人乙（审核专家）。与 R2 有关的步骤：

- 第 2 步：新项目的质量策略为 `grounded/v1`，创建人的角色只有投标经理。
- 第 3 步（新增）：评审人乙加入前读取项目得到 404；投标经理在“成员与质量策略”页添加撰写人甲与评审人乙，成员变更记录显示；评审人乙的页面显示自己的账号。
- 第 10~15 步：撰写人甲兼任审核专家，能看到“粉队批准”按钮，但批准自己的故事板被拒绝（作者与批准人分离按账号判定）；评审人乙在自己的页面批准。
- 第 16 步：AI 派发后的修订作者为 `ai:e2e-author`。
- 第 22~27 步：评审人乙定稿，定稿人记为账号 `e2e-reviewer`。
- 第 30 步：最终稿由投标经理提交。
- 第 33 步：评审人乙在自己的页面确认提交检查清单项，被拒绝并提示“需要：投标经理”；投标经理逐项处理后，7 条确认记录的确认人都是投标经理的账号。

`go test ./server -run 'Browser$'`：材料处理（4/4）与模型设置（5/5）两个套件通过。它们用测试替身鉴权，测的是页面功能而不是登录。

### 5.4 升级演练

`AUTOBID_S7_DRILL=1 go run ./cmd/upgrade-drill`：14 项全部通过（59 秒；D4 时 12 项，本次新增 2 项）。用 `8b48a4b` 的真实旧程序创建历史数据与真实成稿，另在第二个项目里以请求头 `X-Autobid-User: 张三` 编写并提交一份故事板；备份并校验后由当前程序迁移到数据库代际 3：

- 历史行（17 张表的全部旧列，含 `tasks.updated_at`）与全部产物哈希不变，重复启动幂等。
- 没有故事板的历史项目映射为 `legacy/v1`，有故事板的映射为 `grounded/v1`；升级前的那份故事板被标为声明式姓名；`users` 表为空（没有预置账号）。
- （新增）用 `autobid-ops user create-admin --password-stdin` 创建第一个管理员并登录；添加成员前，管理员读旧项目得到 404；管理员建三个账号并给两个旧项目加成员。
- （新增）`zhangsan`、`lisi` 批准“张三”的故事板都得到 409 `legacy_actor`；管理员把“张三”映射到 `zhangsan` 后，`zhangsan` 即使带 `X-Autobid-User: lisi` 批准仍得到 422 `pink_self_approval`；`lisi` 批准成功，`approved_by` 记为 `lisi`。
- 升级后新提交的严格最终稿与旧作业并存；浏览器以投标经理登录读历史项目；恢复与拒绝写入非空库 / 非空目录的检查通过。

### 5.5 过程中发现并修复的问题

| # | 问题 | 怎么发现的 | 处理 |
| --- | --- | --- | --- |
| 1 | 质量策略回填会触发 `tasks` 的 `updated_at` 触发器，所有历史项目的修改时间被改成升级时刻 | 升级演练的历史快照比对 | 回填期间暂停该触发器 |
| 2 | 开发服务器的 `/api` 代理改写 Host，浏览器的 Origin 与 Host 不一致，所有写请求被当作跨站拒绝 | 读代码（`vite.config.ts` 的 `changeOrigin: true`） | 代理不改写 Host；OPERATIONS.md 写明前置反向代理须保留 Host |
| 3 | Agent 套件偶发失败（两次全量运行加单独复跑共 17 次，失败 6 次）：替身模型按固定顺序调用工具，先提交的解析作业恰好在图表提交之前结束时，宿主按设计先投递作业事件并拦下这次工具调用，脚本不会重试，于是没有图表作业。同一基线 `4cac800` 连跑 6 次未复现，但失败原因与 R2 改动无关（失败的是 `steerHooks` 的作业事件投递，未经过鉴权） | 最终树全量浏览器验收 | 脚本把图表与导出提交各重发一次（同一请求键，已执行时只是重放），并断言图表作业被受理；修正后连跑 10 次全部通过 |
| 4 | 全流程新加的成员一步结束时，投标经理的页面停在成员页，下一步“刷新后上传原件”找不到上传控件 | 第一次全量浏览器验收 | 成员一步结束时回到招标文件页 |

## 6. 升级后的行为变化

1. **必须登录。** 页面先显示登录页；会话失效时弹出“登录已失效”，重新登录同一账号后，页面上未保存的编辑（包括章节草稿）保留；改用其他账号则整页重载、丢弃未保存内容。页面不再有“令牌设置”和“当前操作人”输入框，右上角是账号菜单（修改口令、退出登录）。
2. **旧项目没有成员。** 升级后除管理员外谁都看不到旧项目，管理员也只能管成员。要先建管理员、建账号、给每个旧项目加成员（OPERATIONS.md 10.4）。
3. **按角色放行。** 原来任何人都能做的操作现在按角色限制，例如批准大纲、确认清单、提交最终稿、处理提交检查清单只给投标经理；粉队 / 红队批准只给审核专家；模型配置、拦截规则、知识库索引的启用与回滚只给管理员。
4. **AI 稿算派发人写的。** 派发人不能再红队定稿自己派发的 AI 正文。查过现有测试与浏览器流程：没有依赖这条宽松行为的（`s7-flow` 一直由另一个人定稿）。升级前已经由派发人自批的 AI 正文，导出门禁会显示“红队审批人与作者相同”，需要另一位审核专家重新定稿。
5. **遗留姓名不算数。** 依赖升级前声明式作者的粉队批准、红队定稿、承诺会签返回 `legacy_actor`，需要作者重新保存或管理员映射。升级前已作出的批准不撤销。
6. **质量策略显式化。** 没有故事板的旧项目为 `legacy/v1`，不能新建故事板，要先由投标经理改为 `grounded/v1`；以前“在旧项目里建一个故事板就自动启用门禁”的隐含行为没有了。已有故事板的项目为 `grounded/v1`，不能降回。
7. **受控引用在 `legacy/v1` 下不再生成写作核验记录。** 以前在旧模式项目里保存含受控引用的正文会留下一条核验记录、并因此让整个项目启用门禁；现在仍展开并核验（不通过照样拒绝保存），但不留记录、不改变策略。
8. **提交检查清单只有投标经理能确认。** 以前任何填了“当前操作人”的人都能确认。
9. **项目列表按成员过滤。** 管理员看到全部项目的名称与状态，用于管理成员。
10. **`AUTOBID_API_TOKEN` 不再起作用。** 设置时启动告警；依赖它的脚本要改为账号登录。

## 7. 边界

- 单租户：所有账号属于 `default` 租户，没有 `tenants` 表与 `tasks.tenant_id`，跨租户隔离没有做，Phase 7 未完成。新代码从登录身份取租户，但大多数既有处理函数仍写死默认租户。
- 招标静态要素（`/api/assets/{assetID}/edges`）按招标编号跨项目共享，任何登录账号可读。
- 来源地址的登录限速在进程内，重启清零；账号锁定在数据库里。
- 没有口令复杂度规则，只有长度与“不等于账号”；没有口令过期策略；没有多因素认证。
- 会话 Cookie 不设 Expires，浏览器关闭即丢弃，需要重新登录。
- 前端按角色隐藏按钮只做了少数位置（粉队评审按钮、账号管理与系统设置导航），其余按钮对无权角色仍可见、点击后由服务端拒绝并说明所需角色。
- 管理员能读模型录制，录制里有所有项目的提示与正文；这与“管理员身份本身不能读项目内容”在录制这一处不一致，是有意保留的运维能力。
- 承诺台账的创建人是首次创建者，后续编辑者不改变它；会签判定只比较创建人，与升级前一致。
- HTTPS 只验证了配置开关与告警（读代码），没有在局域网里用证书实测。
- 没有渗透测试与第三方安全评审。

## 8. 复跑命令

```powershell
$env:CGO_ENABLED = '1'; $env:CC = 'gcc'
$env:AUTOBID_MMDC_PATH = "$PWD\tools\mermaid-cli\node_modules\@mermaid-js\mermaid-cli\src\cli.js"
$env:AUTOBID_MMDC_BROWSER = 'C:\Program Files\Google\Chrome\Application\chrome.exe'
$env:AUTOBID_OCR_URL = 'http://127.0.0.1:18765/ocr'      # 先启动 tools\paddleocr\service.py
go vet ./...
go test -race -count=1 ./...
go test ./db ./server ./bidtools -run 'R2|EveryRoute' -v -count=1     # R2 性质
npm --prefix web run build; npm --prefix web run test:unit

$env:PLAYWRIGHT_MODULE = "$env:LOCALAPPDATA\ms-playwright-go\1.57.0\package"
$env:BROWSER_EXECUTABLE = "$env:LOCALAPPDATA\ms-playwright\chromium_headless_shell-1223\chrome-headless-shell-win64\chrome-headless-shell.exe"
go test ./e2e -v -count=1 -timeout 180m
go test ./server -run 'Browser$' -v -count=1
$env:AUTOBID_S7_DRILL = '1'; go run ./cmd/upgrade-drill
go run ./eval/bid/d2 -verify-determinism; go run ./eval/bid/d3 -verify-determinism; go run ./eval/bid/accept
```

原始日志在 Git 忽略的 `data/r2-checks/`，浏览器套件的截图在 `data/s7-checks/<套件>/`，升级演练的证据在 `data/s7-checks/upgrade/`。日志不含连接串、口令与会话令牌。

## 附录 A：逐条路由权限

| 路由 | 所需权限 |
| --- | --- |
| `GET /healthz` | 无需登录 |
| `/` | 无需登录（前端静态产物与 SPA 回退） |
| **登录与账号** | |
| `POST /api/auth/login` | 无需登录 |
| `POST /api/auth/logout` | 任何登录账号 |
| `GET /api/auth/session` | 任何登录账号 |
| `POST /api/auth/password` | 任何登录账号 |
| `GET /api/users/directory` | 任何登录账号 |
| `GET /api/users` | 管理员 |
| `POST /api/users` | 管理员 |
| `PATCH /api/users/{userID}` | 管理员 |
| `POST /api/users/{userID}/password` | 管理员 |
| `GET /api/user-events` | 管理员 |
| `GET /api/legacy-actors` | 管理员 |
| `POST /api/legacy-actors` | 管理员 |
| `DELETE /api/legacy-actors/{name}` | 管理员 |
| `GET /api/tasks/{id}/members` | 项目成员；非成员的管理员也可 |
| `PUT /api/tasks/{id}/members/{login}` | 投标经理；非成员的管理员也可 |
| `DELETE /api/tasks/{id}/members/{login}` | 投标经理；非成员的管理员也可 |
| `GET /api/tasks/{id}/member-events` | 项目成员；非成员的管理员也可 |
| `GET /api/tasks/{id}/quality-policy` | 项目成员 |
| `PUT /api/tasks/{id}/quality-policy` | 投标经理 |
| **任务生命周期与执行控制** | |
| `GET /api/tasks` | 任何登录账号（处理函数只列出本人所属项目（管理员看到全部项目的元数据）） |
| `POST /api/tasks` | 任何登录账号（创建人成为该项目的投标经理） |
| `GET /api/tasks/{id}` | 项目成员 |
| `DELETE /api/tasks/{id}` | 投标经理 |
| `POST /api/tasks/{id}/start` | 投标经理 |
| `POST /api/tasks/{id}/pause` | 投标经理 |
| `POST /api/tasks/{id}/resume` | 投标经理 |
| `POST /api/tasks/{id}/revive` | 投标经理 |
| `GET /api/tasks/{id}/coverage` | 项目成员 |
| `POST /api/tasks/{id}/goals` | 投标经理 |
| `POST /api/tasks/{id}/hints` | 投标经理、技术撰写人 |
| `POST /api/tasks/{id}/works/{intentID}/control` | 投标经理、技术撰写人 |
| `POST /api/tasks/{id}/works/{intentID}/steer` | 投标经理、技术撰写人 |
| `POST /api/tasks/{id}/works/{intentID}/message` | 投标经理、技术撰写人 |
| `GET /api/tasks/{id}/overview` | 项目成员 |
| `GET /api/tasks/{id}/graph` | 项目成员 |
| `GET /api/tasks/{id}/nodes/{nodeID}` | 项目成员 |
| `GET /api/tasks/{id}/findings` | 项目成员 |
| `GET /api/tasks/{id}/assets` | 项目成员 |
| `GET /api/assets/{assetID}/edges` | 任何登录账号（招标静态要素按招标编号共享，不属于单个项目） |
| `GET /api/tasks/{id}/report` | 项目成员 |
| `GET /api/tasks/{id}/events` | 项目成员 |
| `GET /api/tasks/{id}/agent-jobs` | 项目成员 |
| `GET /api/tasks/{id}/llm-chain` | 项目成员 |
| `PUT /api/tasks/{id}/llm-chain` | 投标经理 |
| `POST /api/tasks/{id}/llm-chain/reset` | 投标经理 |
| `GET /api/tasks/{id}/llm-usage` | 项目成员 |
| `GET /api/tasks/{id}/intercept-audit` | 项目成员 |
| **招标文件、材料与解析** | |
| `POST /api/tasks/{id}/documents` | 投标经理、技术撰写人 |
| `GET /api/tasks/{id}/documents` | 项目成员 |
| `POST /api/tasks/{id}/documents/{docID}/reparse` | 投标经理、技术撰写人 |
| `POST /api/tasks/{id}/files` | 投标经理、技术撰写人 |
| `GET /api/tasks/{id}/files` | 项目成员 |
| `POST /api/tasks/{id}/files/{fileID}/import` | 投标经理、技术撰写人 |
| `GET /api/tasks/{id}/file-processing-records` | 项目成员 |
| `GET /api/tasks/{id}/file-processing-records/{recordID}` | 项目成员 |
| `POST /api/tasks/{id}/file-processing-records/{recordID}/adopt` | 投标经理 |
| `POST /api/tasks/{id}/file-processing-records/{recordID}/reject` | 投标经理 |
| `GET /api/tasks/{id}/materials` | 项目成员 |
| `POST /api/tasks/{id}/materials` | 投标经理、技术撰写人、商务专员 |
| `POST /api/tasks/{id}/file-jobs` | 投标经理、技术撰写人 |
| `GET /api/tasks/{id}/file-jobs` | 项目成员 |
| `GET /api/tasks/{id}/file-jobs/{jobID}` | 项目成员 |
| `POST /api/tasks/{id}/file-jobs/{jobID}/cancel` | 投标经理、技术撰写人 |
| **企业资料库（租户级）** | |
| `GET /api/company/credentials` | 任何登录账号 |
| `POST /api/company/credentials` | 管理员，或在任一项目中担任 商务专员 |
| `PUT /api/company/credentials/{credID}` | 管理员，或在任一项目中担任 商务专员 |
| `DELETE /api/company/credentials/{credID}` | 管理员，或在任一项目中担任 商务专员 |
| `GET /api/company/performance` | 任何登录账号 |
| `POST /api/company/performance` | 管理员，或在任一项目中担任 商务专员 |
| `DELETE /api/company/performance/{perfID}` | 管理员，或在任一项目中担任 商务专员 |
| `GET /api/company/evidence/{domain}/{ownerID}/originals` | 任何登录账号 |
| `POST /api/company/evidence/{domain}/{ownerID}/originals` | 管理员，或在任一项目中担任 商务专员 |
| **企业方案知识库与检索** | |
| `GET /api/knowledge/documents` | 任何登录账号 |
| `POST /api/knowledge/documents` | 管理员，或在任一项目中担任 投标经理、技术撰写人 |
| `GET /api/knowledge/documents/{kid}` | 任何登录账号 |
| `DELETE /api/knowledge/documents/{kid}` | 管理员，或在任一项目中担任 投标经理、技术撰写人 |
| `GET /api/knowledge/search` | 任何登录账号 |
| `GET /api/knowledge/indexes` | 任何登录账号 |
| `GET /api/knowledge/retrieval-status` | 任何登录账号 |
| `GET /api/knowledge/index-events` | 任何登录账号 |
| `POST /api/knowledge/indexes` | 管理员 |
| `POST /api/knowledge/indexes/{indexID}/activate` | 管理员 |
| `POST /api/knowledge/indexes/{indexID}/cancel` | 管理员 |
| `DELETE /api/knowledge/indexes/{indexID}` | 管理员 |
| `POST /api/tasks/{id}/research` | 投标经理、技术撰写人 |
| `GET /api/tasks/{id}/retrieval-runs` | 项目成员 |
| `GET /api/retrieval/runs/{runID}` | 所属项目的成员（对象不属于项目时：任何登录账号） |
| `POST /api/retrieval/runs/{runID}/cancel` | 所属项目的投标经理、技术撰写人（对象不属于项目时：任何登录账号） |
| `GET /api/retrieval/runs/{runID}/evidence` | 所属项目的成员（对象不属于项目时：任何登录账号） |
| `POST /api/retrieval/runs/{runID}/evidence/{evidenceID}/verify` | 所属项目的投标经理、技术撰写人、审核专家（对象不属于项目时：任何登录账号） |
| **要求清单、否决项与投标范围** | |
| `GET /api/tasks/{id}/requirements` | 项目成员 |
| `POST /api/tasks/{id}/requirements/extract` | 投标经理 |
| `POST /api/tasks/{id}/goals/bootstrap` | 投标经理 |
| `POST /api/tasks/{id}/requirements/confirm` | 投标经理 |
| `POST /api/tasks/{id}/requirements` | 投标经理、技术撰写人 |
| `PUT /api/tasks/{id}/requirements/{reqID}` | 投标经理、技术撰写人 |
| `DELETE /api/tasks/{id}/requirements/{reqID}` | 投标经理、技术撰写人 |
| `PUT /api/tasks/{id}/requirements/{reqID}/veto` | 投标经理 |
| `PUT /api/tasks/{id}/requirements/bid-scope` | 投标经理 |
| `PUT /api/tasks/{id}/requirements/bid-decision` | 投标经理 |
| **大纲与章节写作** | |
| `GET /api/tasks/{id}/outlines` | 项目成员 |
| `POST /api/tasks/{id}/outlines` | 投标经理、技术撰写人 |
| `GET /api/tasks/{id}/outlines/{version}` | 项目成员 |
| `PUT /api/tasks/{id}/outlines/{version}` | 投标经理、技术撰写人 |
| `POST /api/tasks/{id}/outlines/{version}/submit` | 投标经理、技术撰写人 |
| `POST /api/tasks/{id}/outlines/{version}/approve` | 投标经理 |
| `GET /api/tasks/{id}/sections/{sectionID}` | 项目成员 |
| `POST /api/tasks/{id}/sections/{sectionID}/dispatch` | 技术撰写人 |
| `POST /api/tasks/{id}/sections/{sectionID}/revisions` | 技术撰写人 |
| `POST /api/tasks/{id}/sections/{sectionID}/revisions/{revNo}/approve` | 审核专家 |
| `POST /api/tasks/{id}/sections/{sectionID}/revisions/{revNo}/rollback` | 技术撰写人 |
| `GET /api/tasks/{id}/section-jobs` | 项目成员 |
| `POST /api/tasks/{id}/section-jobs/{jobID}/cancel` | 投标经理、技术撰写人 |
| **D3 故事板与写作核验** | |
| `GET /api/tasks/{id}/storyboards` | 项目成员 |
| `GET /api/tasks/{id}/sections/{sectionID}/storyboard` | 项目成员 |
| `PUT /api/tasks/{id}/sections/{sectionID}/storyboard` | 技术撰写人 |
| `POST /api/tasks/{id}/sections/{sectionID}/storyboard/submit` | 技术撰写人 |
| `POST /api/tasks/{id}/sections/{sectionID}/storyboard/approve` | 审核专家 |
| `POST /api/tasks/{id}/sections/{sectionID}/storyboard/reject` | 审核专家 |
| `POST /api/tasks/{id}/sections/{sectionID}/storyboard/render` | 技术撰写人 |
| `GET /api/tasks/{id}/writing-verifications` | 项目成员 |
| `GET /api/tasks/{id}/sections/{sectionID}/writing-verification` | 项目成员 |
| `POST /api/tasks/{id}/sections/{sectionID}/writing-verification/evaluate` | 技术撰写人、审核专家 |
| **D2 证据、可采性、承诺台账与报价** | |
| `GET /api/tasks/{id}/requirements/{reqID}/evidence-bindings` | 项目成员 |
| `POST /api/tasks/{id}/requirements/{reqID}/evidence-bindings` | 技术撰写人、商务专员 |
| `DELETE /api/tasks/{id}/requirements/{reqID}/evidence-bindings/{domain}/{evidenceID}` | 技术撰写人、商务专员 |
| `GET /api/tasks/{id}/admissibility` | 项目成员 |
| `GET /api/tasks/{id}/admissibility/{runID}` | 项目成员 |
| `POST /api/tasks/{id}/admissibility/evaluate` | 技术撰写人、商务专员、审核专家 |
| `POST /api/tasks/{id}/admissibility/{runID}/resolve` | 审核专家 |
| `GET /api/tasks/{id}/commitments` | 项目成员 |
| `PUT /api/tasks/{id}/commitments/{code}` | 商务专员 |
| `POST /api/tasks/{id}/commitments/{code}/confirm` | 投标经理、商务专员 |
| `POST /api/tasks/{id}/commitments/{code}/signoff` | 投标经理、审核专家 |
| `GET /api/tasks/{id}/quotes` | 项目成员 |
| `POST /api/tasks/{id}/quotes` | 商务专员 |
| `POST /api/tasks/{id}/quotes/{quoteID}/validate` | 商务专员 |
| `POST /api/tasks/{id}/quotes/{quoteID}/confirm` | 投标经理、商务专员 |
| `GET /api/tasks/{id}/objective-scores` | 项目成员 |
| `POST /api/tasks/{id}/objective-scores/compute` | 投标经理、技术撰写人、商务专员 |
| **响应矩阵、一致性与导出就绪度** | |
| `POST /api/tasks/{id}/compliance/verify` | 技术撰写人、审核专家 |
| `GET /api/tasks/{id}/compliance/matrix` | 项目成员 |
| `GET /api/tasks/{id}/compliance/coverage` | 项目成员 |
| `POST /api/tasks/{id}/consistency/runs` | 技术撰写人、审核专家 |
| `GET /api/tasks/{id}/consistency/runs` | 项目成员 |
| `GET /api/tasks/{id}/consistency/findings` | 项目成员 |
| `POST /api/tasks/{id}/consistency/findings/{findingID}/resolve` | 审核专家 |
| `GET /api/tasks/{id}/export-readiness` | 项目成员 |
| **图表** | |
| `GET /api/tasks/{id}/charts` | 项目成员 |
| `POST /api/tasks/{id}/charts` | 技术撰写人 |
| `GET /api/tasks/{id}/charts/{chartID}` | 项目成员 |
| `GET /api/tasks/{id}/charts/{chartID}/image` | 项目成员 |
| `POST /api/tasks/{id}/charts/{chartID}/render` | 技术撰写人 |
| `POST /api/tasks/{id}/charts/{chartID}/rebind` | 技术撰写人 |
| `DELETE /api/tasks/{id}/charts/{chartID}` | 技术撰写人 |
| `POST /api/tasks/{id}/chart-jobs` | 技术撰写人 |
| `GET /api/tasks/{id}/chart-jobs` | 项目成员 |
| `GET /api/tasks/{id}/chart-jobs/{jobID}` | 项目成员 |
| `POST /api/tasks/{id}/chart-jobs/{jobID}/cancel` | 技术撰写人 |
| **导出、请求组与提交检查清单** | |
| `GET /api/export/status` | 任何登录账号 |
| `GET /api/export/templates` | 任何登录账号 |
| `GET /api/export/templates/default/file` | 任何登录账号 |
| `POST /api/export/templates` | 管理员，或在任一项目中担任 投标经理 |
| `DELETE /api/export/templates/{templateID}` | 管理员，或在任一项目中担任 投标经理 |
| `GET /api/tasks/{id}/exports/preflight` | 项目成员 |
| `POST /api/tasks/{id}/exports` | 投标经理、技术撰写人（最终稿另要求投标经理（createExport 内判定）） |
| `GET /api/tasks/{id}/exports` | 项目成员 |
| `GET /api/tasks/{id}/exports/{jobID}` | 项目成员 |
| `GET /api/tasks/{id}/exports/{jobID}/download` | 项目成员 |
| `POST /api/tasks/{id}/exports/{jobID}/cancel` | 投标经理、技术撰写人 |
| `GET /api/tasks/{id}/export-requests` | 项目成员 |
| `GET /api/tasks/{id}/export-requests/{requestID}` | 项目成员 |
| `POST /api/tasks/{id}/export-requests/{requestID}/cancel` | 投标经理 |
| `GET /api/tasks/{id}/export-requests/{requestID}/submission` | 项目成员 |
| `PUT /api/tasks/{id}/export-requests/{requestID}/submission/{key}` | 投标经理 |
| `DELETE /api/tasks/{id}/export-requests/{requestID}/submission/{key}` | 投标经理 |
| **模型配置与录制（含全部项目的提示与正文，只给管理员）** | |
| `GET /api/llm/profiles` | 管理员 |
| `POST /api/llm/profiles` | 管理员 |
| `GET /api/llm/profiles/{pid}` | 管理员 |
| `PUT /api/llm/profiles/{pid}` | 管理员 |
| `DELETE /api/llm/profiles/{pid}` | 管理员 |
| `PUT /api/llm/bindings/{agent}` | 管理员 |
| `GET /api/llm/health` | 任何登录账号 |
| `GET /api/llm/bindings` | 管理员 |
| `POST /api/llm/profiles/active` | 管理员 |
| `GET /api/llm/settings` | 管理员 |
| `PUT /api/llm/settings` | 管理员 |
| `GET /api/llm/pool` | 管理员 |
| `POST /api/llm/pool/reset` | 管理员 |
| `POST /api/llm/models` | 管理员 |
| `POST /api/llm/test` | 管理员 |
| `GET /api/llm/records` | 管理员 |
| `GET /api/llm/records/{recordID}` | 管理员 |
| `DELETE /api/llm/records` | 管理员 |
| `GET /api/system/catalog` | 任何登录账号 |
| `GET /api/runtime/version` | 任何登录账号 |
| `GET /api/runtime/tools` | 任何登录账号 |
| **工具调用拦截** | |
| `GET /api/intercepts` | 任何登录账号（处理函数只列出本人所属项目的待审批） |
| `POST /api/intercepts/{pendingID}/decide` | 所属项目的投标经理（对象不属于项目时：管理员） |
| `GET /api/intercept/rules` | 管理员 |
| `POST /api/intercept/rules` | 管理员 |
| `PUT /api/intercept/rules/{ruleID}` | 管理员 |
| `DELETE /api/intercept/rules/{ruleID}` | 管理员 |
| `GET /api/intercept/config` | 管理员 |
| `PUT /api/intercept/config` | 管理员 |
| `GET /api/intercept/approvals` | 任何登录账号（同上，按成员关系过滤） |

“非成员的管理员也可”只用于成员管理；“对象不属于项目时”指不挂在项目下的检索运行与拦截记录。
