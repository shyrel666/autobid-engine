import { useEffect, useState } from 'react'
import { Monitor, Moon, Sun } from 'lucide-react'
import { IconButton } from '../components/ui'

export type ThemeMode = 'light' | 'dark' | 'system'

const THEME_KEY = 'autobid.theme'

function readStoredTheme(): ThemeMode {
  try {
    const v = localStorage.getItem(THEME_KEY)
    if (v === 'light' || v === 'dark' || v === 'system') return v
  } catch {
    // ignore
  }
  return 'light'
}

function applyTheme(mode: ThemeMode) {
  if (typeof document === 'undefined') return
  const prefersDark = typeof window !== 'undefined' && window.matchMedia?.('(prefers-color-scheme: dark)').matches
  const isDark = mode === 'dark' || (mode === 'system' && prefersDark)
  document.documentElement.classList.toggle('dark', isDark)
  document.documentElement.style.colorScheme = isDark ? 'dark' : 'light'
}

export function ThemeToggle() {
  const [mode, setMode] = useState<ThemeMode>(readStoredTheme)

  useEffect(() => {
    applyTheme(mode)
    if (mode !== 'system' || typeof window === 'undefined' || !window.matchMedia) return
    const mq = window.matchMedia('(prefers-color-scheme: dark)')
    const onChange = () => applyTheme('system')
    mq.addEventListener?.('change', onChange)
    return () => mq.removeEventListener?.('change', onChange)
  }, [mode])

  const cycle = () => {
    const next: ThemeMode = mode === 'light' ? 'dark' : mode === 'dark' ? 'system' : 'light'
    setMode(next)
    try {
      localStorage.setItem(THEME_KEY, next)
    } catch {
      // ignore
    }
    applyTheme(next)
  }

  const label = mode === 'light' ? '外观：浅色（点击切换深色）' : mode === 'dark' ? '外观：深色（点击切换跟随系统）' : '外观：跟随系统（点击切换浅色）'

  return (
    <IconButton label={label} onClick={cycle}>
      {mode === 'light' ? (
        <Sun className="size-4" strokeWidth={1.7} />
      ) : mode === 'dark' ? (
        <Moon className="size-4" strokeWidth={1.7} />
      ) : (
        <Monitor className="size-4" strokeWidth={1.7} />
      )}
    </IconButton>
  )
}
