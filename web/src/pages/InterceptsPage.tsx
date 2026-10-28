import { api } from '../api/client'
import { InterceptList } from '../components/Intercepts'
import { useAsync, useInterval } from '../hooks/useAsync'

export default function InterceptsPage({ onChanged }: { onChanged: () => void }) {
  const rows = useAsync(() => api.listIntercepts(), [])
  useInterval(() => void rows.reload(), 5000)
  return (
    <div className="mx-auto max-w-5xl space-y-4 p-6">
      <div>
        <h1 className="text-xl font-semibold">工具调用审批</h1>
        <p className="text-sm text-slate-500">
          全部任务中等待人工裁决的在途工具调用（每 5 秒刷新）。审批仅适用于短时工具调用：等待方心跳过期、工作被取消或服务重启时，
          挂起请求会被自动按拒绝回收；大纲/章节等长周期业务审批将在 Phase 5 以持久化状态机实现。
        </p>
      </div>
      <InterceptList rows={rows.data} loading={rows.loading} error={rows.error} onReload={rows.reload} showTask
        onDecided={() => { void rows.reload(); onChanged() }} />
    </div>
  )
}
