import { CurrentAccount } from '../../task-workspace/CurrentAccount'
import { useTaskWorkspace, useTick } from '../../task-workspace/TaskWorkspaceProvider'
import { ExportPanel } from './ExportParts'

// 成稿导出：导出门禁（含阻断跳转）、依赖提示、模板与格式选择、历史导出任务、提交检查清单。
// 提交检查清单的每次确认记录的是登录账号；这里显示当前账号与本项目角色（只有投标经理能确认）。
export default function ExportPage() {
  const { taskId, invalidate } = useTaskWorkspace()
  const tick = useTick('export', 'exportJobs', 'templates', 'business')
  return (
    <div className="mx-auto max-w-6xl space-y-3">
      <div className="flex justify-end"><CurrentAccount /></div>
      <ExportPanel taskID={taskId} tick={tick} onCreated={() => invalidate('export_created')} />
    </div>
  )
}
