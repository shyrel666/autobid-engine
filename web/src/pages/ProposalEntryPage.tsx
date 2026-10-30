import { useEffect } from 'react'
import { ArrowRight } from 'lucide-react'
import { api } from '../api/client'
import { PageFrame } from '../components/layout/AppShell'
import { Empty, ErrorBanner, Spinner } from '../components/ui'
import { RunStatusBadge } from '../features/task-workspace/TaskHeader'
import { useAsync } from '../hooks/useAsync'
import { fmtTime } from '../lib/format'
import { rememberProjects } from '../lib/projectCache'
import { linkProps } from '../lib/router'
import { taskUrl } from '../lib/routes'

// 旧写作入口 /proposal 保留为项目选择页：默认进入「大纲及审批」；?intent=export 进入「成稿导出」。
// 写作与导出属于具体项目，不在全局导航中重复出现。
export default function ProposalEntryPage({ intent }: { intent: 'outline' | 'export' }) {
  const tasks = useAsync(() => api.listTasks(), [])
  useEffect(() => { if (tasks.data) rememberProjects(tasks.data) }, [tasks.data])
  const target = intent === 'export' ? 'export' : 'outline'
  return (
    <PageFrame title={intent === 'export' ? '选择要导出成稿的项目' : '选择要编写大纲与章节的项目'}
      description={intent === 'export' ? '成稿导出在项目内进行：先通过导出门禁，再锁定章节修订导出 DOCX / PDF。' : '要求清单、大纲审批与章节修订都按项目隔离。'}>
      <section className="overflow-hidden rounded-[14px] border border-line">
        <ErrorBanner error={tasks.error} onRetry={tasks.reload} />
        {tasks.loading && !tasks.data ? <Spinner /> : !tasks.data?.length ? <Empty>暂无投标项目，请先在「投标项目」中新建并导入招标文件。</Empty> : (
          <ul className="divide-y divide-line-soft">
            {tasks.data.map((t) => (
              <li key={t.id}>
                <a {...linkProps(taskUrl(t.id, target))} className="flex flex-wrap items-center gap-3 px-4 py-3 hover:bg-soft">
                  <span className="rounded bg-soft px-1.5 py-px text-xs text-muted">{t.id}</span>
                  <span className="min-w-0 flex-1 truncate font-semibold">{t.name}</span>
                  <RunStatusBadge task={t} />
                  <span className="text-xs text-muted">{fmtTime(t.created_at)}</span>
                  <ArrowRight className="size-4 text-muted" aria-hidden />
                </a>
              </li>
            ))}
          </ul>
        )}
      </section>
      <p className="mt-2 text-xs text-muted">已加载 {tasks.data?.length ?? 0} 个项目（客户端上限 500 条）。</p>
    </PageFrame>
  )
}
