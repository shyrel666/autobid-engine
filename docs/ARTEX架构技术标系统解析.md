# **ARTEX架构技术标系统解析**

[https\://gemini.google.com/app/bc2bc3b517d45c8a](https://gemini.google.com/app/bc2bc3b517d45c8a)

*User prompt: 我有一个基于ARTEX架构的编写技术标系统，解释一下这个架构*

Response: **ARTEX 架构**最初源于开源的 **LLM 多智能体（Multi-Agent）自主任务编排框架**（原版多用于自动化安全测试，采用 Go 单体后端 \+ Next.js 前端 \+ PostgreSQL \+ norma Agent SDK 构建）。其核心设计解决了一个在大模型工程中最棘手的问题：**超长链路复杂任务（Long-Horizon Tasks）的状态维持、防遗忘与无损协同**。

将 ARTEX 架构迁移并改造为**编写技术标（Technical Proposal）系统**，是非常契合的工程实践。因为一份几十万字的技术标书同样面临上下文窗口爆炸、招标需求漏项、前后章节逻辑冲突等痛点。

在技术标编写系统中，ARTEX 架构的核心机制拆解为以下四大支柱：

### **1\. 核心状态层：双图谱记忆架构（Dual-Graph Memory）**

普通大模型应用依赖线性的对话上下文（Context Window），写到第 5 章时往往忘了第 1 章的架构定义。ARTEX 通过将“客观事实”与“思考推进过程”解耦为两个独立的图谱，并利用锚点（Anchors）进行双向绑定，彻底解决了长文本失忆问题：

| 核心组件 | 原生 ARTEX 概念 | 技术标系统中的映射与作用 |
| :---- | :---- | :---- |
| **资产图谱** *(Asset Graph)* | 全局共享的客观基础设施拓扑（域名、IP、接口等）。 | **客观知识与标书底座（全局共享）：** 存储招标文件硬性要求（评分点、星号实质性条款）、企业资质库、历史业绩、产品技术参数及标准架构图。节点由程序严格去重并维护父子层级，作为不可篡改的“单一事实来源”。 |
| **探索图谱** *(Exploration Graph)* | 单任务内的 AI 推理与推进链路（目标、意图、事实、成果）。 | **标书编写与演进链路（单项目独立）：** 记录当前标书从大纲到成文的推导过程。包含 Goal（章节目标）→ Intent（写作/检索意图）→ Fact（提取的支撑论据/架构设计）→ Finding（最终生成的标书段落与方案）。 |
| **探索锚点** *(Exploration Anchors)* | 连接“测试动作”与“具体资产”的映射表。 | **响应度与覆盖率矩阵（偏离度控制）：** 将探索图谱中的“生成段落”锚定到资产图谱中的“招标评分点”。系统借此实时计算**招标需求覆盖率（Coverage）**，确保每一个评分项都有对应的技术方案支撑，杜绝漏写与废标风险。 |

### **2\. 智能体协同层：职责解耦的多 Agent 角色（Role-Separated Agents）**

ARTEX 严格分离了“战略规划”与“战术执行”，避免单个 Agent 既要思考全局结构又要撰写细节导致的注意力分散：

> * **Goals（目标分解器）：** 解析招标文件（RFP），拆解出评分标准、技术偏离表要求及标书整体目录大纲，初始化图谱根节点。  
> * **Planner（规划器 / 总编）：** 系统中**唯一能产生“意图（Intent）”的决策大脑**。它不直接写长文，而是读取当前的双图谱状态和覆盖率，决定下一步该写哪个小节、需要补充什么技术参数，并将任务拆解为具体的 Intent 放入队列。  
> * **Workers（执行器 / 撰稿专员）：** 多个并发运行的无状态执行 Agent。每个 Worker 从队列中认领一个具体的 Intent（例如：“编写 3.2.1 节高并发微服务架构设计，需结合资产库中的政企云案例”），调用本地检索工具、绘图工具或知识库完成撰写，并将产出写回图谱。  
> * **MainAgent（人机协同网关 / 审校总工）：** 提供 Human-in-the-loop（人在回路）接口。售前工程师可以随时介入查看进度、注入人工提示（Hint 节点，如“把数据库全部替换为国产达梦数据库”），或通过审批门禁（Approval Gates）对核心报价与实质性响应承诺进行人工确认，无需打断底层引擎的调度循环。

### **3\. 调度与稳定层：事件驱动闭环与抗失忆机制**

为了让多 Agent 在编写几百页标书时做到“前后口径一致、不重不漏”，ARTEX 引入了三项关键的自治机制：

> 1. **事件驱动唤醒闭环（Event-Driven Loop）：** 系统并非按死板的固定脚本顺序执行，而是通过图谱状态变更触发（Graph-Change Debounce）。每当 Worker 写完一个章节并更新图谱状态，就会触发唤醒 Planner；Planner 读取最新全局状态后，再动态指派下一批写作意图，直到所有招标评分点被 100% 满足（Proven）。  
> 2. **跨轮次共享待办列表（Shared Todolist）：** 针对强依赖的串行章节（例如：必须先确定“总体技术架构” → 才能定“软件功能模块” → 才能算“软硬件部署清单与信创配置”），Planner 会在共享 Todolist 中锁定依赖顺序。即便每次 LLM 会话是无状态的（Stateless），也只有当前置章节校验通过后，才会派发后续章节的写作任务，保证全书逻辑链条严丝合缝。  
> 3. **跨 Worker 轨迹检索（Cross-Worker Trace Search）：** 并发编写不同章节的 Worker 之间具备进程级的信息交换能力。一个负责写“数据安全方案”的 Worker 可以实时检索另一个负责写“总体架构”的 Worker 的执行日志与产出物，从而统一技术栈命名、接口规范和性能指标，避免出现“第 2 章写用 MySQL，第 6 章写用 PostgreSQL”的低级错误。

### **4\. 基础工程底座（Infrastructure Stack）**

如果您的系统保留了 ARTEX 的原生工程栈，其底层通常具备极高的轻量化与高并发特性：

> * **Go 单体后端 \+ PostgreSQL：** 利用 Go 语言天然的高并发协程调度多个 Worker Agent，并通过 PostgreSQL 持久化存储双图谱结构与锚点关系，支持中断恢复与幂等升级。  
> * **多模型抽象层（Multi-LLM Abstraction）：** 支持灵活路由不同的大模型（例如：Planner 使用推理能力极强的模型做大纲拆解与逻辑审校，Worker 使用长文本生成快、性价比高的模型或本地私有化部署的 Ollama 模型批量扩写章节）。

**注：** 如果您团队内部的“ARTEX”并非指代上述开源多智能体双图谱架构，而是团队自研的**特定缩写架构**（例如：**A**gent **R**etrieval **T**emplate **EX**ecution 等技术标专用流水线缩写），请告诉我它的具体模块名称或技术栈，我可以为您做针对性的深度拆解。

**Sources**

> 1. [ARTEX \- AI autonomous penetration testing system ... \- GitHub](https://github.com/BaaRai/ARTEX)  
> 2. [ARTEX \- Multi-Agent LLM Penetration Testing System \| AI/TLDR](https://ai-tldr.dev/tools/artex/)  
> 3. [ARTEX Unveiled: Key Pillars of Offensive AI \| Guardz.com](https://guardz.com/blog/artex-agentic-ai-framework/)  
> 4. [artex-global/docs/defense-en.md at main \- GitHub](https://github.com/jiwoochris/artex-global/blob/main/docs/defense-en.md)  
> 5. [Autopsy of an AI Hacker: Inside the Architecture of ARTEX Before](https://www.armelnene.com/blog/autopsy-of-an-ai-hacker-inside-the-architecture-of-artex-before-the-lights-went-out)  
> 6. [ztx888/ARTEX: AI 自主渗透测试系统\| 百度“agent+”攻防挑战赛冠军项目](https://github.com/ztx888/ARTEX)

---

*User prompt: 今天刷到deepseek开源了super-agent，给我讲讲这个是什么？以及能运用到该框架里么？为什么？*

Response: DeepSeek Harness 团队刚刚开源的 **Super-agent**（开源仓库名为 dsh-superagent-prototype，伴随着官方技术文章《Super-agent 设计探索：迈向更自主的 Agent 系统》发布），是一套旨在打造 **7×24 小时持续在线、跨平台自主干活的“超级智能体底座”**。

这套架构不仅能运用到你现有的 **ARTEX 技术标编写系统**中，而且两者的底层设计哲学高度同源，能以极低的认知成本实现深度互补。

### **一、 DeepSeek 开源的 Super-agent 到底是什么？**

传统的 Agent 往往困在单一的对话窗口（Session）里，所有任务串行执行，不仅慢，而且上下文极易互相污染（被 DeepSeek 称为“Session 焦虑”）。Super-agent 基于 DeepSeek Harness 插件化运行时构建，核心设计 philosophy 只有一句话：**“尽可能少地消耗人类的注意力，尽可能多地交付价值。”**

它的底层架构由**五大核心机制**组成：

> 1. **Router \+ Worker 双层架构（解耦连续性与并发执行）：**  
   * **Router 层（Main Session）：** 常驻在线，唯一负责对外承接人类在各个入口（如飞书群聊、GitHub 等）的交互、维护全局上下文、评估意图并分派任务单（Ticket）。  
   * **Worker 层（Executor Session）：** 在各自完全隔离的会话和独立文件目录中并发执行具体的 Ticket 任务。Worker 干完活不直接打扰用户，而是把状态汇报给 Router 统一对外沟通。  
> 2. **两级过滤与事件直通引擎（Event-Driven & Passthrough）：**  
   * 外部环境信号（群消息、评论、文档变更、报错等）统称为 **Trigger（触发器）**。  
   * **两级过滤：** 先用低成本的**程序规则**筛掉绝大部分无关噪音，再交给语义层（大模型 Eval）判断是否立项生成 Ticket。  
   * **事件直通（Subscription）：** 一旦某个 Worker 认领了任务并订阅了对应的沟通流（Thread），后续人类在该话题下的补充消息会由程序**直接路由给该 Worker**，绕过 Router 层，大幅降低中心路由负载和延迟。  
> 3. **Inline & Spill 与链式召回（Chain of Recall）：**  
   * **Inline & Spill（内联与溢出）：** 近期高频用到的信息直接内联进模型 Context Window；超出预算或低频长文则 Spill（溢出）保存在窗口外，供模型按需读取。  
   * **链式召回：** 通过 Trigger \-\> Thread \-\> Ticket \-\> Worker Session 之间的强引用 ID 关系，让模型顺着关联 ID 像查链表一样精准找回历史任务细节。  
> 4. **面向人的交互：异步 ReAct Loop 与投机执行（Speculative Execution）：**  
   * **异步 ReAct：** 将耗时的工具调用改造为异步的 start() 和 stop()，启动后台长任务后前台对话立刻释放，随时响应用户的新指令，后台跑完再以新事件注回会话。  
   * **投机执行：** 在没人明确 @ 它的情况下，根据环境信号在后台静默预判并开展工作（例如听到群里讨论某个需求或缺陷，后台悄悄拉分支尝试生成方案），只有确认成功做出高价值结果后，才主动呈现给人类。  
> 5. **外部独立 Watchdog（看门狗控制回路）：**  
   * 在主架构之外设有一个独立的 Watchdog Session。它不仅监控接口报错、死锁等系统问题，还会**用模型去审视 Worker 是否取得了实质进展、方向是否偏离目标（Mandate）**，发现跑偏时主动介入修复或熔断。

### **二、 能运用到你的 ARTEX 技术标框架里吗？为什么？**

**完全可以，而且非常契合。**

不过在工程落地上，**不建议推翻你的 Go \+ PostgreSQL 底座去硬套它的 TypeScript 代码**（dsh-superagent-prototype 目前是跑在 Node.js / DeepSeek Harness 上的研究原型），而是应该**将 Super-agent 的核心调度与上下文机制“吸收移植”到你的 ARTEX 架构中**。

原因在于以下三点深度契合与互补：

#### **1\. 架构同构性极高（天然无缝映射）**

你现有的 ARTEX 架构与 DeepSeek Super-agent 在顶层拓扑上几乎是镜像关系：

| ARTEX 技术标系统组件 | DeepSeek Super-agent 概念 | 融合与升级方向 |
| :---- | :---- | :---- |
| **Planner \+ MainAgent** | **Router 层 (Main Session \+ Eval)** | 引入 Super-agent 的**两级过滤与批处理合并机制**，防止多人协作修改标书时频繁唤醒 Planner 造成算力浪费。 |
| **Workers（撰稿专员）** | **Worker 层 (Executor Session)** | 引入 **Thread 订阅与事件直通机制**。当售前工程师针对“第 3 章某小节”连续提修改意见时，消息直接穿透给负责该章节的 Worker，无需每次经过全局 Planner 重算。 |
| **Intent（写作意图）** | **Ticket \+ Mandate（任务单与权限边界）** | 给每个章节的 Intent 加上不可篡改的 Mandate（规定该章节的字数预算、必须引用的招标条款、严禁修改的全局参数）。 |
| **双图谱记忆 \+ Trace 检索** | **Inline & Spill \+ Chain of Recall** | 用 Inline & Spill 规范化图谱节点如何装填进 Prompt；用 Chain of Recall 优化跨 Worker 的历史检索。 |

#### **2\. 精准补齐了 ARTEX 在“长文本管理”与“死循环监控”上的短板**

ARTEX 的双图谱虽然解决了客观事实（Asset）和推进链路（Exploration）的存储问题，但在标书这种**单节点动辄上万字**的场景下，很容易遇到两个工程瓶颈，而 Super-agent 刚好给出了标准答案：

> * **解决长标书撑爆上下文（引入 Inline & Spill）：** 编写技术标时，招标文件原文、历史业绩长文档极易塞满 Worker 的上下文。借鉴 Inline & Spill 机制，只将当前小节的评分点摘要和核心架构参数 **Inline** 进上下文；将几万字的详细技术白皮书、长篇历史标书参考范文 **Spill** 到外部存储（或作为图谱冷数据），仅在 Prompt 中保留指针 ID，让 Worker 通过 Chain of Recall 按需调取段落。  
> * **防止撰稿 Agent 幻觉跑偏（引入独立 Watchdog）：** 技术标最怕某个 Worker 在写分项方案时“自嗨”写偏题，或者陷入死循环反复调用检索工具。在 ARTEX 的调度循环外加一个轻量级的 **Watchdog 协程**，定期巡检各个正在撰写的章节 Worker 是否偏离了招标文件的评分标准（Mandate），一旦发现偏离立即熔断并注入修正提示。

#### **3\. 将技术标系统从“被动生成工具”升级为“主动投标协作者”**

目前的标书系统大多是人类点一下“生成”，在页面前干等几十分钟。结合 Super-agent 的两大交互创新，体验会产生质变：

> * **群聊潜伏与投机执行（Speculative Execution）：** 投标往往是一个团队在飞书/企微群里反复拉锯的过程。你可以接入飞书 Source（dsh-superagent 原生支持飞书事件流）。当售前和产品经理在群里讨论：“客户刚刚口头提出要把并发性能指标提高到 10 万 QPS，数据库改用分布式架构”，你的系统在后台触发**投机执行**——静默检索资产库、重新核算服务器硬件配置表、并生成修改后的技术章节草稿。等确认无误后，直接在群里冒泡推送：“已根据群内讨论准备好第 4 章架构调整草稿及新的硬件清单，是否应用到主标书？”  
> * **异步 ReAct 释放前台交互：** 生成几百页的 Word 排版、渲染高清架构图或跑全书合规性扫描非常耗时。采用异步 start() / stop() 模式后，售前人员在下达“全书生成”指令的同时，依然可以在前台顺畅地和 Router 对话调整商务偏离表，后台章节生成完毕后以事件流形式自动合并入图谱。

**Sources**

> 1. [DeepSeek重磅开源：超级Agent架构引领AI新纪元 \- 搜狐](https://m.sohu.com/a/1095494694_122004016?scm=10001.325_13-325_13.0.0-0-0-0-0.5_1334)  
> 2. [Router 与Worker 双层架构，探索更自主的Agent 系统- IT之家](https://www.ithome.com/1/020/615.htm)  
> 3. [dsh-superagent-prototype/docs/001-domain.md at main \- GitHub](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/main/docs/001-domain.md)

---

