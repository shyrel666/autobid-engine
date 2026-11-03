# 本机 LibreOffice 与全量竞态复验（2026-11-01）

结论：原安装的 LibreOffice 已可正常执行真实 DOCX/PDF 导出，原先阻塞模型设置 M4、F3a、F3a+ 的两个导出测试均通过，三个条目收口为 E。此前的浏览器与升级恢复验收记录继续有效；本次未重新执行浏览器测试，不改变 F2/F3b/F4 或 D0~D5 的 V/P 状态。

## 环境与处理

- Windows amd64，Go 1.27.0；通过 PowerShell 显式启动 `D:\Git\bin\bash.exe` 执行 Bash 命令。
- 原安装：`F:\LibreOffice\program\soffice.exe`，LibreOffice 26.8.1.1，build `bf6ef2662401c7d6dce671ab4deb26ee0af3a7c1`；自带 Python 3.13.15。
- 首次探测时，原安装的 Python、`import uno`、UNO 组件上下文和 `soffice.com --version` 均已成功。此前记录的 `0xc0000135` 和 `Binary URP bridge disposed during call` 本次未复现，无法据此确定此前故障根因。
- 本次无需重装、替换 DLL、重置用户配置或修改永久环境变量；未改动导出代码或测试条件，也未使用此前的隔离运行时副本。`AUTOBID_SOFFICE_PATH` 沿用原安装，未设置 `AUTOBID_SOFFICE_PYTHON` 覆盖。
- `AUTOBID_TEST_DATABASE_URL` 已设置，集成测试连接真实 PostgreSQL；无数据库或 LibreOffice 依赖导致的跳过。连接凭据不写入报告。

## 本次验证

| 检查 | 结果 |
| --- | --- |
| `go vet ./...` | 通过 |
| `go test -race ./export -run '^(TestExportEndToEndDOCXAndPDF\|TestCustomTemplateLayout)$' -count=1 -v` | 两项通过；默认模板 PDF 25 页、目录位于第 2 页、3 张图片、45 行偏离表及自定义模板检查通过 |
| `go test -race ./... -count=1 -json` | 退出码 0；24 个测试包通过，11 个包无测试文件；297 条测试/子测试通过，4 项跳过，0 失败、0 竞态报告 |
| 全量中的两个真实导出用例 | 均 PASS；分别耗时 24.63 秒、10.64 秒 |
| `npm --prefix web run test:unit` | 15/15 通过，0 跳过 |
| `npm --prefix web run build` | 通过；保留既有 bundle 超过 500 kB 的提示 |

全量命令使用 `-count=1` 强制实际执行，`-json` 仅用于保留逐项结果。四项可选跳过如下，不能算作本次已验证范围：

| 用例 | 原因 |
| --- | --- |
| `TestLiveModelContract` | 未启用会消耗模型额度的 `AUTOBID_LIVE_MODEL_TEST=1` |
| `TestFileProcessingBrowser` | 未配置 `PLAYWRIGHT_MODULE`；此前浏览器验收见 F3a/F3a+ 记录 |
| `TestModelSettingsBrowser` | 未配置 `PLAYWRIGHT_MODULE`；此前浏览器验收见模型设置记录 |
| `TestMMDCRendersGantt` | mermaid-cli 未安装或未配置；真实导出的图表渲染仍使用既有测试替身，不代表 mmdc 验收通过 |

## 本地证据

日志位于 Git 忽略的 `data/libreoffice-repair-2026-11-01/`：

- `runtime-probe.json`：原安装版本、关键二进制 SHA-256、探测结果与代码提交号。
- `export-focused.log`：两个真实导出专项的完整输出。
- `go-test-race.jsonl`、`go-test-race.exit`、`go-test-race.stderr.log`：全量测试原始事件、退出码和 stderr（为空）。
- `test-summary.json`：包结果、通过数、完整跳过原因与导出用例结果。
- `go-vet.log`、`frontend-unit.log`、`frontend-build.log`：其余检查日志。

本次仅更新工程验证与进度文档；旧诊断副本与历史失败记录保留供追溯。
