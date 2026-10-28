import { BookOpen, Construction, FileOutput, PenTool } from 'lucide-react'
import type { ReactNode } from 'react'
import { Badge, Card } from '../components/ui'

interface Plan {
  phase: string
  title: string
  icon: ReactNode
  gaps: string
  summary: string
  items: string[]
  apis: string[]
}

// 仅展示 ROADMAP.md 中的规划；这些模块的后端接口尚未实现，本页不调用任何接口、不展示假数据。
const plans: Record<string, Plan> = {
  phase4: {
    phase: 'Phase 4', title: '多格式文档解析与企业方案知识库', icon: <BookOpen className="size-5" />, gaps: '缺口 #1、#4',
    summary: '上传 PDF / Word / 扫描件并还原章节、页码与跨页表格；沉淀历史标书、产品资料与标准方案，供 Worker 检索引用。',
    items: [
      'POST /api/tasks/{id}/documents 支持 multipart/form-data 上传 .docx / .pdf / 扫描件（兼容现有 JSON 文本上传）',
      '章节、页码范围（page_start / page_end）与坐标溯源；跨页表格还原为 JSON + Markdown 表格；可插拔 OCR',
      'PostgreSQL knowledge_documents / knowledge_chunks（tsvector GIN 全文 + 可选 pgvector 混合检索，含 tenant_id）',
      'kb_search_proposals、kb_search_products、kb_read_chunk 工具，检索结果强制附带文档名、章节号与页码引用',
    ],
    apis: ['知识库文档管理 API（待定义）'],
  },
  phase5: {
    phase: 'Phase 5', title: '要求清单、大纲审批与章节写作', icon: <PenTool className="size-5" />, gaps: '缺口 #2、#3、#5',
    summary: '先形成可复核的投标要求清单，再生成并持久化审批技术标大纲，按依赖顺序派发章节写作并以乐观锁管理多版本修订。',
    items: [
      'requirement_items：评分项、★/▲ 强制条款、非评分必答项与格式附件要求，支持人工补充与“清单已完整”确认',
      'proposal_outlines / proposal_sections：draft → pending_approval → approved → superseded 持久化状态机（不依赖短时 intercept）',
      '派发入口与在途 Worker 写回双端校验 based_on_outline_version；大纲过期的写回仅存为 stale_outline 候选稿',
      'section_revisions：base_rev_no 乐观锁，人工修改绝不被滞后的 AI 写回覆盖；批准绑定具体版本，重写自动撤销批准',
    ],
    apis: ['要求清单 / 大纲 / 章节版本 API（待定义）'],
  },
  phase6: {
    phase: 'Phase 6', title: '图表、一致性核验与成稿套版导出', icon: <FileOutput className="size-5" />, gaps: '缺口 #6、#7、#8',
    summary: '生成架构图、甘特图等标书图表；双层核验要求覆盖与正文实质响应并检查全文一致性；锁定已批准版本套模板导出 DOCX / PDF。',
    items: [
      'proposal_charts + chart_generate：Mermaid/SVG 架构图、实施甘特图、组织架构图、设备选型表，绑定章节版本',
      'compliance_matrix：大纲映射率与正文实质响应率两层验收；要求或正文变更自动标记 stale 待重检',
      'fulltext_consistency_check：工期、质保期、SLA、团队人数、软硬件型号等关键承诺前后一致性',
      'export_jobs + export/：锁定 section_id → rev_no 快照，套用 .docx 模板，渲染 TOC 页码、页眉页脚与《点对点应答及偏离表》',
    ],
    apis: ['导出任务 API（待定义）'],
  },
}

export default function RoadmapPage({ phase }: { phase: string }) {
  const p = plans[phase] ?? plans.phase4
  return (
    <div className="mx-auto max-w-4xl space-y-4 p-6">
      <div className="flex items-center gap-3">
        <div className="flex size-10 items-center justify-center rounded-lg bg-slate-100 text-slate-500">{p.icon}</div>
        <div>
          <h1 className="flex items-center gap-2 text-xl font-semibold">{p.title} <Badge>{p.phase}</Badge> <Badge tone="amber">规划中</Badge></h1>
          <p className="text-sm text-slate-500">对应 ROADMAP.md {p.gaps}</p>
        </div>
      </div>
      <div className="flex items-start gap-2 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-800">
        <Construction className="mt-0.5 size-4 shrink-0" />
        该模块的后端表结构、工具与接口尚未实现。本页仅说明规划范围，不调用接口、不展示模拟数据；实施后将在此处接入真实功能。
      </div>
      <Card title="模块目标"><p className="text-sm text-slate-700">{p.summary}</p></Card>
      <Card title="计划交付">
        <ul className="list-disc space-y-2 pl-5 text-sm text-slate-700">{p.items.map((i) => <li key={i}>{i}</li>)}</ul>
      </Card>
      <Card title="前端接入点">
        <ul className="space-y-1 text-sm text-slate-500">{p.apis.map((a) => <li key={a}>{a}</li>)}</ul>
        <p className="mt-2 text-xs text-slate-400">所有新增业务表预留 tenant_id 与 created_by / updated_by / approved_by，为 Phase 7 多租户 RBAC 做准备。</p>
      </Card>
    </div>
  )
}
