import { AlertCircle, ArrowRight, CheckCircle2, CircleDashed, Clock3, RefreshCw, TriangleAlert } from 'lucide-react'
import type { ReactNode } from 'react'
import { api } from '../../api/client'
import { Badge, Button, Card, ErrorBanner, Spinner, cx } from '../../components/ui'
import type { Tone } from '../../components/ui'
import { useAsync } from '../../hooks/useAsync'
import { fmtBeijing, fmtTime } from '../../lib/format'
import { linkProps } from '../../lib/router'
import BlockerList from './BlockerList'
import { deriveSteps, nextStep } from './progress'
import type { DerivedState } from './progress'
import { RunStatusBadge } from './TaskHeader'
import { useTaskWorkspace, useTick } from './TaskWorkspaceProvider'

const stateMeta: Record<DerivedState, { label: string; tone: Tone; icon: ReactNode }> = {
  unknown: { label: '待判定', tone: 'slate', icon: <CircleDashed className="size-4 text-muted" /> },
  empty: { label: '尚无数据', tone: 'slate', icon: <CircleDashed className="size-4 text-muted" /> },
  action_required: { label: '需要处理', tone: 'amber', icon: <TriangleAlert className="size-4 text-warn" /> },
  in_progress: { label: '进行中', tone: 'blue', icon: <Clock3 className="size-4 text-link" /> },
  complete: { label: '已完成', tone: 'green', icon: <CheckCircle2 className="size-4 text-ok" /> },
  stale: { label: '已失效', tone: 'red', icon: <AlertCircle className="size-4 text-danger" /> },
  error: { label: '读取失败', tone: 'red', icon: <AlertCircle className="size-4 text-danger" /> },
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0 rounded-[10px] border border-line px-4 py-3">
      <div className="text-xs text-muted">{label}</div>
      <div className="mt-1 min-w-0 text-sm">{children}</div>
    </div>
  )
}

// 项目概览：复用要求清单、大纲、章节状态与导出就绪度；「下一步」只是导航建议，不自动推进流程。
export default function OverviewPage() {
  const { taskId, task, outlines } = useTaskWorkspace()
  const tReq = useTick('requirements', 'business')
  const tSec = useTick('sections', 'outline', 'business')
  const tExp = useTick('export', 'business', 'matrix', 'consistency')
  const reqs = useAsync(() => api.requirements(taskId), [taskId, tReq])
  const approvedVersion = outlines.data?.approved_version ?? 0
  const approved = useAsync(() => (approvedVersion ? api.getOutline(taskId, approvedVersion) : Promise.resolve(null)), [taskId, approvedVersion, tSec], !!outlines.data)
  const readiness = useAsync(() => api.exportReadiness(taskId), [taskId, tExp])

  const steps = deriveSteps(taskId, {
    requirements: { data: reqs.data, error: reqs.error },
    outlines: { data: outlines.data, error: outlines.error },
    approvedOutline: { data: approved.data, error: approved.error },
    readiness: { data: readiness.data, error: readiness.error },
  })
  const next = nextStep(steps)
  const reload = () => { void reqs.reload(); void outlines.reload(); void approved.reload(); void readiness.reload(); void task.reload() }
  const t = task.data
  const m = reqs.data?.manifest ?? outlines.data?.manifest
  const src = m?.bid_deadline_source
  const lots = reqs.data?.stats.lots ?? []

  return (
    <div className="mx-auto max-w-6xl space-y-6">
      <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <Fact label="投标截止（北京时间）">
          {!m ? <span className="text-muted">{reqs.error ? '读取失败' : '读取中'}</span>
            : m.bid_deadline ? <><span className="font-semibold">{fmtBeijing(m.bid_deadline)}</span>
              <div className="text-xs text-muted">{src?.origin === 'manual' ? `人工指定${src.by ? ` · ${String(src.by)}` : ''}` : `原文抽取${src?.page ? ` · 第 ${src.page} 页` : ''}`}</div></>
            : <><span className="font-semibold text-warn">尚未确认</span><div className="text-xs text-muted">在「要求与否决项」的投标范围中确认</div></>}
        </Fact>
        <Fact label="投标分包">
          {!reqs.data ? <span className="text-muted">{reqs.error ? '读取失败' : '读取中'}</span>
            : lots.length === 0 ? '不分包'
            : m?.bid_lots.length ? `包 ${m.bid_lots.join('、')}（共 ${lots.length} 个分包）` : `全部 ${lots.length} 个分包`}
        </Fact>
        <Fact label="运行状态">
          {t ? <><RunStatusBadge task={t} />
            <div className="mt-1 text-xs text-muted">运行期限 {t.deadline_at ? fmtTime(t.deadline_at) : '不限时'}（推演超时，不是投标截止）</div></> : <span className="text-muted">读取中</span>}
        </Fact>
        <Fact label="当前批准大纲">
          {!outlines.data ? <span className="text-muted">{outlines.error ? '读取失败' : '读取中'}</span>
            : approvedVersion ? <span className="font-semibold">v{approvedVersion}</span> : <span className="text-warn">尚无批准版本</span>}
          {outlines.data?.open_version ? <div className="text-xs text-muted">v{outlines.data.open_version} 修订中</div> : null}
        </Fact>
      </div>

      <Card title="下一步建议" subtitle="依据当前读取的业务状态生成，只用于导航；是否允许审批、写作或导出仍由相应接口判定。"
        actions={<Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={reload}>刷新</Button>}>
        {next ? (
          <div className="flex flex-wrap items-center gap-3">
            {stateMeta[next.state].icon}
            <div className="min-w-0 flex-1">
              <div className="font-semibold">{next.label}</div>
              <div className="text-sm text-muted">{next.detail}</div>
            </div>
            <a {...linkProps(next.href)} className="inline-flex h-8 items-center gap-1.5 rounded-[10px] bg-primary px-2.5 text-sm font-medium text-[#fafafa] hover:bg-primary-hover">
              前往处理<ArrowRight className="size-4" aria-hidden /></a>
          </div>
        ) : steps.every((s) => s.state === 'complete')
          ? <p className="text-sm text-ok">所有步骤当前没有待处理事项。导出成功只表示产物生成，不代表已签章或已提交交易平台。</p>
          : <Spinner />}
      </Card>

      <Card title="业务进度" bodyClassName="p-0">
        <ol className="divide-y divide-line-soft">
          {steps.map((s, i) => (
            <li key={s.key} className="flex flex-wrap items-center gap-3 px-4 py-3">
              <span className="w-5 text-xs text-muted tabular-nums">{i + 1}</span>
              {stateMeta[s.state].icon}
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-2"><span className="font-medium">{s.label}</span><Badge dot tone={stateMeta[s.state].tone}>{stateMeta[s.state].label}</Badge></div>
                <div className={cx('text-sm', s.state === 'error' ? 'text-danger' : 'text-muted')}>{s.detail}</div>
              </div>
              <a {...linkProps(s.href)} className="text-sm text-link hover:underline">打开</a>
            </li>
          ))}
        </ol>
      </Card>

      <Card title={<>导出门禁 {readiness.data && (readiness.data.ready ? <Badge dot tone="green">当前可导出</Badge> : <Badge dot tone="red">{readiness.data.blockers.length} 项阻断</Badge>)}</>}
        subtitle={readiness.data ? `检查于 ${fmtTime(readiness.data.checked_at)}；创建导出时服务端会在锁定快照时再次校验` : undefined}>
        <ErrorBanner error={readiness.error} onRetry={readiness.reload} />
        {!readiness.data ? (!readiness.error && <Spinner />)
          : readiness.data.ready ? <p className="text-sm text-ok">没有阻断事项，可在「成稿导出」锁定版本并导出。</p>
          : <BlockerList taskId={taskId} blockers={readiness.data.blockers} max={8} />}
      </Card>
    </div>
  )
}
