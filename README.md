<p align="center">
  <img src="web/public/autobid-logo.svg" width="112" height="112" alt="AutoBid Engine" />
</p>

<h1 align="center">AutoBid Engine</h1>

<p align="center"><strong>面向招投标团队的技术标编制与审查工作台</strong></p>

<p align="center">
  <a href="#快速开始">快速开始</a> · <a href="#核心功能">核心功能</a> · <a href="#开发与测试">开发与测试</a> · <a href="#项目文档">项目文档</a>
</p>

AutoBid Engine 将招标文件解析、要求拆解、企业证据复用、章节编制、人工评审和成稿导出连成一条可追溯的工作流。投标经理、技术撰写人、商务人员和评审人通过浏览器协作，按项目角色处理材料、草稿与审批。

## 快速开始

准备 Windows 11、Go 1.24 或更高版本、Node.js 22 LTS（至少 22.12）与 npm，以及一个运行中的 PostgreSQL 实例。安装和版本记录见[本机运行组件](docs/LOCAL_RUNTIME.md)。

1. 克隆仓库，在 PostgreSQL 中创建一个空数据库（例如 `autobid`）。

   ```bat
   git clone https://github.com/shyrel666/autobid-engine.git
   cd autobid-engine
   ```

2. 在仓库根目录创建 `.env.local.bat`，将示例账号和密码替换为数据库的实际凭据。Git 已忽略此配置文件。

   ```bat
   set "AUTOBID_DATABASE_URL=postgres://USER:PASSWORD@127.0.0.1:5432/autobid?sslmode=disable"
   ```

3. 在命令提示符中启动服务。脚本会安装所需的前端依赖、构建前后端，并在首次启动时迁移数据库。

   ```bat
   start.bat
   ```

4. 服务就绪后，从另一个命令提示符在仓库根目录创建首个管理员。根据提示设置口令，然后在浏览器打开 `http://127.0.0.1:8080` 登录。

   ```bat
   ops.bat user create-admin --login admin --display-name 管理员
   ```

使用 AI 写作前，在工作台的模型设置中配置并测试生成模型。扫描件识别、图表渲染与严格最终稿导出分别依赖 PaddleOCR、Mermaid CLI 与浏览器、WPS 或 Microsoft Word。配置原件留存及这些组件的步骤见[本机运行组件](docs/LOCAL_RUNTIME.md)。

## 核心功能

| 环节 | 工作台能力 |
| --- | --- |
| 招标解读 | 解析 PDF、DOCX 和表格，核对招标原文、澄清更正、评分项与否决项，确认投标分包和要求清单。 |
| 证据与承诺 | 从知识库检索可追溯的材料，记录证据复核结果和经确认的投标承诺。 |
| 写作与评审 | 按大纲编制章节，使用故事板、引用与版本修订开展评审，按登录账号执行撰写与批准的职责分离。 |
| 核验与导出 | 通过响应矩阵和全文一致性核验检查要求与承诺，生成图表，按锁定版本导出 DOCX 或 PDF 并复核成稿。 |

当前基线为单租户、单实例的工程交付。投标得分、业务质量和生产使用仍待外部验证，验收标准见[产品路线图](ROADMAP.md)。

## 开发与测试

后端使用 Go 和 PostgreSQL。`server/` 提供 REST API 与 SSE，`db/` 负责业务状态和迁移，`ingest/`、`retrieval/`、`export/` 分别处理解析、检索和导出。`web/` 使用 React、TypeScript 和 Vite。

在仓库根目录运行以下命令。Windows 上运行竞态检测需要支持 CGO 的 C 编译器（如 GCC）。将 `AUTOBID_TEST_DATABASE_URL` 指向专用测试库以启用 PostgreSQL 集成测试。

```bat
go vet ./...
set "CGO_ENABLED=1"
set "AUTOBID_TEST_DATABASE_URL=postgres://USER:PASSWORD@127.0.0.1:5432/autobid_test?sslmode=disable"
go test -race ./...
npm --prefix web ci
npm --prefix web run test:unit
npm --prefix web run build
```

前端开发时可在后端运行期间执行 `npm --prefix web run dev`。Vite 默认将 API 请求代理到 `http://127.0.0.1:8080`。

## 项目文档

| 文档 | 内容 |
| --- | --- |
| [产品路线图](ROADMAP.md) | 能力现状、实施阶段与业务验收标准 |
| [架构与工程约定](CLAUDE.md) | 运行时架构、模块职责和数据库迁移 |
| [本机运维手册](docs/OPERATIONS.md) | 账号管理、运行检查、备份恢复与升级 |
| [本机运行组件](docs/LOCAL_RUNTIME.md) | WPS、Word、Mermaid、PaddleOCR 和语义模型配置 |
| [生成模型设置](docs/MODEL_SETTINGS_PLAN.md) | 模型协议、凭据、路由和连接测试 |
| [技术标评测](eval/bid/README.md) · [检索评测](eval/retrieval/README.md) | 评测语料、运行方式和结果口径 |
