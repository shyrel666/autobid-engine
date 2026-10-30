import { useEffect, useRef } from 'react'

/** useScrollTo 在数据就绪后把 #elementId 滚动到可视区域；同一目标只滚动一次，避免刷新时打断阅读。 */
export function useScrollTo(elementId: string | undefined, ready: boolean) {
  const done = useRef<string | undefined>(undefined)
  useEffect(() => {
    if (!elementId || !ready || done.current === elementId) return
    const el = document.getElementById(elementId)
    if (!el) return
    done.current = elementId
    el.scrollIntoView({ block: 'center', behavior: 'smooth' })
  })
}
