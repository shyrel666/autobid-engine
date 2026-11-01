import { useEffect, useId, useRef, useState } from 'react'
import type { KeyboardEvent } from 'react'
import { Check, ChevronDown, RefreshCw } from 'lucide-react'
import { errorText } from '../../api/client'
import { Button, Field, Input } from '../../components/ui'

export function ModelPicker({ value, onChange, onRefresh, loading, disabled }: {
  value: string
  onChange: (value: string) => void
  onRefresh: () => Promise<string[]>
  loading: boolean
  disabled: boolean
}) {
  const id = useId()
  const input = useRef<HTMLInputElement>(null)
  const [models, setModels] = useState<string[]>()
  const [error, setError] = useState('')
  const [open, setOpen] = useState(false)
  const [filtering, setFiltering] = useState(false)
  const [active, setActive] = useState(-1)
  const expanded = open && !disabled
  const options = (models ?? []).filter((model) => !filtering || model.toLowerCase().includes(value.trim().toLowerCase()))
  const activeID = expanded && options[active] ? `${id}-option-${active}` : undefined

  useEffect(() => { if (expanded) input.current?.focus() }, [expanded])
  useEffect(() => { if (activeID) document.getElementById(activeID)?.scrollIntoView({ block: 'nearest' }) }, [activeID])

  const choose = (model: string) => {
    onChange(model)
    setOpen(false)
    setFiltering(false)
    setActive(-1)
    input.current?.focus()
  }
  const refresh = async () => {
    setOpen(false); setError(''); setModels(undefined); setActive(-1)
    try {
      const result = await onRefresh()
      setModels([...new Set(result)])
      setFiltering(false)
      setOpen(true)
    } catch (e) { setError(`列表获取失败：${errorText(e)}。仍可手动输入模型 ID。`) }
  }
  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.nativeEvent.isComposing) return
    if (e.key === 'Escape' && expanded) {
      e.preventDefault(); e.stopPropagation(); setOpen(false)
    } else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault()
      setOpen(true)
      setActive(options.length ? (!expanded || active < 0 ? (e.key === 'ArrowDown' ? 0 : options.length - 1) : (active + (e.key === 'ArrowDown' ? 1 : -1) + options.length) % options.length) : -1)
    } else if (e.key === 'Enter' && expanded) {
      e.preventDefault()
      if (options[active]) choose(options[active])
      else setOpen(false)
    } else if (e.key === 'Tab') setOpen(false)
  }

  return <Field label="模型">
    <div className="flex min-w-0 gap-2">
      <div className="relative min-w-0 flex-1" onBlur={(e) => { if (!e.currentTarget.contains(e.relatedTarget)) setOpen(false) }}>
        <Input ref={input} aria-label="模型" role="combobox" aria-autocomplete="list" aria-haspopup="listbox"
          aria-expanded={expanded} aria-controls={expanded ? `${id}-list` : undefined} aria-activedescendant={activeID}
          aria-describedby={`${id}-hint`} autoComplete="off" spellCheck={false} disabled={disabled}
          value={value} placeholder="输入或选择模型 ID" className="pr-9 font-mono"
          onChange={(e) => { onChange(e.target.value); setFiltering(true); setActive(-1); setOpen(true) }}
          onKeyDown={onKeyDown} />
        <button type="button" aria-label="展开模型列表" title="展开模型列表" disabled={disabled} tabIndex={-1}
          className="absolute inset-y-0 right-0 flex w-8 items-center justify-center rounded-r-[10px] text-muted hover:text-fg disabled:opacity-50"
          onMouseDown={(e) => e.preventDefault()} onClick={() => { setFiltering(false); setActive(-1); setOpen(!expanded) }}>
          <ChevronDown className={`size-4 transition-transform ${expanded ? 'rotate-180' : ''}`} aria-hidden />
        </button>
        {expanded && <div className="absolute inset-x-0 top-full z-20 mt-1 overflow-hidden rounded-[10px] border border-line bg-panel shadow-lg">
          <div role="listbox" id={`${id}-list`} aria-label="模型候选" className="max-h-56 overflow-y-auto overscroll-contain p-1">
            {options.map((model, index) => <div key={model} id={`${id}-option-${index}`} role="option" aria-selected={model === value}
              className={`flex cursor-pointer items-center gap-2 rounded-md px-2 py-2 text-sm ${index === active ? 'bg-soft' : 'hover:bg-soft'}`}
              onMouseDown={(e) => e.preventDefault()} onClick={() => choose(model)}>
              <span className="min-w-0 flex-1 break-all font-mono">{model}</span>
              {model === value && <Check className="size-4 shrink-0 text-muted" aria-hidden />}
            </div>)}
          </div>
          {!options.length && <p className="px-3 pb-3 pt-1 text-xs leading-relaxed text-muted">{models === undefined ? '点击「刷新列表」获取候选，也可直接输入模型 ID。' : !models.length ? '该端点未返回模型列表，可直接输入模型 ID。' : '没有匹配的模型，可直接使用输入的模型 ID。'}</p>}
        </div>}
      </div>
      <Button aria-label="刷新模型列表" loading={loading} disabled={disabled} icon={<RefreshCw className="size-4" />} onClick={() => void refresh()}>刷新列表</Button>
    </div>
    <p id={`${id}-hint`} role="status" className={`mt-1.5 text-xs leading-relaxed ${error ? 'text-danger' : 'text-muted'}`}>
      {error || (loading ? '正在获取模型列表…' : '可直接输入模型 ID，或刷新列表后搜索选择。')}
    </p>
  </Field>
}
