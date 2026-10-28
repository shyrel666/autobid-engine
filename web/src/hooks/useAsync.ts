import { useCallback, useEffect, useRef, useState } from 'react'
import { errorText } from '../api/client'

export interface AsyncState<T> {
  data: T | undefined
  error: string
  loading: boolean
  reload: () => Promise<void>
  setData: (v: T | undefined) => void
}

// useAsync 在 deps 变化时执行 fn；reload 可手动重拉，过期请求的结果被丢弃。
export function useAsync<T>(fn: () => Promise<T>, deps: unknown[], enabled = true): AsyncState<T> {
  const [data, setData] = useState<T>()
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(enabled)
  const seq = useRef(0)
  const fnRef = useRef(fn)
  fnRef.current = fn

  const reload = useCallback(async () => {
    const my = ++seq.current
    setLoading(true)
    try {
      const v = await fnRef.current()
      if (my === seq.current) {
        setData(v)
        setError('')
      }
    } catch (e) {
      if (my === seq.current) setError(errorText(e))
    } finally {
      if (my === seq.current) setLoading(false)
    }
  }, [])

  useEffect(() => {
    if (enabled) void reload()
  }, [...deps, enabled]) // eslint-disable-line react-hooks/exhaustive-deps

  return { data, error, loading, reload, setData }
}

// useInterval 以固定间隔调用 fn（delay 为 null 时暂停）。
export function useInterval(fn: () => void, delay: number | null) {
  const ref = useRef(fn)
  ref.current = fn
  useEffect(() => {
    if (delay === null) return
    const id = setInterval(() => ref.current(), delay)
    return () => clearInterval(id)
  }, [delay])
}
