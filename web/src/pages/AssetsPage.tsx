import { useEffect, useState } from 'react'
import { ArrowUpRight, Network, RefreshCw } from 'lucide-react'
import { api } from '../api/client'
import { PageFrame } from '../components/layout/AppShell'
import { Card, Empty, ErrorBanner, IconButton, Select, Spinner } from '../components/ui'
import { useAsync } from '../hooks/useAsync'
import { linkProps } from '../lib/router'
import { taskUrl } from '../lib/routes'
import ElementsTab from './task/ElementsTab'

export default function AssetsPage() {
  const tasks = useAsync(() => api.listTasks(), [])
  const [selectedTaskId, setSelectedTaskId] = useState<number>(0)

  const list = tasks.data ?? []

  useEffect(() => {
    if (selectedTaskId === 0 && list.length > 0) {
      setSelectedTaskId(list[0].id)
    }
  }, [list, selectedTaskId])

  const currentTask = list.find((t) => t.id === selectedTaskId)

  return (
    <PageFrame
      title="招标要素"
      description="浏览各投标项目的招标静态要素树（标段、条款、资格、评分项、商务项）、要素覆盖折叠图与推演探索图"
      actions={
        <div className="flex flex-wrap items-center gap-2">
          {list.length > 0 && (
            <Select
              aria-label="选择投标项目"
              className="w-64"
              value={selectedTaskId}
              onChange={(e) => setSelectedTaskId(Number(e.target.value))}
            >
              {list.map((t) => (
                <option key={t.id} value={t.id}>
                  #{t.id} {t.name}
                </option>
              ))}
            </Select>
          )}
          {currentTask && (
            <a
              {...linkProps(taskUrl(currentTask.id, 'overview'))}
              className="inline-flex h-8 items-center gap-1 rounded-[10px] border border-line bg-panel px-2.5 text-xs font-medium text-fg hover:bg-soft"
            >
              打开项目工作台 <ArrowUpRight className="size-3.5" />
            </a>
          )}
          <IconButton label="刷新项目列表" onClick={() => void tasks.reload()}>
            <RefreshCw className="size-4" />
          </IconButton>
        </div>
      }
    >
      <ErrorBanner error={tasks.error} onRetry={tasks.reload} />
      {tasks.loading && !tasks.data ? (
        <Spinner />
      ) : list.length === 0 ? (
        <Card>
          <Empty>
            <div className="flex flex-col items-center gap-2">
              <Network className="size-6 text-muted" />
              <span>暂无投标项目，请先在「投标项目」页面创建并导入招标文件。</span>
            </div>
          </Empty>
        </Card>
      ) : selectedTaskId > 0 ? (
        <ElementsTab key={selectedTaskId} taskID={selectedTaskId} />
      ) : null}
    </PageFrame>
  )
}
