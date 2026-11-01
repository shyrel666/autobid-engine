import { useEffect, useRef, useState } from 'react'
import { eventsURL } from '../api/client'
import type { Activity } from '../api/types'

export type StreamStatus = 'connecting' | 'open' | 'reconnecting' | 'closed'

export interface StreamEvent extends Activity {
  /** 服务端 SSE 序号；服务重启后可能重复，不能单独作为 key。 */
  seq: string
  /** 客户端稳定唯一编号：连接代次 + 本地递增序号，用作列表 key。 */
  uid: string
  /** 第几次连接收到（重连后递增），用于区分服务重启前后的同号事件。 */
  gen: number
}

interface Options {
  onActivity?: (a: StreamEvent) => void
  // onReconnect 在断线重连成功后调用，用于重读业务快照与工具审批。
  onReconnect?: () => void
  max?: number
}

// useTaskEvents 订阅 GET /api/tasks/{id}/events：解析 event: activity 与 event: dropped；
// 浏览器自动重连失败（例如 401 或服务重启导致连接被关闭）时以指数退避手动重建连接。
// 事件按接收顺序从旧到新保存，客户端最多保留 max 条（默认 300），超出部分从最旧处淘汰并计入 trimmed。
// 只是本次打开期间的实时活动：不持久化、不补拉断开期间的事件。
export function useTaskEvents(taskID: number | undefined, opts: Options = {}) {
  const [events, setEvents] = useState<StreamEvent[]>([])
  const [status, setStatus] = useState<StreamStatus>('closed')
  const [dropped, setDropped] = useState(0)
  const [reconnects, setReconnects] = useState(0)
  const [trimmed, setTrimmed] = useState(0)
  const optsRef = useRef(opts)
  optsRef.current = opts
  const held = useRef(0)
  const max = opts.max ?? 300

  useEffect(() => {
    if (!taskID) return
    let es: EventSource | null = null
    let timer: ReturnType<typeof setTimeout> | undefined
    let disposed = false
    let everOpened = false
    let backoff = 1000
    let gen = 0
    let counter = 0
    setEvents([])
    held.current = 0
    setDropped(0)
    setTrimmed(0)
    setReconnects(0)

    const connect = () => {
      if (disposed) return
      gen++
      const myGen = gen
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
          const ev: StreamEvent = { ...a, seq: me.lastEventId, gen: myGen, uid: `${myGen}-${++counter}` }
          held.current++
          if (held.current > max) { held.current = max; setTrimmed((n) => n + 1) }
          setEvents((list) => (list.length < max ? [...list, ev] : [...list.slice(list.length + 1 - max), ev]))
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

  return { events, status, dropped, reconnects, trimmed, max, clear: () => { setEvents([]); held.current = 0; setTrimmed(0) } }
}

export type TaskStream = ReturnType<typeof useTaskEvents>
