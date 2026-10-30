import { api } from '../api/client'
import { InterceptList } from '../components/Intercepts'
import { PageFrame, useShell } from '../components/layout/AppShell'
import { useAsync, useInterval } from '../hooks/useAsync'

export default function InterceptsPage() {
  const shell = useShell()
  const rows = useAsync(() => api.listIntercepts(), [])
  useInterval(() => void rows.reload(), 5000)
  return (
    <PageFrame title="工具调用审批"
      description="全部项目中等待人工裁决的在途工具调用（每 5 秒刷新）。只适用于短时工具调用：等待方心跳过期、工作被取消或服务重启时，挂起请求按拒绝回收。大纲与章节的业务审批在各项目的「大纲与写作」中进行。">
      <InterceptList rows={rows.data} loading={rows.loading} error={rows.error} onReload={rows.reload} showTask
        onDecided={() => { void rows.reload(); shell.refreshPending() }} />
    </PageFrame>
  )
}
