import { createContext, useCallback, useContext, useEffect, useId, useRef, useState } from 'react'
import type {
  ButtonHTMLAttributes, InputHTMLAttributes, KeyboardEvent as ReactKeyboardEvent, ReactNode, RefObject, SelectHTMLAttributes, TextareaHTMLAttributes,
} from 'react'
import { AlertCircle, AlertTriangle, CheckCircle2, Info, Loader2, X } from 'lucide-react'

// 公共组件：尺寸、颜色与状态样式按 FRONTEND_REFACTOR_PLAN.md 第 3.2 节冻结（按钮 32px / 紧凑 28px、
// 输入框 32px、圆角 10/8px、Badge 12px 带圆点、卡片 14px 圆角）。旧页面依赖的导出签名保持不变。

export function cx(...cls: (string | false | null | undefined)[]) {
  return cls.filter(Boolean).join(' ')
}

// 基础控件的默认尺寸类可被调用方覆盖：调用方给出同组工具类（宽、高、内边距、字号、圆角）时丢弃默认值，
// 避免两条冲突的 Tailwind 类按样式表顺序而非书写顺序生效。
const textSizes = /^text-(xs|sm|base|lg|xl|\d?xl|\[\d+px\])$/
function group(token: string): string {
  const t = token.replace(/^[a-z-]+:/, (m) => (m.startsWith('hover') || m.startsWith('focus') || m.startsWith('disabled') ? m : ''))
  if (textSizes.test(t)) return 'text-size'
  const m = /^(w|h|min-w|px|py|pl|pr|rounded)(-|$)/.exec(t)
  return m ? m[1] : ''
}
export function mergeCls(base: string, extra?: string) {
  if (!extra) return base
  const taken = new Set(extra.split(/\s+/).map(group).filter(Boolean))
  return cx(base.split(/\s+/).filter((t) => !taken.has(group(t))).join(' '), extra)
}

type Variant = 'primary' | 'secondary' | 'danger' | 'ghost' | 'success'

const variants: Record<Variant, string> = {
  primary: 'border border-primary bg-primary text-[#fafafa] hover:enabled:bg-primary-hover hover:enabled:border-primary-hover',
  secondary: 'border border-line bg-panel text-fg hover:enabled:bg-soft',
  danger: 'border border-danger bg-danger text-white hover:enabled:bg-[#b91c1c]',
  success: 'border border-ok bg-ok text-white hover:enabled:bg-[#047857]',
  ghost: 'border border-transparent text-fg hover:enabled:bg-soft',
}

export function Button({ variant = 'secondary', size = 'md', loading, icon, children, className, disabled, ...rest }:
  ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant; size?: 'sm' | 'md'; loading?: boolean; icon?: ReactNode }) {
  return (
    <button
      type="button"
      {...rest}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
      className={mergeCls(cx('relative inline-flex shrink-0 items-center justify-center gap-1.5 whitespace-nowrap font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-50',
        size === 'sm' ? 'h-7 rounded-lg px-2 text-[13px]' : 'h-8 rounded-[10px] px-2.5 text-sm', variants[variant]), className)}
    >
      {/* loading 时保留原内容占位，按钮宽度不变 */}
      <span className={cx('inline-flex items-center gap-1.5', loading && 'invisible')}>{icon}{children}</span>
      {loading && <Loader2 className="absolute size-4 animate-spin" aria-hidden />}
    </button>
  )
}

export function IconButton({ label, children, className, ...rest }: ButtonHTMLAttributes<HTMLButtonElement> & { label: string }) {
  return (
    <button type="button" aria-label={label} title={label} {...rest}
      className={cx('grid size-8 shrink-0 place-items-center rounded-[10px] border border-transparent text-fg hover:enabled:bg-soft disabled:opacity-50', className)}>
      {children}
    </button>
  )
}

export function Card({ title, actions, children, className, bodyClassName, subtitle }:
  { title?: ReactNode; actions?: ReactNode; children: ReactNode; className?: string; bodyClassName?: string; subtitle?: ReactNode }) {
  return (
    <section className={cx('min-w-0 rounded-[14px] border border-line bg-panel', className)}>
      {(title || actions) && (
        <header className="flex min-h-12 flex-wrap items-center justify-between gap-2 border-b border-line px-4 py-2.5">
          <div className="min-w-0">
            <h3 className="flex flex-wrap items-center gap-2 text-sm font-semibold text-fg">{title}</h3>
            {subtitle && <p className="text-xs text-muted">{subtitle}</p>}
          </div>
          <div className="flex flex-wrap items-center gap-2">{actions}</div>
        </header>
      )}
      <div className={cx('p-4', bodyClassName)}>{children}</div>
    </section>
  )
}

const tones = {
  slate: 'border-line bg-soft text-muted',
  blue: 'border-[#bfdbfe] bg-[#eff6ff] text-link',
  green: 'border-[#a7f3d0] bg-[#ecfdf5] text-ok',
  amber: 'border-[#fde68a] bg-[#fffbeb] text-warn',
  red: 'border-[#fecaca] bg-[#fef2f2] text-danger',
  purple: 'border-[#ddd6fe] bg-[#f5f3ff] text-[#6d28d9]',
}
export type Tone = keyof typeof tones

export function Badge({ tone = 'slate', children, className, dot, title }: { tone?: Tone; children: ReactNode; className?: string; dot?: boolean; title?: string }) {
  return (
    <span title={title} className={cx('inline-flex w-max items-center gap-1.5 rounded-[7px] border px-2 py-0.5 text-xs leading-4 font-normal whitespace-nowrap', tones[tone], className)}>
      {dot && <span className="size-1.5 shrink-0 rounded-full bg-current" aria-hidden />}
      {children}
    </span>
  )
}

export function statusTone(s: string): Tone {
  switch (s) {
    case 'running': case 'open': return 'blue'
    case 'done': case 'confirmed': case 'allowed': case 'allow': return 'green'
    case 'paused': case 'pending': case 'blocked': return 'amber'
    case 'failed': case 'timeout': case 'denied': case 'deny': case 'cancelled': case 'dismissed': return 'red'
    default: return 'slate'
  }
}

export function severityTone(s: string): Tone {
  return s === 'critical' || s === 'high' ? 'red' : s === 'medium' ? 'amber' : s === 'low' ? 'blue' : 'slate'
}

export function Spinner({ label }: { label?: string }) {
  return (
    <div className="flex items-center justify-center gap-2 py-6 text-sm text-muted" role="status">
      <Loader2 className="size-4 animate-spin" aria-hidden /> {label ?? '加载中…'}
    </div>
  )
}

export function ErrorBanner({ error, onRetry }: { error?: string; onRetry?: () => void }) {
  if (!error) return null
  return (
    <div role="alert" className="flex items-start gap-2 rounded-[10px] border border-[#fecaca] bg-[#fef2f2] px-3 py-2 text-sm text-danger">
      <AlertCircle className="mt-0.5 size-4 shrink-0" aria-hidden />
      <span className="flex-1 break-all">{error}</span>
      {onRetry && <Button size="sm" variant="ghost" onClick={onRetry}>重试</Button>}
    </div>
  )
}

/** Notice 页面内提示条：warn / danger / info 同时带图标与文字，不只靠颜色区分。 */
export function Notice({ tone = 'warn', children, action, className }: { tone?: 'warn' | 'danger' | 'info' | 'ok'; children: ReactNode; action?: ReactNode; className?: string }) {
  const cls = {
    warn: 'border-[#fde68a] bg-[#fffbeb] text-[#92400e]',
    danger: 'border-[#fecaca] bg-[#fef2f2] text-[#991b1b]',
    info: 'border-line bg-soft text-fg',
    ok: 'border-[#a7f3d0] bg-[#ecfdf5] text-[#065f46]',
  }[tone]
  const Icon = tone === 'danger' ? AlertCircle : tone === 'warn' ? AlertTriangle : tone === 'ok' ? CheckCircle2 : Info
  return (
    <div className={cx('flex items-start gap-2 rounded-[10px] border px-3 py-2 text-sm', cls, className)}>
      <Icon className="mt-0.5 size-4 shrink-0" aria-hidden />
      <div className="min-w-0 flex-1">{children}</div>
      {action}
    </div>
  )
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="py-8 text-center text-sm text-muted">{children}</div>
}

export function Stat({ label, value, hint, tone }: { label: string; value: ReactNode; hint?: ReactNode; tone?: 'green' | 'amber' | 'red' }) {
  return (
    <div className="min-w-0 rounded-[10px] border border-line bg-panel px-4 py-3">
      <div className="text-xs text-muted">{label}</div>
      <div className={cx('mt-1 text-2xl font-semibold tabular-nums', tone === 'green' ? 'text-ok' : tone === 'amber' ? 'text-warn' : tone === 'red' ? 'text-danger' : 'text-fg')}>{value}</div>
      {hint && <div className="mt-0.5 text-xs text-muted">{hint}</div>}
    </div>
  )
}

export function Field({ label, hint, children, className }: { label: ReactNode; hint?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <label className={cx('flex min-w-0 flex-col gap-1.5 text-sm', className)}>
      <span className="text-fg">{label}</span>
      {children}
      {hint && <span className="text-xs text-muted">{hint}</span>}
    </label>
  )
}

const inputCls = 'w-full min-w-0 rounded-[10px] border border-line bg-panel px-2.5 text-sm text-fg placeholder:text-muted focus-visible:border-[#a3a3a3] disabled:bg-soft disabled:opacity-70'

export function Input(props: InputHTMLAttributes<HTMLInputElement>) {
  return <input {...props} className={mergeCls(`${inputCls} h-8`, props.className)} />
}

export function TextArea(props: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea {...props} className={mergeCls(`${inputCls} py-1.5 leading-5`, props.className)} />
}

export function Select(props: SelectHTMLAttributes<HTMLSelectElement>) {
  return <select {...props} className={mergeCls(`${inputCls} h-8 pr-7`, props.className)} />
}

// ---------------------------------------------------------------------------
// 焦点约束：对话框与抽屉打开时聚焦首个可聚焦元素，Tab 在内部循环，关闭后恢复原焦点。
// ---------------------------------------------------------------------------

// 对话框栈：嵌套的对话框 / 抽屉中只有最上层响应 Esc。
const dialogStack: symbol[] = []
function useDialogLayer(open: boolean) {
  const id = useRef(Symbol('dialog')).current
  useEffect(() => {
    if (!open) return
    dialogStack.push(id)
    return () => { const i = dialogStack.lastIndexOf(id); if (i >= 0) dialogStack.splice(i, 1) }
  }, [open, id])
  return () => dialogStack[dialogStack.length - 1] === id
}

const FOCUSABLE = 'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])'

export function useFocusTrap(ref: RefObject<HTMLElement | null>, open: boolean) {
  useEffect(() => {
    if (!open) return
    const prev = document.activeElement as HTMLElement | null
    const el = ref.current
    if (el && !el.contains(document.activeElement)) {
      const auto = el.querySelector<HTMLElement>('[autofocus],[data-autofocus]')
      const first = auto ?? el.querySelector<HTMLElement>(FOCUSABLE)
      ;(first ?? el).focus()
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Tab' || !ref.current) return
      const items = [...ref.current.querySelectorAll<HTMLElement>(FOCUSABLE)].filter((x) => x.offsetParent !== null)
      if (items.length === 0) return
      const first = items[0], last = items[items.length - 1]
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus() }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus() }
    }
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('keydown', onKey)
      if (prev && document.contains(prev)) prev.focus()
    }
  }, [open, ref])
}

// dirty 为 true 时，Esc、点击遮罩或关闭按钮会先确认，避免误丢表单内容。
export function Modal({ open, title, onClose, children, footer, wide, dirty }:
  { open: boolean; title: ReactNode; onClose: () => void; children: ReactNode; footer?: ReactNode; wide?: boolean; dirty?: boolean }) {
  const ref = useRef<HTMLDivElement>(null)
  const titleID = useId()
  const confirm = useConfirm()
  useFocusTrap(ref, open)
  const isTop = useDialogLayer(open)
  const tryClose = useCallback(async () => {
    if (dirty && !(await confirm({ title: '放弃未保存的修改？', message: '关闭后本次填写的内容不会保存。', confirmLabel: '放弃修改', danger: true }))) return
    onClose()
  }, [dirty, onClose, confirm])
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && isTop()) { e.stopPropagation(); void tryClose() }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, tryClose, isTop])
  if (!open) return null
  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-[#17171766] p-4 pt-16" onMouseDown={() => void tryClose()}>
      <div ref={ref} role="dialog" aria-modal="true" aria-labelledby={titleID} tabIndex={-1}
        className={cx('w-full rounded-[14px] border border-line bg-panel shadow-xl outline-none', wide ? 'max-w-4xl' : 'max-w-lg')} onMouseDown={(e) => e.stopPropagation()}>
        <div className="flex items-center justify-between gap-2 border-b border-line px-4 py-3">
          <h2 id={titleID} className="flex min-w-0 flex-wrap items-center gap-2 text-base font-semibold text-fg">{title}</h2>
          <IconButton label="关闭" onClick={() => void tryClose()}><X className="size-4" /></IconButton>
        </div>
        <div className="max-h-[70vh] overflow-y-auto p-4">{children}</div>
        {footer && <div className="flex flex-wrap justify-end gap-2 border-t border-line px-4 py-3">{footer}</div>}
      </div>
    </div>
  )
}

/** Drawer 侧边抽屉（证据详情、窄屏目录/工作列表）。side=left|right，宽度上限为视口宽度减 32px。 */
export function Drawer({ open, title, onClose, children, side = 'right', width = 400, footer }:
  { open: boolean; title: ReactNode; onClose: () => void; children: ReactNode; side?: 'left' | 'right'; width?: number; footer?: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null)
  const titleID = useId()
  useFocusTrap(ref, open)
  const isTop = useDialogLayer(open)
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape' && isTop()) { e.stopPropagation(); onClose() } }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onClose, isTop])
  if (!open) return null
  return (
    <div className="fixed inset-0 z-40 bg-[#17171740]" onMouseDown={onClose}>
      <div ref={ref} role="dialog" aria-modal="true" aria-labelledby={titleID} tabIndex={-1} onMouseDown={(e) => e.stopPropagation()}
        style={{ width: `min(${width}px, calc(100vw - 32px))` }}
        className={cx('absolute top-0 bottom-0 flex flex-col bg-panel shadow-xl outline-none', side === 'right' ? 'right-0 border-l border-line' : 'left-0 border-r border-line')}>
        <div className="flex min-h-12 items-center justify-between gap-2 border-b border-line px-4 py-2">
          <h2 id={titleID} className="min-w-0 truncate text-sm font-semibold">{title}</h2>
          <IconButton label="关闭" onClick={onClose}><X className="size-4" /></IconButton>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto">{children}</div>
        {footer && <div className="flex flex-wrap justify-end gap-2 border-t border-line px-4 py-3">{footer}</div>}
      </div>
    </div>
  )
}

/** 同页切换组件使用 Tabs 语义；页间导航请用链接（见 layout/TaskHeader）。 */
export function Tabs<T extends string>({ tabs, value, onChange, className }: { tabs: { key: T; label: ReactNode }[]; value: T; onChange: (k: T) => void; className?: string }) {
  return (
    <div role="tablist" className={cx('scrollbar-none flex gap-4 overflow-x-auto border-b border-line', className)}>
      {tabs.map((t) => (
        <button key={t.key} role="tab" aria-selected={value === t.key} onClick={() => onChange(t.key)}
          className={cx('-mb-px flex h-8 items-center gap-1.5 whitespace-nowrap border-b-2 text-xs font-medium',
            value === t.key ? 'border-fg text-fg' : 'border-transparent text-muted hover:text-fg')}>
          {t.label}
        </button>
      ))}
    </div>
  )
}

export function Pre({ children, className }: { children: ReactNode; className?: string }) {
  return <pre className={cx('max-h-80 overflow-auto rounded-lg border border-line bg-panel p-3 font-mono text-xs leading-[18px] text-fg whitespace-pre-wrap [overflow-wrap:anywhere]', className)}>{children}</pre>
}

/** Menu 「更多操作」弹出菜单：Esc 与点击外部关闭，菜单项为 role=menuitem 按钮。 */
export function Menu({ label, trigger, items, align = 'right' }:
  { label: string; trigger: ReactNode; align?: 'left' | 'right'
    items: ({ key: string; label: ReactNode; onSelect: () => void; danger?: boolean; disabled?: boolean } | false | null | undefined)[] }) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)
  const menuID = useId()
  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => { if (!ref.current?.contains(e.target as Node)) setOpen(false) }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false) }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    ref.current?.querySelector<HTMLElement>('[role=menuitem]:not([disabled])')?.focus()
    return () => { document.removeEventListener('mousedown', onDown); document.removeEventListener('keydown', onKey) }
  }, [open])
  const list = items.filter(Boolean) as { key: string; label: ReactNode; onSelect: () => void; danger?: boolean; disabled?: boolean }[]
  const onMenuKey = (e: ReactKeyboardEvent) => {
    if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return
    e.preventDefault()
    const els = [...(ref.current?.querySelectorAll<HTMLElement>('[role=menuitem]:not([disabled])') ?? [])]
    const i = els.indexOf(document.activeElement as HTMLElement)
    els[(i + (e.key === 'ArrowDown' ? 1 : -1) + els.length) % els.length]?.focus()
  }
  return (
    <div ref={ref} className="relative inline-flex">
      <button type="button" aria-label={label} title={label} aria-haspopup="menu" aria-expanded={open} aria-controls={open ? menuID : undefined}
        onClick={() => setOpen((v) => !v)} className="inline-flex">{trigger}</button>
      {open && (
        <div id={menuID} role="menu" onKeyDown={onMenuKey}
          className={cx('absolute top-full z-30 mt-1 min-w-40 rounded-[10px] border border-line bg-panel p-1 shadow-lg', align === 'right' ? 'right-0' : 'left-0')}>
          {list.map((it) => (
            <button key={it.key} role="menuitem" type="button" disabled={it.disabled}
              onClick={() => { setOpen(false); it.onSelect() }}
              className={cx('flex h-8 w-full items-center gap-2 rounded-lg px-2 text-left text-sm whitespace-nowrap hover:enabled:bg-soft focus-visible:bg-soft disabled:opacity-50',
                it.danger ? 'text-danger' : 'text-fg')}>
              {it.label}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------
// 确认对话框：替代 window.confirm，展示对象名称与影响；返回 Promise<boolean>。
// ---------------------------------------------------------------------------

interface ConfirmOpts { title: ReactNode; message?: ReactNode; confirmLabel?: string; cancelLabel?: string; danger?: boolean }
const ConfirmCtx = createContext<(o: ConfirmOpts) => Promise<boolean>>(async () => true)

export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<(ConfirmOpts & { resolve: (v: boolean) => void }) | null>(null)
  const ref = useRef<HTMLDivElement>(null)
  const titleID = useId()
  useFocusTrap(ref, !!state)
  const isTop = useDialogLayer(!!state)
  const ask = useCallback((o: ConfirmOpts) => new Promise<boolean>((resolve) => setState({ ...o, resolve })), [])
  const done = (v: boolean) => { state?.resolve(v); setState(null) }
  useEffect(() => {
    if (!state) return
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape' && isTop()) { e.stopPropagation(); state.resolve(false); setState(null) } }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [state, isTop])
  return (
    <ConfirmCtx.Provider value={ask}>
      {children}
      {state && (
        <div className="fixed inset-0 z-[70] flex items-start justify-center bg-[#17171766] p-4 pt-24" onMouseDown={() => done(false)}>
          <div ref={ref} role="alertdialog" aria-modal="true" aria-labelledby={titleID} tabIndex={-1}
            className="w-full max-w-md rounded-[14px] border border-line bg-panel shadow-xl outline-none" onMouseDown={(e) => e.stopPropagation()}>
            <div className="p-4">
              <h2 id={titleID} className="text-base font-semibold">{state.title}</h2>
              {state.message && <div className="mt-2 text-sm text-muted">{state.message}</div>}
            </div>
            <div className="flex justify-end gap-2 border-t border-line px-4 py-3">
              <Button data-autofocus onClick={() => done(false)}>{state.cancelLabel ?? '取消'}</Button>
              <Button variant={state.danger ? 'danger' : 'primary'} onClick={() => done(true)}>{state.confirmLabel ?? '确定'}</Button>
            </div>
          </div>
        </div>
      )}
    </ConfirmCtx.Provider>
  )
}

export function useConfirm() {
  return useContext(ConfirmCtx)
}

// ---------------------------------------------------------------------------
// Toast
// ---------------------------------------------------------------------------

interface Toast { id: number; kind: 'ok' | 'error' | 'info'; text: string }
const ToastCtx = createContext<(kind: Toast['kind'], text: string) => void>(() => {})

export function ToastProvider({ children }: { children: ReactNode }) {
  const [list, setList] = useState<Toast[]>([])
  const push = useCallback((kind: Toast['kind'], text: string) => {
    const id = Date.now() + Math.random()
    setList((l) => [...l, { id, kind, text }])
    setTimeout(() => setList((l) => l.filter((t) => t.id !== id)), kind === 'error' ? 7000 : 3500)
  }, [])
  return (
    <ToastCtx.Provider value={push}>
      {children}
      <div className="pointer-events-none fixed right-4 bottom-4 z-[80] flex w-80 max-w-[calc(100vw-32px)] flex-col gap-2" role="status" aria-live="polite">
        {list.map((t) => (
          <div key={t.id} className={cx('pointer-events-auto flex items-start gap-2 rounded-[10px] border px-3 py-2 text-sm shadow-lg',
            t.kind === 'error' ? 'border-[#fecaca] bg-[#fef2f2] text-danger' : t.kind === 'ok' ? 'border-[#a7f3d0] bg-[#ecfdf5] text-ok' : 'border-line bg-panel text-fg')}>
            {t.kind === 'error' ? <AlertCircle className="mt-0.5 size-4 shrink-0" /> : t.kind === 'ok' ? <CheckCircle2 className="mt-0.5 size-4 shrink-0" /> : <Info className="mt-0.5 size-4 shrink-0" />}
            <span className="break-all">{t.text}</span>
          </div>
        ))}
      </div>
    </ToastCtx.Provider>
  )
}

export function useToast() {
  return useContext(ToastCtx)
}
