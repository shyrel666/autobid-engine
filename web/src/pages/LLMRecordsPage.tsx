import { RefreshCw } from 'lucide-react'
import { api } from '../api/client'
import { PageFrame } from '../components/layout/AppShell'
import { ErrorBanner, IconButton } from '../components/ui'
import { LLMRecordsPanel } from '../features/models/LLMRecordsPanel'
import { useAsync } from '../hooks/useAsync'

export default function LLMRecordsPage() {
  const settings = useAsync(() => api.llmSettings(), [])
  return (
    <PageFrame
      title="LLM 录制"
      description="录制与回放 LLM 供应商 HTTP 原始请求体与响应体（自动脱敏 API Key），用于排查工具调用与推理输出"
      actions={
        <IconButton label="刷新录制配置" onClick={() => void settings.reload()}>
          <RefreshCw className="size-4" />
        </IconButton>
      }
    >
      <ErrorBanner error={settings.error} onRetry={settings.reload} />
      <LLMRecordsPanel
        settings={settings.data ?? undefined}
        onSettingsUpdated={(s) => settings.setData(s)}
      />
    </PageFrame>
  )
}
