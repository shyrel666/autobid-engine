import { useEffect, useMemo, useRef, useState } from 'react'
import { ArrowDown, ArrowUp, Check, Search, X } from 'lucide-react'
import type { ChainEntry, LLMProfile } from '../../api/types'
import { Badge, Button, Select, cx } from '../../components/ui'

export interface TaskLLMProfileChainProps {
  profiles: LLMProfile[]
  value: number[]
  onValueChange: (value: number[]) => void
  activeProfileId?: number
  onActiveProfileChange?: (value: number) => void
  chainEntries?: ChainEntry[] | null
  disabled?: boolean
  inputId?: string
}

function ProfileRoleBadge({ index, currentIndex, exhausted }: { index: number; currentIndex: number; exhausted?: boolean }) {
  if (exhausted) return <Badge tone="red">配额耗尽</Badge>
  if (index === currentIndex) return <Badge tone="blue">当前</Badge>
  if (index < currentIndex) {
    return (
      <span title="当前游标之前的配置不会被自动故障转移选中">
        <Badge tone="slate">已跳过</Badge>
      </span>
    )
  }
  return <Badge tone="amber">备用 {index - currentIndex}</Badge>
}

export function TaskLLMProfileChain({
  profiles,
  value,
  onValueChange,
  activeProfileId,
  onActiveProfileChange,
  chainEntries,
  disabled = false,
  inputId,
}: TaskLLMProfileChainProps) {
  const [query, setQuery] = useState('')
  const [open, setOpen] = useState(false)
  const boxRef = useRef<HTMLDivElement>(null)

  const profilesByID = useMemo(() => new Map(profiles.map((p) => [p.id, p])), [profiles])
  const statusByID = useMemo(() => {
    const m = new Map<number, string>()
    for (const c of chainEntries ?? []) m.set(c.profile_id, c.status)
    return m
  }, [chainEntries])

  const currentProfileID = activeProfileId && value.includes(activeProfileId) ? activeProfileId : value[0]
  const currentIndex = currentProfileID !== undefined ? value.indexOf(currentProfileID) : -1

  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => {
      if (boxRef.current && !boxRef.current.contains(e.target as Node)) setOpen(false)
    }
    window.addEventListener('mousedown', onDown)
    return () => window.removeEventListener('mousedown', onDown)
  }, [open])

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    if (!q) return profiles
    return profiles.filter((p) => `${p.name} ${p.model} ${p.id}`.toLowerCase().includes(q))
  }, [profiles, query])

  const toggleProfile = (id: number) => {
    if (disabled) return
    const exists = value.includes(id)
    const next = exists ? value.filter((x) => x !== id) : [...value, id]
    onValueChange(next)
    if (activeProfileId && !next.includes(activeProfileId)) {
      onActiveProfileChange?.(next[0] ?? 0)
    } else if (!activeProfileId && next.length === 1) {
      onActiveProfileChange?.(next[0])
    }
  }

  const move = (index: number, offset: -1 | 1) => {
    const target = index + offset
    if (target < 0 || target >= value.length) return
    const next = [...value]
    ;[next[index], next[target]] = [next[target], next[index]]
    onValueChange(next)
  }

  const remove = (id: number) => {
    const next = value.filter((pid) => pid !== id)
    onValueChange(next)
    if (activeProfileId === id) onActiveProfileChange?.(next[0] ?? 0)
  }

  return (
    <div className="flex flex-col gap-3">
      <div ref={boxRef} className="relative">
        <div
          onClick={() => !disabled && profiles.length > 0 && setOpen(true)}
          className={cx(
            'flex min-h-9 flex-wrap items-center gap-1.5 rounded-lg border border-line bg-panel px-2.5 py-1.5 text-sm transition-colors focus-within:border-fg',
            disabled && ' cursor-not-allowed opacity-60',
          )}
        >
          <Search className="size-3.5 shrink-0 text-muted" aria-hidden />
          {value.map((id) => {
            const p = profilesByID.get(id)
            return (
              <span key={id} className="inline-flex items-center gap-1 rounded-md bg-soft px-2 py-0.5 text-xs font-medium">
                {p?.name ?? `#${id}`}
                {!disabled && (
                  <button
                    type="button"
                    aria-label={`移除 ${p?.name ?? id}`}
                    onClick={(e) => { e.stopPropagation(); remove(id) }}
                    className="rounded hover:bg-line"
                  >
                    <X className="size-3 text-muted hover:text-fg" />
                  </button>
                )}
              </span>
            )
          })}
          <input
            id={inputId}
            type="text"
            value={query}
            disabled={disabled || profiles.length === 0}
            onFocus={() => setOpen(true)}
            onChange={(e) => { setQuery(e.target.value); setOpen(true) }}
            placeholder={profiles.length > 0 ? '搜索并添加 LLM 配置…' : '暂无可用 LLM 配置'}
            className="min-w-28 flex-1 bg-transparent text-xs outline-none placeholder:text-muted"
          />
        </div>

        {open && !disabled && (
          <div className="absolute z-40 mt-1 max-h-56 w-full overflow-y-auto rounded-[10px] border border-line bg-panel p-1 shadow-lg">
            {filtered.length === 0 ? (
              <div className="px-3 py-2 text-center text-xs text-muted">没有匹配的 LLM 配置</div>
            ) : (
              filtered.map((p) => {
                const selected = value.includes(p.id)
                const idx = value.indexOf(p.id)
                return (
                  <button
                    key={p.id}
                    type="button"
                    onClick={() => toggleProfile(p.id)}
                    className={cx(
                      'flex w-full items-center justify-between gap-2 rounded-lg px-2.5 py-1.5 text-left text-xs hover:bg-soft',
                      selected && 'bg-soft/70 font-medium',
                    )}
                  >
                    <div className="min-w-0 flex-1">
                      <div className="flex items-center gap-1.5 truncate">
                        <span className="truncate">{p.name}</span>
                        {p.is_default && <span className="text-[11px] text-muted">（默认激活）</span>}
                        {!p.enabled && <span className="text-[11px] text-warn">[已停用]</span>}
                      </div>
                      <div className="truncate font-mono text-[11px] text-muted">{p.model}</div>
                    </div>
                    <div className="flex shrink-0 items-center gap-1.5">
                      {idx >= 0 && <span className="rounded bg-panel px-1.5 py-0.5 text-[11px] text-muted tabular-nums">#{idx + 1}</span>}
                      <span className={cx('grid size-4 place-items-center rounded border', selected ? 'border-fg bg-fg text-panel' : 'border-line')}>
                        {selected && <Check className="size-3" />}
                      </span>
                    </div>
                  </button>
                )
              })
            )}
          </div>
        )}
      </div>

      {value.length === 0 ? (
        <p className="text-xs text-muted">未指定配置时，任务跟随 Agent 角色绑定或全局激活模型池。</p>
      ) : (
        <div className="flex flex-col divide-y divide-line rounded-lg border border-line bg-panel">
          {value.map((id, index) => {
            const profile = profilesByID.get(id)
            const exhausted = statusByID.get(id) === 'quota_exhausted'
            return (
              <div
                key={id}
                className="flex min-w-0 items-center gap-2 px-2.5 py-2"
              >
                <span className="w-5 shrink-0 text-center text-xs text-muted tabular-nums">{index + 1}</span>
                <div className="min-w-0 flex-1">
                  <p className="truncate text-sm font-medium">{profile?.name ?? `配置 #${id}`}</p>
                  <p className="truncate font-mono text-xs text-muted">{profile?.model ?? '配置已不可用'}</p>
                </div>
                <ProfileRoleBadge index={index} currentIndex={currentIndex} exhausted={exhausted} />
                <div className="flex shrink-0 items-center gap-0.5">
                  <Button
                    size="sm"
                    variant="ghost"
                    aria-label="上移配置"
                    onClick={() => move(index, -1)}
                    disabled={disabled || index === 0}
                  >
                    <ArrowUp className="size-3.5" />
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    aria-label="下移配置"
                    onClick={() => move(index, 1)}
                    disabled={disabled || index === value.length - 1}
                  >
                    <ArrowDown className="size-3.5" />
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    aria-label="移除配置"
                    onClick={() => remove(id)}
                    disabled={disabled}
                  >
                    <X className="size-3.5" />
                  </Button>
                </div>
              </div>
            )
          })}
        </div>
      )}

      {onActiveProfileChange && value.length > 0 && (
        <div className="flex flex-col items-stretch gap-2 sm:flex-row sm:items-center sm:justify-between sm:gap-3">
          <span className="text-xs font-medium text-muted">当前活动配置游标</span>
          <Select
            aria-label="当前活动配置游标"
            className="sm:w-56"
            value={currentProfileID ?? value[0]}
            disabled={disabled}
            onChange={(e) => onActiveProfileChange(Number(e.target.value))}
          >
            {value.map((id) => {
              const p = profilesByID.get(id)
              return (
                <option key={id} value={id}>
                  {p?.name ?? `配置 #${id}`}{p?.model ? `（${p.model}）` : ''}
                </option>
              )
            })}
          </Select>
        </div>
      )}
    </div>
  )
}
