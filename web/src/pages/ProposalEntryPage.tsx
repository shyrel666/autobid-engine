import { ArrowRight, PenTool } from 'lucide-react'
import { api } from '../api/client'
import { Badge, Button, Card, Empty, ErrorBanner, Spinner, statusTone } from '../components/ui'
import { useAsync } from '../hooks/useAsync'
import { fmtTime, outlineStatusLabel, taskStatusLabel } from '../lib/format'
import { navigate } from '../lib/router'

// 技术标大纲与写作入口：要求清单、大纲审批与章节修订都按任务隔离，这里列出任务并跳到任务工作台的 proposal 页签。
function OutlineBadge({ taskID }: { taskID: number }) {
  const list = useAsync(() => api.outlines(taskID), [taskID])
  const l = list.data
  if (!l) return <span className="text-xs text-slate-400">{list.error ? '读取失败' : '…'}</span>
  if (l.outlines.length === 0) return <Badge>尚无大纲</Badge>
  const latest = l.outlines[0]
  return (
    <span className="flex flex-wrap items-center gap-1">
      {l.approved_version ? <Badge tone="green">批准 v{l.approved_version}</Badge> : null}
      {latest.status !== 'approved' && <Badge tone={latest.status === 'pending_approval' ? 'amber' : latest.status === 'draft' ? 'blue' : 'slate'}>
        v{latest.version} {outlineStatusLabel[latest.status]}</Badge>}
    </span>
  )
}

export default function ProposalEntryPage() {
  const tasks = useAsync(() => api.listTasks(), [])
  return (
    <div className="mx-auto max-w-5xl space-y-4 p-6">
      <div className="flex items-center gap-3">
        <div className="flex size-10 items-center justify-center rounded-lg bg-blue-50 text-blue-600"><PenTool className="size-5" /></div>
        <div>
          <h1 className="text-xl font-semibold">技术标大纲与写作</h1>
          <p className="text-sm text-slate-500">可复核要求清单 → 大纲持久化审批 → 按依赖派发章节写作与多版本修订（ROADMAP Phase 5，缺口 #2、#3、#5）</p>
        </div>
      </div>
      <Card title="选择任务">
        <ErrorBanner error={tasks.error} onRetry={tasks.reload} />
        {tasks.loading && !tasks.data ? <Spinner /> : !tasks.data?.length ? <Empty>暂无任务，请先在「任务与招标文档」中创建任务并上传招标文件。</Empty> : (
          <table className="w-full text-sm">
            <thead><tr className="border-b border-slate-200 text-left text-xs text-slate-500">
              <th className="py-2 pr-2">任务</th><th className="pr-2">状态</th><th className="pr-2">大纲</th><th className="pr-2">创建时间</th><th />
            </tr></thead>
            <tbody>{tasks.data.map((t) => (
              <tr key={t.id} className="border-b border-slate-100">
                <td className="py-2 pr-2"><span className="text-slate-400">#{t.id}</span> {t.name}</td>
                <td className="pr-2"><Badge tone={statusTone(t.status)}>{taskStatusLabel[t.status] ?? t.status}</Badge></td>
                <td className="pr-2"><OutlineBadge taskID={t.id} /></td>
                <td className="pr-2 text-xs text-slate-500">{fmtTime(t.created_at)}</td>
                <td className="text-right"><Button size="sm" icon={<ArrowRight className="size-3.5" />} onClick={() => navigate(`/tasks/${t.id}/proposal`)}>进入写作</Button></td>
              </tr>
            ))}</tbody>
          </table>
        )}
      </Card>
    </div>
  )
}
