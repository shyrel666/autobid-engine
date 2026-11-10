# 第三方组件与授权登记

登记日期：2026-11-10（[AGENT_TOOL_SKILLS_PLAN.md](../AGENT_TOOL_SKILLS_PLAN.md) S7，对应第 6 节“记录授权与第三方 notices”）。

依据是本机已安装组件自带的包元数据、LICENSE 文件与模型卡，只记录它们声明的授权，不是法律意见。分发、商用或在其他机器部署前，由使用者自行核对。版本与安装位置见 [LOCAL_RUNTIME.md](LOCAL_RUNTIME.md)。

## 1. 本机运行组件

这些组件不随仓库分发，按 LOCAL_RUNTIME.md 第 3 节在本机安装；固定版本清单在 `tools/` 下纳入 Git。

| 组件 | 版本 | 声明的授权 | 声明所在位置 |
| --- | --- | --- | --- |
| `@mermaid-js/mermaid-cli` | 11.17.0 | MIT | `tools/mermaid-cli/node_modules/@mermaid-js/mermaid-cli/LICENSE` |
| `mermaid` | 11.17.2 | MIT | `tools/mermaid-cli/node_modules/mermaid/package.json` |
| `puppeteer` / `puppeteer-core` | 25.13.0 | Apache-2.0 | 同目录下各自的 `package.json` |
| PaddleOCR | 3.7.0 | Apache-2.0 | `tools/paddleocr/.venv` 中的包元数据 |
| PaddlePaddle（CPU） | 3.3.1 | Apache-2.0 | 同上 |
| PaddleX | 3.7.2 | Apache-2.0 | 同上 |
| OpenCV（`opencv-contrib-python`） | 4.10.0.84 | Apache-2.0 | 同上 |
| NumPy | 2.2.6（OCR）/ 1.26.4（语义模型） | BSD-3-Clause | 两个虚拟环境中的包元数据 |
| onnxruntime | 1.20.1 | MIT | `tools/local-models/.venv` 中的包元数据 |
| tokenizers | 0.21.0 | Apache-2.0 | 同上 |
| PostgreSQL | 16.15 | PostgreSQL License | `C:\Program Files\PostgreSQL\16\server_license.txt` |

间接依赖的完整列表在 `tools/paddleocr/requirements.lock.txt`、`tools/local-models/requirements.lock.txt` 与 `tools/mermaid-cli/package-lock.json`，各自的授权文件在对应虚拟环境的 `*.dist-info` 或 `node_modules` 目录中。

## 2. 模型文件

| 模型 | 声明的授权 | 依据 |
| --- | --- | --- |
| `PP-OCRv5_mobile_det`、`PP-OCRv5_mobile_rec`（默认）、`PP-OCRv5_server_rec`（可选） | Apache-2.0 | `tools/paddleocr/models/<模型>/README.md` 模型卡 |
| `Xenova/bge-small-zh-v1.5`、`Xenova/bge-reranker-base`（可选，默认不启用） | 未核对 | 本机只下载了 ONNX 与分词器文件，没有保存模型卡；启用前须在模型仓库核对 Xenova 转换版本与上游 BAAI 模型的授权声明 |

## 3. 商业软件与系统组件

由使用者自行安装并确认授权用途，不作为工程验收条件：

- WPS Office 12.1（个人版）、Microsoft Word 2021（家庭和学生版）：导出文档引擎。
- Google Chrome：Mermaid-CLI 的渲染浏览器。
- 微软雅黑、黑体：Windows 自带字体，用于图表与导出。PDF 由文档引擎按字体自带的嵌入许可嵌入，本项目未单独审查字体授权。

## 4. 编译进程序的依赖

直接依赖如下，间接依赖见 `go.sum` 与 `web/package-lock.json`，授权文件分别在 Go 模块缓存与 `web/node_modules` 中。

| 依赖 | 版本 | 声明的授权 |
| --- | --- | --- |
| `github.com/jackc/pgx/v5` | 5.7.1 | MIT |
| `github.com/anthropics/anthropic-sdk-go` | 1.84.0 | MIT |
| `golang.org/x/text` | 0.27.0 | BSD-3-Clause |
| `github.com/xuri/excelize/v2` | 2.9.1 | BSD-3-Clause |
| `react` / `react-dom` | 19.3.1 | MIT |
| `lucide-react` | 1.63.0 | ISC |
| 构建工具 `vite` 7.3.7、`tailwindcss` / `@tailwindcss/vite` 4.3.3、`@vitejs/plugin-react` 5.2.0 | — | MIT |
| 构建工具 `typescript` | 5.9.3 | Apache-2.0 |

## 5. 只用于验收的工具

不属于运行依赖，业务用户不需要：

- `playwright-core` 1.57.0（Apache-2.0）与 Chrome for Testing 无头版 148.0.7778.96：浏览器验收。
- `web/e2e/stub_llm.py`：本仓库自带的脚本替身模型，不是第三方组件。
