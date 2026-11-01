import { useTaskWorkspace, useTick } from '../../task-workspace/TaskWorkspaceProvider'
import { ExportPanel } from './ExportParts'

// 成稿导出：导出门禁（含阻断跳转）、依赖提示、模板与格式选择、历史导出任务。
export default function ExportPage() {
  const { taskId, invalidate } = useTaskWorkspace()
  const tick = useTick('export', 'exportJobs', 'templates', 'business')
  return (
    <div className="mx-auto max-w-6xl">
      <ExportPanel taskID={taskId} tick={tick} onCreated={() => invalidate('export_created')} />
    </div>
  )
}
