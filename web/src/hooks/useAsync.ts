import { useCallback, useEffect, useRef, useState } from 'react'
import { errorText } from '../api/client'

export interface AsyncState<T> {
  data: T | undefined
  error: string
  loading: boolean
  /** 最近一次成功读取的时间；读取失败时保留旧数据并通过 error 标明。 */
  loadedAt: number | undefined
  reload: () => Promise<void>
  setData: (v: T | undefined) => void
}

// useAsync 在 deps 变化时执行 fn；reload 可手动重拉。
// 过期请求（deps 已变化、组件已卸载或被禁用）的结果一律丢弃；令牌变更后自动以新令牌重读，不重新挂载页面。
export function useAsync<T>(fn: () => Promise<T>, deps: unknown[], enabled = true): AsyncState<T> {
  const [data, setData] = useState<T>()
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(enabled)
  const [loadedAt, setLoadedAt] = useState<number>()
  const seq = useRef(0)
  const alive = useRef(true)
  const fnRef = useRef(fn)
  fnRef.current = fn
  const enabledRef = useRef(enabled)
  enabledRef.current = enabled

  const reload = useCallback(async () => {
    const my = ++seq.current
    setLoading(true)
    try {
      const v = await fnRef.current()
      if (alive.current && my === seq.current) {
        setData(v)
        setError('')
        setLoadedAt(Date.now())
      }
    } catch (e) {
      if (alive.current && my === seq.current) setError(errorText(e))
    } finally {
      if (alive.current && my === seq.current) setLoading(false)
    }
  }, [])

  useEffect(() => {
    alive.current = true
    return () => { alive.current = false }
  }, [])

  useEffect(() => {
    if (enabled) void reload()
    else { seq.current++; setLoading(false) }
  }, [...deps, enabled]) // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    const on = () => { if (enabledRef.current) void reload() }
    window.addEventListener('autobid:token-changed', on)
    return () => window.removeEventListener('autobid:token-changed', on)
  }, [reload])

  return { data, error, loading, loadedAt, reload, setData }
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

/** useElementWidth 用 ResizeObserver 测量元素的实际内容宽度（三栏写作、执行台按内容区宽度切换布局）。 */
export function useElementWidth<E extends HTMLElement>(): [(el: E | null) => void, number] {
  const [width, setWidth] = useState(0)
  const cleanup = useRef<(() => void) | null>(null)
  const ref = useCallback((el: E | null) => {
    cleanup.current?.()
    cleanup.current = null
    if (!el) return
    // 内容宽度 = clientWidth − 左右内边距；挂载时同步测量一次，之后由 ResizeObserver 与窗口 resize 更新。
    const measure = () => {
      const cs = getComputedStyle(el)
      setWidth(Math.round(el.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight)))
    }
    measure()
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    window.addEventListener('resize', measure)
    cleanup.current = () => { ro.disconnect(); window.removeEventListener('resize', measure) }
  }, [])
  return [ref, width]
}
