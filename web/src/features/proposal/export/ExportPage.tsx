import { OperatorField } from '../../task-workspace/OperatorField'
import { useTaskWorkspace, useTick } from '../../task-workspace/TaskWorkspaceProvider'
import { ExportPanel } from './ExportParts'

// 成稿导出：导出门禁（含阻断跳转）、依赖提示、模板与格式选择、历史导出任务、提交检查清单。
// 提交检查清单的每次确认都要写明操作人，所以这一页也要能填写“当前操作人”。
export default function ExportPage() {
  const { taskId, invalidate } = useTaskWorkspace()
  const tick = useTick('export', 'exportJobs', 'templates', 'business')
  return (
    <div className="mx-auto max-w-6xl space-y-3">
      <div className="flex justify-end"><OperatorField /></div>
      <ExportPanel taskID={taskId} tick={tick} onCreated={() => invalidate('export_created')} />
    </div>
  )
}
