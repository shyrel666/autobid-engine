# 本机运行依赖登记

登记日期：2026-11-03。对应 [AGENT_TOOL_SKILLS_PLAN.md](../AGENT_TOOL_SKILLS_PLAN.md) S1。

本项目不做目标部署，运行、开发、测试与验收都以本机为准，见计划第 1、6 节。本文记录本机实际安装并验证过的程序、版本、位置、用途、环境变量和自检方法。

使用浏览器的业务人员不需要安装这些程序。程序都装在运行服务的这台机器上，由服务调用。

## 1. 运行能力与自检

- 服务启动时，用固定小样本对每项能力**实际执行一次**。只检测到程序已安装，不算可用。
- 结果从两个入口读取，两者读同一份状态：
  - 接口：`GET /api/runtime/tools`
  - Agent 工具：`tool_runtime_status`
- 前端在三个位置显示状态：
  - 项目顶栏的“运行能力”按钮；
  - 文档上传与材料处理入口；
  - 导出门禁卡片。
- 返回的原因文本已脱敏，不含宿主路径、URL 查询串、令牌或密钥。

| 能力 | 自检样本与判定 | 状态 |
| --- | --- | --- |
| `office` 文档引擎 | 用内置模板生成两章小文档。刷新目录，另存 DOCX 并导出 PDF，再回读：DOCX 目录域要有带页码的条目，PDF 目录页码要等于标题实际所在页 | 已安装（WPS 优先） |
| `mermaid` 图表渲染 | 用项目生成器构造中文分层架构图和甘特图，各渲染一次，PNG 要能解码，且尺寸不小于 100×50 | 已安装 |
| `ocr` 扫描件识别 | 识别内置的中文样本图片 `ingest/ocr_selftest.png`，要识别出“招标”和“2026”，坐标落在图片范围内 | **未安装**（见 3.3） |
| `enhanced_pdf` 增强 PDF 解析 | 尚未交付，固定为 `unconfigured` | 未交付（S5） |
| `embedding` 语义向量 | 嵌入一句固定短文本，向量维度要等于配置值，且没有非有限值 | 按 `.env.local.bat` 配置 |
| `reranker` 语义重排 | 对两段固定文本打分，相关段落得分要高于无关段落 | 按 `.env.local.bat` 配置 |

状态值：

- `unconfigured`：未安装或未配置。
- `starting`：自检进行中。
- `healthy`：自检通过。
- `degraded`：自检通过，但最近一次实际调用失败。下一次调用成功后恢复为 `healthy`。
- `self_test_failed`：已配置，但自检失败。

文档引擎和 Mermaid 的自检在启动时并行执行，完成后才开始执行导出作业。其余三项在服务开始监听后于后台执行，自检期间状态为 `starting`。

自检会消耗少量 embedding / reranker 额度，约几十个 token。

## 2. 已登记的程序与版本

| 组件 | 版本 | 位置 | 用途 |
| --- | --- | --- | --- |
| 操作系统 | Windows 11 专业版，Build 26300 | — | 运行环境 |
| Go | 1.27.0 | `C:\Program Files\Go` | 构建服务端 |
| PostgreSQL | 16.15 | 系统服务 `postgresql-x64-16` | 唯一事实来源 |
| Node.js / npm | 22.22.1 / 10.9.4 | 系统 PATH | 前端构建、运行 Mermaid-CLI |
| WPS Office | 12.1.0.29161（个人版） | COM `KWPS.Application` | 导出文档引擎（优先） |
| Microsoft Word | 2021，16.0.20527（家庭和学生版） | COM `Word.Application` | 导出文档引擎（备选） |
| `@mermaid-js/mermaid-cli` | 11.17.0 | `tools\mermaid-cli\node_modules` | 图表渲染 |
| puppeteer | 25.13.0（不下载自带浏览器） | `tools\mermaid-cli\node_modules` | 被 Mermaid-CLI 调用 |
| Google Chrome | 156.0.8078.80 | `C:\Program Files\Google\Chrome\Application\chrome.exe` | Mermaid-CLI 的渲染浏览器 |
| 中文字体 | 系统自带：微软雅黑 `msyh.ttc`、黑体 `simhei.ttf` | `C:\Windows\Fonts` | 图表中文标签；导出正文使用模板指定的字体 |

授权用途由用户自行确认，不作为工程验收条件。

- WPS 为个人版。
- Word 为家庭和学生版。

## 3. 安装与配置

### 3.1 文档引擎（WPS / Word）

无需额外配置，按 6.1 节的规则选择：

- `AUTOBID_OFFICE_ENGINE`：`auto`（默认，WPS 优先、Word 备选）、`wps` 或 `word`。
- `AUTOBID_OFFICE_TIMEOUT_SECONDS`：单次刷新超时，默认 180 秒。

注意事项：

- COM 自动化需要已登录用户的交互会话，所以服务要在已登录的桌面会话中运行（`start.bat`），不能作为 Windows 服务运行。
- 同一引擎同一时间只处理一个文档。

### 3.2 Mermaid-CLI

固定版本清单在 `tools/mermaid-cli/package.json` 与 `package-lock.json`（纳入 Git）。`node_modules` 不纳入 Git。

安装时不下载 puppeteer 自带的浏览器，改用本机 Chrome：

```powershell
$env:PUPPETEER_SKIP_DOWNLOAD = 'true'
npm ci --prefix tools/mermaid-cli --no-audit --no-fund
```

相关环境变量：

| 变量 | 说明 |
| --- | --- |
| `AUTOBID_MMDC_PATH` | `mmdc` 可执行文件，或 `cli.js`（`.js` 时经 Node 执行，Node 路径可用 `AUTOBID_NODE_PATH` 指定） |
| `AUTOBID_MMDC_BROWSER` | puppeteer 使用的浏览器可执行文件 |
| `AUTOBID_MMDC_TIMEOUT_SECONDS` | 单次渲染超时，默认 90 秒 |
| `AUTOBID_MMDC_CONCURRENCY` | 并发上限，默认 2，最大 8 |
| `AUTOBID_CHART_WORKERS` | 图表渲染作业（`chart_render_jobs`，S3）的执行者数，默认 2；实际渲染并发仍受上一项限制 |
| `AUTOBID_CHART_JOB_DEADLINE_SECONDS` | 图表渲染作业从首次提交起的截止时长，默认 600 秒；回收、暂停恢复与重试都不延长 |

`start.bat` 在未设置时会自动填上两项默认值：

- `AUTOBID_MMDC_PATH`：`tools\mermaid-cli\...\src\cli.js`；
- `AUTOBID_MMDC_BROWSER`：本机 Chrome，没有 Chrome 时用 Edge。

隔离与清理：

- 每次渲染在独立临时目录中执行。子进程的 `TEMP`/`TMP` 指向该目录，puppeteer 的浏览器配置目录也落在里面。
- 超时或取消时，结束 node 及其启动的全部浏览器进程，再删除临时目录。用户自己打开的浏览器不受影响，验证见第 4 节。
- 图表中文字体默认是“微软雅黑 / 黑体”，在 Mermaid 配置中指定。

### 3.3 OCR（未安装）

本机没有 tesseract，也没有配置 `AUTOBID_OCR_URL`，因此 `ocr` 状态为 `unconfigured`，扫描件上传会被拒绝。

PaddleOCR 本机服务在 S5a 安装和验收。配置后的接入方式：

- 服务地址：`AUTOBID_OCR_URL`（服务只监听本机），可选 `AUTOBID_OCR_TOKEN`。
- 也可以安装 tesseract 并带上 `chi_sim` 语言包，通过 PATH 或 `AUTOBID_TESSERACT` 找到。

两种方式在启动时都会用内置样本自检。

### 3.4 Embedding / Reranker

配置与限制见 [eval/retrieval/README.md](../eval/retrieval/README.md)。

- 当前是阿里云百炼的临时授权，2026-11-29 到期。
- 密钥只放在被 Git 忽略的 `.env.local.bat` 中。
- 未配置时 `embedding` 状态为 `unconfigured`，知识库使用 256 维哈希向量，不是语义检索。

## 4. 本机验证记录（2026-11-03，E 级）

测试命令（PowerShell，需 `CGO_ENABLED=1`、`CC=gcc` 与 `AUTOBID_TEST_DATABASE_URL`）：

```powershell
$env:AUTOBID_MMDC_PATH = "$PWD\tools\mermaid-cli\node_modules\@mermaid-js\mermaid-cli\src\cli.js"
$env:AUTOBID_MMDC_BROWSER = "C:\Program Files\Google\Chrome\Application\chrome.exe"
go test -race -count=1 ./...
# 文档引擎大文档与连续运行（默认跳过，耗时数分钟）；改用 Word 时另设 AUTOBID_OFFICE_ENGINE=word
$env:AUTOBID_OFFICE_SOAK = "30"
go test -count=1 -v -run TestOfficeLargeDocumentAndSoak ./export/
```

真实组件用例（未安装对应组件时跳过）：

| 用例 | WPS 12.1.0.29161 | Word 16.0.20527 |
| --- | --- | --- |
| `TestMMDCRendersGantt` | 真实 mmdc 11.17.0 渲染中文甘特图，自检通过 | — |
| `TestMMDCCancelLeavesNoProcesses` | 本次 Chrome 出现后取消渲染：结束 8 个浏览器进程，删除临时目录；调用前已有的 2 个用户浏览器主进程仍在 | — |
| `TestExportWithRealMermaidCharts` | 真实 mmdc 渲染的架构图（1228×904）、甘特图（3168×440）、组织图（880×396）插入成稿：17 页，3 张图，版式自检 0 问题，目录页码一致 | — |
| `TestOfficeCancelLeavesNoProcesses` | 取消刷新：结束本次 2 个自动化进程，调用前已有的 1 个 WPS 常驻进程仍在 | 结束本次 1 个自动化进程 |
| `TestOfficeQueuesConcurrentExportJobs` | 2 个执行者争用 6 个作业：全部首次成功，用时 23 秒，同时最多 2 个自动化进程（一次会话），PDF 页数一致（22 页） | 全部首次成功，用时 43 秒，同时最多 1 个自动化进程，PDF 同为 22 页 |
| `TestOfficeLargeDocumentAndSoak` | 140 页、61 条目录的大文档刷新 7.8 秒，PDF 0 问题、DOCX 目录页码与 PDF 一致；连续 30 次刷新，前 5 次平均 4.5 秒、后 5 次 3.8 秒，无残留进程 | 同一大文档同为 140 页，刷新 30.1 秒，PDF 0 问题；连续 15 次刷新，前 5 次平均 10.9 秒、后 5 次 8.8 秒，无残留进程 |

全量 `go vet ./...` 与 `go test -race -count=1 ./...`（真实 PostgreSQL，mmdc 与 WPS 可用）全部通过。

交付 DOCX 换软件打开后目录页码零误差的对照，见计划 12.5 节。

**尚未验证**：

- OCR（未安装，S5a）；
- 真实企业投标模板（需要用户提供）；
- 引擎内同时处理多个文档（并发固定为 1）、数小时级运行与生产负载。

## 5. 升级规则

- 升级任何已登记组件后，都要重启服务重新自检，并更新本文版本表。
- 不使用浮动的 `latest` 版本。
- 业务请求中不执行 `npm` / `pip` 安装，也不做首次联网下载。
