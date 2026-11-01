import { useEffect, useState } from 'react'
import { ArrowDown, ArrowUp, ArrowUpDown } from 'lucide-react'
import { cx } from '../components/ui'

export type SortDirection = 'asc' | 'desc'

export interface SortSpec<Field extends string> {
  field: Field
  direction: SortDirection
}

export function nextSortSpec<Field extends string>(
  current: SortSpec<Field>,
  field: Field,
  defaultDirection: SortDirection = 'desc',
): SortSpec<Field> {
  if (current.field === field) {
    return {
      field,
      direction: current.direction === 'asc' ? 'desc' : 'asc',
    }
  }
  return {
    field,
    direction: defaultDirection,
  }
}

export function useStoredSortPreference<Field extends string>({
  storageKey,
  defaultSort,
  allowedFields,
}: {
  storageKey: string
  defaultSort: SortSpec<Field>
  allowedFields: readonly Field[]
}) {
  const [sort, setSort] = useState<SortSpec<Field>>(() => {
    try {
      const raw = window.localStorage.getItem(storageKey)
      if (!raw) return defaultSort
      const parsed = JSON.parse(raw) as Partial<SortSpec<Field>>
      if (!parsed.field || !allowedFields.includes(parsed.field)) return defaultSort
      if (parsed.direction !== 'asc' && parsed.direction !== 'desc') return defaultSort
      return { field: parsed.field, direction: parsed.direction }
    } catch {
      return defaultSort
    }
  })

  useEffect(() => {
    try {
      window.localStorage.setItem(storageKey, JSON.stringify(sort))
    } catch {
      // ignore storage errors
    }
  }, [storageKey, sort])

  return [sort, setSort] as const
}

export function SortableHeaderButton<Field extends string>({
  field,
  sort,
  onToggle,
  label,
  className,
}: {
  field: Field
  sort: SortSpec<Field>
  onToggle: (field: Field) => void
  label: string
  className?: string
}) {
  const active = sort.field === field
  return (
    <button
      type="button"
      onClick={() => onToggle(field)}
      className={cx(
        'inline-flex items-center gap-1 rounded px-1 py-0.5 text-left font-medium transition-colors hover:bg-slate-200/60 hover:text-fg',
        active ? 'text-fg' : 'text-slate-500',
        className,
      )}
    >
      <span>{label}</span>
      {active ? (
        sort.direction === 'asc' ? <ArrowUp className="size-3.5 text-link" /> : <ArrowDown className="size-3.5 text-link" />
      ) : (
        <ArrowUpDown className="size-3 text-slate-400" />
      )}
    </button>
  )
}
