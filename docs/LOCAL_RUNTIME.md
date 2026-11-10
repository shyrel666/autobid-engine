# 本机运行依赖登记

登记日期：2026-11-03。对应 [AGENT_TOOL_SKILLS_PLAN.md](../AGENT_TOOL_SKILLS_PLAN.md) S1；2026-11-04 随 S5a 登记本机 OCR（PaddleOCR）；2026-11-06 随 S6 登记可选的本机语义模型；2026-11-10 随 S7 登记离线安装步骤（3.5）、运维与浏览器验收用到的工具（3.6）。日常运维步骤见 [OPERATIONS.md](OPERATIONS.md)。

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
| `ocr` 扫描件识别 | 识别内置的中文样本图片 `ingest/ocr_selftest.png`，要识别出“招标”和“2026”，坐标落在图片范围内；服务还须回报执行版本（解析作业据此锁定） | 已安装（PaddleOCR，见 3.3） |
| `enhanced_pdf` 增强 PDF 解析 | 尚未交付，固定为 `unconfigured` | 未交付（S5） |
| `embedding` 语义向量 | 配了 `INFO_URL` 时先核对服务端模型修订；再嵌入一句固定短文本，向量维度要等于配置值，且没有非有限值 | 按 `.env.local.bat` 配置；可选本机模型见 3.4 |
| `reranker` 语义重排 | 配了 `INFO_URL` 时先核对服务端模型修订；再对两段固定文本打分，相关段落得分要高于无关段落 | 按 `.env.local.bat` 配置；可选本机模型见 3.4 |

状态值：

- `unconfigured`：未安装或未配置。
- `starting`：自检进行中。
- `healthy`：自检通过。
- `degraded`：自检通过，但最近一次实际调用失败。下一次调用成功后恢复为 `healthy`。
- `self_test_failed`：已配置，但自检失败。

文档引擎和 Mermaid 的自检在启动时并行执行，完成后才开始执行导出作业。其余三项在服务开始监听后于后台执行，自检期间状态为 `starting`。本机 OCR 或语义模型服务还在加载模型时（连接被拒或 503），自检在 2 分钟内每 2 秒重试一次，不立即判为失败。

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
| Python（OCR 服务） | 3.10.8 | `D:\python-3.10.8`（只用来创建虚拟环境） | 本机 OCR 服务的运行时 |
| PaddlePaddle | 3.3.1（CPU） | `tools\paddleocr\.venv` | OCR 推理框架 |
| PaddleOCR / PaddleX | 3.7.0 / 3.7.2 | `tools\paddleocr\.venv` | 扫描件与图片识别（`tools/paddleocr/service.py`） |
| 语义模型运行时（可选） | onnxruntime 1.20.1、tokenizers 0.21.0、numpy 1.26.4；Python 3.10.8 | `tools\local-models\.venv` | 本机 embedding / rerank 的 CPU 推理，不装 torch |
| 语义模型（可选） | `Xenova/bge-small-zh-v1.5`（`model.onnx`，512 维）、`Xenova/bge-reranker-base`（`model_int8.onnx`） | `tools\local-models\models` | 本机语义检索，默认不启用 |
| OCR 模型 | `PP-OCRv5_mobile_det`、`PP-OCRv5_mobile_rec`（默认）；`PP-OCRv5_server_rec`（可选） | `tools\paddleocr\models` | 文字检测与识别；模型文件摘要写进执行版本 |
| PostgreSQL 客户端工具 | `pg_dump` / `pg_restore` 16.15 | `C:\Program Files\PostgreSQL\16\bin` | `ops.bat backup` / `restore`（S7） |
| playwright-core（仅验收） | 1.57.0，随 playwright-go 安装 | `C:\Users\LENOVO\AppData\Local\ms-playwright-go\1.57.0\package` | 浏览器验收脚本 `web/e2e/*.mjs`，不是运行依赖 |
| Chrome for Testing 无头版（仅验收） | 148.0.7778.96（Playwright 浏览器修订 1223） | `C:\Users\LENOVO\AppData\Local\ms-playwright\chromium_headless_shell-1223` | 同上 |

授权用途由用户自行确认，不作为工程验收条件。

- WPS 为个人版。
- Word 为家庭和学生版。

## 3. 安装与配置

### 3.1 文档引擎（WPS / Word）

无需额外配置，按 6.1 节的规则选择：

- `AUTOBID_OFFICE_ENGINE`：`auto`（默认，WPS 优先、Word 备选）、`wps`、`word` 或 `off`。`off` 不探测或启动任何文档引擎，运行能力为 `unconfigured`；拒绝 PDF 与严格最终稿，只允许 DOCX 预览稿。已有产物仍可下载。重新启用 WPS / Word 后须重启服务并通过启动自检。
- `AUTOBID_OFFICE_TIMEOUT_SECONDS`：单次刷新超时，默认 180 秒。
- `AUTOBID_EXPORT_WORKERS`：导出作业执行者数，默认 2；引擎内仍串行。
- `AUTOBID_EXPORT_JOB_DEADLINE_SECONDS`：导出作业从首次提交起的截止时长，默认 1800 秒（S4）；回收、暂停恢复与重试都不延长。
- `AUTOBID_AGENT_EXPORT_ENABLED`：为 `1/true` 时允许 Worker 经 `proposal_export_submit` 提交严格最终稿（S4），默认关闭。这是高影响入口，R2 可信身份完成前保持关闭；关闭时页面导出不受影响。

最终稿（严格策略）在提交时固定当时的引擎版本标签（如 `WPS 12.1.0.29161`）。之后升级或切换引擎，按原版本提交的作业会以 `RUNTIME_UNAVAILABLE` 失败，需要用新的请求键重新提交；预览稿不受影响。

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

### 3.3 OCR（PaddleOCR 本机服务，S5a）

`tools/paddleocr/service.py` 是只监听 `127.0.0.1` 的 HTTP 服务，实现 `ingest.HTTPOCR` 协议：

- `GET /ocr`：返回引擎与执行版本，如 `paddleocr 3.7.0; paddle 3.3.1; PP-OCRv5_mobile_det@3206ba2888d2; PP-OCRv5_mobile_rec@b9c2be6f68f1; cpu`（`@` 后是模型文件摘要）。
- `POST /ocr`：请求体是图片原始字节，带请求 ID 与内容 SHA-256；响应给出每行文字、像素坐标框、置信度、执行版本与收到的图片哈希。哈希不一致时客户端拒绝结果。
- 错误为结构化 JSON：400、401、413、415、500、503。连接失败与 502/503/504 归为暂时故障，解析作业在原截止时间与尝试预算内重试；其他错误为确定性失败。
- 服务只读取 `tools\paddleocr\models` 中预先下载的模型，运行时不联网下载；同一时间只识别一张图片，其余请求排队。

固定版本清单：

- `tools/paddleocr/requirements.txt`：顶层固定 `paddlepaddle==3.3.1`、`paddleocr==3.7.0`；
- `requirements.lock.txt`：完整的 `pip freeze`；
- 两者纳入 Git。虚拟环境 `.venv`、模型目录 `models` 与下载缓存不纳入 Git。

安装（一次性，需联网）：

```powershell
python -m venv tools\paddleocr\.venv
tools\paddleocr\.venv\Scripts\python.exe -m pip install -r tools\paddleocr\requirements.lock.txt -i https://pypi.tuna.tsinghua.edu.cn/simple
tools\paddleocr\.venv\Scripts\python.exe -X utf8 tools\paddleocr\fetch_models.py
# 可选：服务器版识别模型（更准、更慢，见下方实测）
$env:PADDLEOCR_REC_MODEL = 'PP-OCRv5_server_rec'; tools\paddleocr\.venv\Scripts\python.exe -X utf8 tools\paddleocr\fetch_models.py
```

启动与配置：

- `start.bat` 检测到 `.venv` 和默认模型后，在名为 `autobid-ocr` 的最小化窗口中启动服务（端口 `AUTOBID_OCR_PORT`，默认 18765），并设置 `AUTOBID_OCR_URL=http://127.0.0.1:18765/ocr`。
- 已设置 `AUTOBID_OCR_URL` 或 `AUTOBID_OCR_AUTOSTART=0` 时，不自动启动。
- 手动启动：`tools\paddleocr\.venv\Scripts\python.exe -X utf8 tools\paddleocr\service.py --port 18765`。
- 可选参数：`--rec PP-OCRv5_server_rec`（或 `PADDLEOCR_REC_MODEL`），`AUTOBID_OCR_TOKEN`（服务与服务端同时设置后校验 Bearer 令牌）。
- 只配置了 CPU。本机有 RTX 3050 Laptop GPU，但没有安装 GPU 版 PaddlePaddle，也没有测过 GPU 配置。

已知问题：PaddlePaddle 3.3.1 在 Windows 上使用 oneDNN 时报 `ConvertPirAttribute2RuntimeAttribute not support`，服务固定 `enable_mkldnn=False`，用普通 CPU 内核推理。

执行版本锁定（S5a）：

- 解析作业首次提交时，把服务回报的执行版本写入作业的 `executor`。
- 之后升级 PaddleOCR、更换模型或改用服务器版识别模型，排队中按旧版本提交的作业会以 `RUNTIME_UNAVAILABLE` 失败，需要用新的请求键重新提交。
- 服务暂时停止时，作业回到队列，在原截止时间（固定解析作业默认 15 分钟）与 3 次尝试内重试。

模型选择实测（2026-11-04，E 级，本机 CPU）。样本是一页合成的扫描招标页：900×1270 灰度 JPEG，含旋转与噪点，`ingest/testdata/scan_tender_page.jpg`，文字为虚构内容。

| 识别模型 | 每页耗时 | 文字行 | 结果 |
| --- | --- | --- | --- |
| `PP-OCRv5_mobile_rec`（默认） | 4.9 秒 | 18 行全部检出，坐标正确 | 4 行条款共漏 5 个字，置信度仍在 0.97 以上：“工期”→“期”、“日历天”→“历天”、“投标人”→“投标”、“可用性”→“可性”、“2小时”→“2时” |
| `PP-OCRv5_server_rec` | 17.4 秒 | 18 行全部检出 | 条款文字逐字正确 |

- 默认用 mobile 模型（2026-11-04 用户确认）：固定解析作业默认截止 15 分钟，按服务器版速度只能处理约 50 页扫描件。
- 置信度高也会漏字，所以 OCR 结果只作为候选。须人工对照原件采用，采用后也不等于证据已核验。
- 只有 1 页合成样本，不是质量结论。

tesseract 仍可作为备选：安装后带上 `chi_sim` 语言包，经 PATH 或 `AUTOBID_TESSERACT` 找到。版本取自 `tesseract --version`。

### 3.4 Embedding / Reranker

配置与限制见 [eval/retrieval/README.md](../eval/retrieval/README.md)。

- 当前是阿里云百炼的临时授权，2026-11-29 到期。
- 密钥只放在被 Git 忽略的 `.env.local.bat` 中。
- 授权到期后如果不配置其它模型，检索如实退回全文与关键词，页面和 Agent 都能看到原因。

**可选的本机语义模型（S6）**

`tools/local-models/service.py` 是只监听 `127.0.0.1` 的 HTTP 服务，协议沿用 TEI，服务端按 `format=tei` 调用，没有单独的适配代码。**默认不启用**：评测没有显示稳定收益（见 [AGENT_TOOL_SKILLS_PLAN.md](../AGENT_TOOL_SKILLS_PLAN.md) 12.10）。

- `POST /embedding/embed`、`POST /reranker/rerank`：与 TEI 一致；超长且 `truncate=false` 时返回 413。
- `GET /embedding/info`、`GET /reranker/info`：返回 `model_id`、`model_sha`（ONNX 文件 SHA-256 前 16 位，形如 `sha256:69a0b846f4f116b5`）、服务版本与引擎。服务端自检用 `model_sha` 核对配置的修订，不一致时自检失败。
- `GET /health`：`loading` / `ready` / `failed`。加载完成前业务接口返回 503。
- 启动时校验模型文件 SHA-256，只读 `models` 目录，运行时不联网；不打印请求文本。

固定版本清单（纳入 Git）：`models.lock.json`（模型仓库提交与 SHA-256）、`requirements.txt`、`requirements.lock.txt`。`.venv`、`models`、下载缓存不纳入 Git。

安装（一次性，需联网，共约 600 MB）：

```powershell
D:\python-3.10.8\python.exe -m venv tools\local-models\.venv
tools\local-models\.venv\Scripts\python.exe -m pip install -r tools\local-models\requirements.lock.txt
tools\local-models\.venv\Scripts\python.exe -X utf8 tools\local-models\fetch_models.py
```

`fetch_models.py` 按 `models.lock.json` 下载并校验（约 375 MB），校验不符就删除并报错；下载完成后写 `models\env.bat`（模型修订号）。Hugging Face 不通时加 `--mirror https://hf-mirror.com`。

启用：

1. 删除或注释 `.env.local.bat` 里的 `AUTOBID_EMBEDDING_*` 与 `AUTOBID_RERANKER_*`（已配置时 `start.bat` 保持现有模型）。
2. 在 `.env.local.bat` 加 `set "AUTOBID_LOCAL_MODELS=1"`。
3. 运行 `start.bat`。它在名为 `autobid-models` 的最小化窗口中启动服务（端口 `AUTOBID_LOCAL_MODELS_PORT`，默认 18766），并设置全部模型变量，重排候选上限默认 30 条。
4. 在知识库“语义索引”页构建并启用。模型变了，旧索引不会参与检索。

切回原模型：恢复 `.env.local.bat`，重启后在“语义索引”页启用旧索引，不需要重算。
- 未配置时 `embedding` 状态为 `unconfigured`，知识库使用 256 维哈希向量，不是语义检索。

### 3.5 离线安装与模型导入（S7）

运行时不联网：OCR 与语义模型服务只读本地 `models` 目录，Mermaid-CLI 不下载浏览器，业务请求中不执行 npm / pip 安装。安装阶段的下载可以在另一台联网机器上完成，再复制过来：

1. **Mermaid-CLI**：在联网机器上按 3.2 执行 `npm ci`（设置 `PUPPETEER_SKIP_DOWNLOAD=true`），把 `tools\mermaid-cli\node_modules` 整个目录复制到本机同一位置。本机另需 Chrome 或 Edge。
2. **Python 依赖**：在系统、架构与 Python 版本（3.10，Windows x64）都和本机一致的联网机器上执行 `python -m pip download -r tools\paddleocr\requirements.lock.txt -d wheelhouse\paddleocr`（镜像参数同 3.3）。把 `wheelhouse` 复制过来，在本机新建虚拟环境，再执行 `pip install --no-index --find-links wheelhouse\paddleocr -r tools\paddleocr\requirements.lock.txt`。语义模型同理。不要直接复制 `.venv`：它记录了创建它的 Python 路径。
3. **模型**：在联网机器上运行 `fetch_models.py`，把 `tools\paddleocr\models`、`tools\local-models\models` 复制到本机同一位置。语义模型服务启动时校验模型文件 SHA-256（3.4）；OCR 服务把模型文件摘要写进执行版本，换了模型时，按旧版本提交的解析作业会失败，不会悄悄改用新模型（3.3）。
4. **字体**：使用 Windows 自带的微软雅黑与黑体，无需导入。

组件与模型的授权声明见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。这套离线步骤没有在第二台机器上实测。

### 3.6 运维工具与浏览器验收（S7）

运维命令都经 `ops.bat` 读取 `.env.local.bat` 后运行 `bin\autobid-ops.exe`（`start.bat` 一并构建），停机、备份、恢复、升级、回退的步骤见 [OPERATIONS.md](OPERATIONS.md)。`pg_dump` 依次从 `AUTOBID_PG_BIN`、PATH、`C:\Program Files\PostgreSQL\16\bin` 查找。

浏览器验收不属于运行依赖，业务人员不需要。维护人员复跑时设置：

| 变量 | 说明 |
| --- | --- |
| `AUTOBID_TEST_DATABASE_URL` | 测试用 PostgreSQL；每个套件新建并删除自己的隔离库，库名为 `autobid` 时拒绝执行 |
| `PLAYWRIGHT_MODULE` | playwright 或 playwright-core 模块目录；未设置时浏览器套件跳过 |
| `BROWSER_EXECUTABLE` | Chromium 可执行文件；本机用上表的无头版（Playwright 自带的修订 1200 未安装） |
| `AUTOBID_MMDC_PATH` / `AUTOBID_MMDC_BROWSER` / `AUTOBID_OCR_URL` | 真实 Mermaid 与 OCR，传给被测服务 |
| `E2E_REAL_TENDER` | 真实招标验收使用的开发集文件（`ccgp_2026-10-06/` 中的一份，不入库） |
| `AUTOBID_S7_DRILL=1` | 执行升级恢复演练 `cmd/upgrade-drill` |

命令与结果见 [S7_ACCEPTANCE.md](S7_ACCEPTANCE.md)。这些套件会启动真实 WPS，持 `export/officetest` 文件锁串行运行；本机服务运行时不要跑。

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

真实文档引擎用例经 `export/officetest` 跨包排队，默认并行运行即可，不需要 `-p 1`；本机服务运行或自检时不要跑这些用例，它不走这把锁。

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

- OCR 的业务质量：只有合成样本，没有真实扫描招标件与人工标注（S5a 的本机记录见 4.1）；
- 真实企业投标模板（需要用户提供）；
- 引擎内同时处理多个文档（并发固定为 1）、数小时级运行与生产负载。

### 4.1 OCR 本机验证记录（2026-11-04，E 级，S5a）

真实 PaddleOCR 用例在设置 `AUTOBID_OCR_URL` 时执行：

| 用例 | 结果 |
| --- | --- |
| `TestRealPaddleOCRScannedPDF`（`ingest`） | 自检样本通过。两页 PDF（第 1 页文本层、第 2 页为上表的合成扫描页）按 `auto` 路由解析：只有第 2 页走 OCR，用时约 5 秒；识别出“服务器数量不少于12台”“ISO9001”“99.9%”“机架式服务器”“核心交换机”；逐行识别框共 18 条，都落在 900×1270 图像范围内并带置信度 |
| `TestRealOCRCandidateImportEndToEnd`（`filework`） | 同一 PDF 经固定解析作业生成候选。执行版本锁定为 PaddleOCR + 两个模型的摘要；候选逐段给出页码、OCR 行数与最低置信度；人工采用后正式入库为 2 页文档，来源 epoch 推进 |

### 4.2 语义检索本机验证记录（2026-11-06，E 级，S6）

服务 `autobid-local-models/1.0.0`，模型修订 `sha256:69a0b846f4f116b5`（embedding）、`sha256:2059d8ef0b6e935b`（reranker）。本机 CPU i7-12700H，6 线程，下表为中位数：

| 文本长度 | embedding（32 条） | rerank 30 条 | rerank 100 条 |
| --- | --- | --- | --- |
| 约 70 字（200 字节） | 160 ms | 338 ms | 1.0 s |
| 约 200 字（600 字节） | 466 ms | 899 ms | 2.8 s |
| 约 470 字（1400 字节，真实分块 p90） | 1.2 s | 2.4 s | 8.4 s |

- 服务峰值工作集约 2.2 GB。
- 3 份真实招标 DOCX（973 个分块）全量构建 28.1 秒；新增 1 份文档后重建 0.8 秒（新算 1 块，复用 973 块）。
- 带重排的混合检索端到端 2.4~2.9 秒。
- 故障演练（杀服务进程、杀模型服务、模型挂起、模型切换与回滚）与质量对比见 [AGENT_TOOL_SKILLS_PLAN.md](../AGENT_TOOL_SKILLS_PLAN.md) 12.10。这些只是本机小样本的工程数据，不是质量或负载结论。

## 5. 升级规则

- 升级前先 `ops.bat check` 确认没有在途作业，停机后备份并校验，步骤见 [OPERATIONS.md](OPERATIONS.md) 第 5 节。
- 升级任何已登记组件后，都要重启服务重新自检，并更新本文版本表。
- 不使用浮动的 `latest` 版本。
- 业务请求中不执行 `npm` / `pip` 安装，也不做首次联网下载。
- 更换语义模型或修订后要重建索引再启用；旧索引保留，可在切回原模型后直接启用。
