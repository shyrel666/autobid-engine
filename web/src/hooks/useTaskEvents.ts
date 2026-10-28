import { useEffect, useRef, useState } from 'react'
import { eventsURL } from '../api/client'
import type { Activity } from '../api/types'

export type StreamStatus = 'connecting' | 'open' | 'reconnecting' | 'closed'

export interface StreamEvent extends Activity {
  seq: string
}

interface Options {
  onActivity?: (a: StreamEvent) => void
  // onReconnect 在断线重连成功后调用，用于重拉 overview / intercepts 恢复状态。
  onReconnect?: () => void
  max?: number
}

// useTaskEvents 订阅 GET /api/tasks/{id}/events：解析 event: activity 与 event: dropped；
// 浏览器自动重连失败（例如 401 或服务重启导致连接被关闭）时以指数退避手动重建连接。
export function useTaskEvents(taskID: number | undefined, opts: Options = {}) {
  const [events, setEvents] = useState<StreamEvent[]>([])
  const [status, setStatus] = useState<StreamStatus>('closed')
  const [dropped, setDropped] = useState(0)
  const [reconnects, setReconnects] = useState(0)
  const optsRef = useRef(opts)
  optsRef.current = opts
  const max = opts.max ?? 300

  useEffect(() => {
    if (!taskID) return
    let es: EventSource | null = null
    let timer: ReturnType<typeof setTimeout> | undefined
    let disposed = false
    let everOpened = false
    let backoff = 1000
    setEvents([])
    setDropped(0)

    const connect = () => {
      if (disposed) return
      setStatus(everOpened ? 'reconnecting' : 'connecting')
      const src = new EventSource(eventsURL(taskID))
      es = src
      src.onopen = () => {
        backoff = 1000
        setStatus('open')
        if (everOpened) {
          setReconnects((n) => n + 1)
          optsRef.current.onReconnect?.()
        }
        everOpened = true
      }
      src.addEventListener('activity', (e) => {
        const me = e as MessageEvent<string>
        try {
          const a = JSON.parse(me.data) as Activity
          const ev: StreamEvent = { ...a, seq: me.lastEventId }
          setEvents((list) => {
            const next = [ev, ...list]
            return next.length > max ? next.slice(0, max) : next
          })
          optsRef.current.onActivity?.(ev)
        } catch {
          /* 忽略无法解析的事件 */
        }
      })
      src.addEventListener('dropped', (e) => {
        try {
          const d = JSON.parse((e as MessageEvent<string>).data) as { dropped: number }
          setDropped(d.dropped)
        } catch {
          /* ignore */
        }
      })
      src.onerror = () => {
        if (disposed || es !== src) return
        if (src.readyState === EventSource.CLOSED) {
          src.close()
          setStatus('reconnecting')
          timer = setTimeout(connect, backoff)
          backoff = Math.min(backoff * 2, 30000)
        } else {
          setStatus('reconnecting') // 浏览器正在自动重连
        }
      }
    }
    const onToken = () => {
      es?.close()
      es = null
      clearTimeout(timer)
      connect()
    }
    connect()
    window.addEventListener('autobid:token-changed', onToken)
    return () => {
      disposed = true
      clearTimeout(timer)
      es?.close()
      setStatus('closed')
      window.removeEventListener('autobid:token-changed', onToken)
    }
  }, [taskID, max])

  return { events, status, dropped, reconnects, clear: () => setEvents([]) }
}
