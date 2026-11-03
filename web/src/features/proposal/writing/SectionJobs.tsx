import { useEffect, useRef, useState } from 'react'
import { api, errorText } from '../../../api/client'
import { Button, ErrorBanner, Notice } from '../../../components/ui'
import { useAsync, useInterval } from '../../../hooks/useAsync'

const labels: Record<string, string> = { queued: '等待写作', running: '正在写作', awaiting_review: '已生成，待评审', candidate: '已保留为候选稿', blocked: '写作受阻', cancelled: '已取消' }

export default function SectionJobs({ taskId, sectionId, onChange }: { taskId: number; sectionId: number; onChange: () => void }) {
  const jobs = useAsync(() => api.sectionJobs(taskId), [taskId, sectionId])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  useInterval(() => void jobs.reload(), 3000)
  const job = jobs.data?.find((j) => j.section_id === sectionId)
  const changed = useRef(onChange)
  changed.current = onChange
  useEffect(() => { if (job) changed.current() }, [job?.job_id, job?.status])
  if (!job) return jobs.error ? <ErrorBanner error={jobs.error} onRetry={jobs.reload} /> : null
  const active = job.status === 'queued' || job.status === 'running'
  return <div className="border-b border-line p-3">
    <Notice tone={job.status === 'blocked' ? 'danger' : undefined} action={active ? <Button size="sm" loading={busy} onClick={async () => {
      setBusy(true); setError('')
      try { await api.cancelSectionJob(taskId, job.job_id); await jobs.reload() } catch (e) { setError(errorText(e)) } finally { setBusy(false) }
    }}>取消写作</Button> : undefined}>
      {labels[job.status] ?? job.status} · 已尝试 {job.attempts}/{job.max_attempts} 次
      {job.error && <div className="mt-1">{job.error}</div>}
      {job.status === 'blocked' && <div className="mt-1">处理原因后，可重新发起 AI 写作。</div>}
    </Notice>
    <ErrorBanner error={error || jobs.error} />
  </div>
}
