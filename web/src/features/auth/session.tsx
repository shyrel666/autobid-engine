import { createContext, useCallback, useContext, useEffect, useState } from 'react'
import type { FormEvent, ReactNode } from 'react'
import { ApiError, api, errorText } from '../../api/client'
import type { SessionInfo } from '../../api/types'
import { Button, Field, Input, Modal, Notice, Spinner } from '../../components/ui'
import { forgetProjects } from '../../lib/projectCache'

// 会话层（R2）：启动时读取 /api/auth/session；未登录时只显示登录页。
// 会话在使用中失效（过期、登出、改口令、账号停用）时不卸载页面，而是弹出重新登录：
// 正在编辑的章节草稿等组件状态保留，登录同一账号后继续保存。

export type SessionUser = SessionInfo['user']

interface SessionCtx {
  user: SessionUser
  logout: () => Promise<void>
  changePassword: () => void
}

const Ctx = createContext<SessionCtx | null>(null)

/** 当前登录账号；只能在 SessionGate 之内使用。 */
export function useSession(): SessionCtx {
  const c = useContext(Ctx)
  if (!c) throw new Error('useSession outside SessionGate')
  return c
}

/** 可选读取（组件也可能在视觉夹具里脱离会话渲染）。 */
export function useOptionalSession(): SessionCtx | null {
  return useContext(Ctx)
}

function LoginForm({ fixedLogin, onDone, submitLabel = '登录' }: { fixedLogin?: string; onDone: (s: SessionInfo) => void; submitLabel?: string }) {
  const [login, setLogin] = useState(fixedLogin ?? '')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setErr('')
    try {
      onDone(await api.login(login.trim(), password))
    } catch (x) {
      setErr(x instanceof ApiError && x.status === 401 ? '账号或口令错误' : errorText(x))
    } finally {
      setBusy(false)
      setPassword('')
    }
  }
  return (
    <form onSubmit={submit} className="flex flex-col gap-3" aria-label="登录">
      <Field label="账号">
        <Input name="login" autoComplete="username" value={login} disabled={!!fixedLogin} autoFocus={!fixedLogin}
          onChange={(e) => setLogin(e.target.value)} />
      </Field>
      <Field label="口令">
        <Input name="password" type="password" autoComplete="current-password" value={password} autoFocus={!!fixedLogin}
          onChange={(e) => setPassword(e.target.value)} />
      </Field>
      {err && <Notice tone="danger">{err}</Notice>}
      <Button variant="primary" type="submit" loading={busy} disabled={!login.trim() || !password}>{submitLabel}</Button>
    </form>
  )
}

function LoginScreen({ onDone }: { onDone: (s: SessionInfo) => void }) {
  return (
    <div className="grid min-h-dvh place-items-center bg-canvas p-4 text-fg">
      <div className="w-full max-w-sm rounded-[14px] border border-line bg-panel p-6 shadow-[0_1px_3px_#00000014]">
        <div className="mb-5 flex items-center gap-3">
          <img src="/autobid-logo.svg" width={28} height={28} className="size-7" alt="" />
          <h1 className="text-lg font-semibold">登录 AutoBid</h1>
        </div>
        <LoginForm onDone={onDone} />
        <p className="mt-4 text-xs text-muted">账号由管理员创建。忘记口令请联系本机管理员用 ops.bat 重置。</p>
      </div>
    </div>
  )
}

function ChangePasswordDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [cur, setCur] = useState('')
  const [next, setNext] = useState('')
  const [again, setAgain] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [done, setDone] = useState(false)
  useEffect(() => { if (open) { setCur(''); setNext(''); setAgain(''); setErr(''); setDone(false) } }, [open])
  const save = async () => {
    if (next !== again) { setErr('两次输入的新口令不一致'); return }
    setBusy(true)
    setErr('')
    try {
      await api.changePassword(cur, next)
      setDone(true)
    } catch (x) {
      setErr(x instanceof ApiError && x.code === 'weak_password' ? '新口令须为 10~256 个字符，且不能与账号相同'
        : x instanceof ApiError && x.code === 'invalid_credentials' ? '当前口令不正确' : errorText(x))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal open={open} title="修改口令" onClose={onClose}
      footer={done ? <Button variant="primary" onClick={onClose}>完成</Button> : <>
        <Button variant="ghost" onClick={onClose}>取消</Button>
        <Button variant="primary" loading={busy} disabled={!cur || !next || !again} onClick={() => void save()}>保存</Button>
      </>}>
      {done ? <Notice tone="ok">口令已修改。本账号在其他浏览器或设备上的登录已全部退出。</Notice> : (
        <div className="flex flex-col gap-3">
          <Field label="当前口令"><Input type="password" autoComplete="current-password" value={cur} onChange={(e) => setCur(e.target.value)} /></Field>
          <Field label="新口令" hint="至少 10 个字符；建议使用一句不常见的短语"><Input type="password" autoComplete="new-password" value={next} onChange={(e) => setNext(e.target.value)} /></Field>
          <Field label="再输入一次"><Input type="password" autoComplete="new-password" value={again} onChange={(e) => setAgain(e.target.value)} /></Field>
          {err && <Notice tone="danger">{err}</Notice>}
        </div>
      )}
    </Modal>
  )
}

/** SessionGate 决定显示登录页还是应用；会话失效时在应用之上弹出重新登录。 */
export function SessionGate({ children }: { children: ReactNode }) {
  const [state, setState] = useState<'loading' | 'anonymous' | 'ready' | 'unreachable'>('loading')
  const [user, setUser] = useState<SessionUser | null>(null)
  const [expired, setExpired] = useState(false)
  const [pwOpen, setPwOpen] = useState(false)

  const load = useCallback(async () => {
    try {
      const s = await api.session()
      setUser(s.user)
      setState('ready')
    } catch (e) {
      setState(e instanceof ApiError && e.status === 401 ? 'anonymous' : 'unreachable')
    }
  }, [])
  useEffect(() => { void load() }, [load])

  useEffect(() => {
    const on = () => setExpired(true)
    window.addEventListener('autobid:unauthorized', on)
    return () => window.removeEventListener('autobid:unauthorized', on)
  }, [])

  const logout = useCallback(async () => {
    try { await api.logout() } catch { /* 会话可能已失效 */ }
    forgetProjects()
    // 退出后整页回到登录页：不同账号之间不共享任何已加载的项目数据。
    window.location.assign('/tasks')
  }, [])

  if (state === 'loading') return <div className="grid min-h-dvh place-items-center"><Spinner label="正在读取登录状态" /></div>
  if (state === 'unreachable') {
    return (
      <div className="grid min-h-dvh place-items-center p-4">
        <Notice tone="danger" action={<Button onClick={() => { setState('loading'); void load() }}>重试</Button>}>无法连接服务，请确认 AutoBid 已启动。</Notice>
      </div>
    )
  }
  if (state === 'anonymous' || !user) {
    return <LoginScreen onDone={(s) => { forgetProjects(); setUser(s.user); setExpired(false); setState('ready') }} />
  }
  return (
    <Ctx.Provider value={{ user, logout, changePassword: () => setPwOpen(true) }}>
      {children}
      <Modal open={expired} title="登录已失效" onClose={() => {}}
        footer={<Button variant="ghost" onClick={() => void logout()}>改用其他账号（放弃未保存内容）</Button>}>
        <p className="mb-3 text-sm text-muted">会话已过期或已在别处退出。重新登录 {user.display_name}（{user.login}）后，页面上未保存的编辑内容仍然保留。</p>
        <LoginForm fixedLogin={user.login} submitLabel="重新登录" onDone={(s) => {
          setUser(s.user)
          setExpired(false)
          // 已挂载的页面用新会话重读数据；组件状态（含未保存的草稿）不变。
          window.dispatchEvent(new Event('autobid:token-changed'))
        }} />
      </Modal>
      <ChangePasswordDialog open={pwOpen} onClose={() => setPwOpen(false)} />
    </Ctx.Provider>
  )
}
