package server

// R2 可信身份：服务端会话鉴权、登录 / 登出 / 改口令接口与"当前操作人"。
//
// 身份只来自服务端核验过的会话 Cookie：请求头、请求体、查询参数都不能指定或改变"我是谁"，
// 旧的 X-Autobid-User 请求头不再有任何含义。运行时没有"信任请求头"或匿名模式的开关；
// 测试替身（见 *_test.go）通过未导出的 API.authn 字段注入，不进入任何配置。

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/identity"
)

// SessionCookie 是会话 Cookie 名。
const SessionCookie = "autobid_session"

// CSRFHeader 是改变状态的请求必须携带的防跨站请求伪造令牌头。
const CSRFHeader = "X-Autobid-CSRF"

// Identity 是一次请求的已核验身份。Login 是写入各 *_by 列的稳定账号标识。
type Identity struct {
	UserID      int64
	SessionID   int64
	Tenant      string
	Login       string
	DisplayName string
	Admin       bool
	ExpiresAt   time.Time
	csrf        string
}

type identityKey struct{}
type rolesKey struct{}

func withIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// identityFrom 返回请求上下文中的已核验身份；匿名请求（只有登录接口与静态页面）返回 nil。
func identityFrom(ctx context.Context) *Identity {
	id, _ := ctx.Value(identityKey{}).(*Identity)
	return id
}

// operator 返回当前登录账号的稳定标识（users.login）。处理函数把它作为 actor 传给存储层。
func operator(r *http.Request) string {
	if id := identityFrom(r.Context()); id != nil {
		return id.Login
	}
	return ""
}

// tenantOf 返回当前登录账号的租户（R2 只有 default 租户；跨租户隔离属 Phase 7）。
func tenantOf(r *http.Request) string {
	if id := identityFrom(r.Context()); id != nil && id.Tenant != "" {
		return id.Tenant
	}
	return db.DefaultTenant
}

// projectRoles 返回路由守卫为当前项目查到的角色（只在项目路由上有值）。
func projectRoles(r *http.Request) []string {
	roles, _ := r.Context().Value(rolesKey{}).([]string)
	return roles
}

func hasAnyRole(have []string, want ...string) bool {
	for _, h := range have {
		for _, w := range want {
			if h == w {
				return true
			}
		}
	}
	return false
}

// authenticator 从请求中核验身份。返回 (nil, nil) 表示匿名；错误为 *authError 时按其状态码与 code 响应。
type authenticator interface {
	authenticate(w http.ResponseWriter, r *http.Request) (*Identity, error)
}

type authError struct {
	status int
	code   string
	msg    string
}

func (e *authError) Error() string { return e.msg }

func writeAuthError(w http.ResponseWriter, err error) {
	var ae *authError
	if errors.As(err, &ae) {
		writeJSON(w, ae.status, map[string]string{"error": ae.msg, "code": ae.code})
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}

var errUnauthenticated = &authError{http.StatusUnauthorized, "unauthenticated", "请先登录"}

// sessionAuth 是生产唯一的鉴权实现：会话 Cookie → auth_sessions（只存令牌哈希）。
type sessionAuth struct {
	pg  *db.DB
	ttl db.SessionTTL
}

func (s *sessionAuth) authenticate(_ http.ResponseWriter, r *http.Request) (*Identity, error) {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	sess, err := s.pg.SessionByTokenHash(r.Context(), identity.HashToken(c.Value), s.ttl.Idle)
	if errors.Is(err, db.ErrSessionInvalid) {
		return nil, &authError{http.StatusUnauthorized, "session_expired", "会话已失效，请重新登录"}
	}
	if err != nil {
		return nil, err
	}
	if !safeMethod(r.Method) {
		if !sameOrigin(r) {
			return nil, &authError{http.StatusForbidden, "csrf_origin", "跨站请求被拒绝"}
		}
		if !identity.CheckCSRF(c.Value, r.Header.Get(CSRFHeader)) {
			return nil, &authError{http.StatusForbidden, "csrf_token", "缺少或无效的防跨站请求令牌，请刷新页面"}
		}
	}
	return &Identity{UserID: sess.UserID, SessionID: sess.SessionID, Tenant: sess.Tenant, Login: sess.Login,
		DisplayName: sess.DisplayName, Admin: sess.Admin, ExpiresAt: sess.ExpiresAt, csrf: identity.CSRFToken(c.Value)}, nil
}

func safeMethod(m string) bool { return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions }

// sameOrigin 在浏览器带了 Origin 头时要求它与 Host 一致（Cookie 已是 SameSite=Strict，此为纵深防御）。
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// publicAPIPath 是不需要会话的 /api 路径（只有登录本身）。
func publicAPIPath(r *http.Request) bool {
	return r.URL.Path == "/api/auth/login"
}

// auth 是全局鉴权中间件：/api 下除登录接口外全部要求有效会话；非 /api 路径（静态页面、/healthz）不需要。
// 逐路由的权限判定在 routeMux 的守卫里（permissions.go）。
func (a *API) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || publicAPIPath(r) {
			next.ServeHTTP(w, r)
			return
		}
		id, err := a.authn.authenticate(w, r)
		if err != nil {
			writeAuthError(w, err)
			return
		}
		if id == nil {
			writeAuthError(w, errUnauthenticated)
			return
		}
		next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
	})
}

// ---------------------------------------------------------------------------
// 登录限速（按来源地址，进程内；账号级锁定在 users.failed_logins / locked_until）
// ---------------------------------------------------------------------------

type loginLimiter struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	fails  map[string][]time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{window: 5 * time.Minute, max: 20, fails: map[string][]time.Time{}}
}

func (l *loginLimiter) prune(key string, now time.Time) []time.Time {
	list := l.fails[key]
	i := 0
	for i < len(list) && now.Sub(list[i]) > l.window {
		i++
	}
	list = list[i:]
	if len(list) == 0 {
		delete(l.fails, key)
	} else {
		l.fails[key] = list
	}
	return list
}

// blocked 返回该来源是否已超过失败上限，以及需等待的时间。
func (l *loginLimiter) blocked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	list := l.prune(key, now)
	if len(list) < l.max {
		return false, 0
	}
	return true, l.window - now.Sub(list[0])
}

func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.fails[key] = append(l.prune(key, now), now)
	if len(l.fails) > 10000 { // 防止伪造来源撑爆内存：整体清理一次
		for k := range l.fails {
			l.prune(k, now)
		}
	}
}

func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// 登录 / 登出 / 会话 / 改口令
// ---------------------------------------------------------------------------

type sessionView struct {
	User      sessionUser `json:"user"`
	CSRFToken string      `json:"csrf_token"`
	ExpiresAt time.Time   `json:"expires_at"`
}

type sessionUser struct {
	ID          int64  `json:"id"`
	Login       string `json:"login"`
	DisplayName string `json:"display_name"`
	Admin       bool   `json:"is_admin"`
}

// setSessionCookie 写会话 Cookie：HttpOnly、SameSite=Strict，HTTPS 时加 Secure；不设 Expires（浏览器关闭即丢弃），
// 有效期由服务端的 expires_at / absolute_expires_at 判定。
func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil, MaxAge: -1})
}

// issueSession 为已通过口令校验的账号创建会话并写 Cookie。
func (a *API) issueSession(w http.ResponseWriter, r *http.Request, u *db.User) (*sessionView, error) {
	token, hash, err := identity.NewSessionToken()
	if err != nil {
		return nil, err
	}
	sess, err := a.pg.CreateSession(r.Context(), u, hash, a.cfg.SessionTTL, clientAddr(r), r.UserAgent())
	if err != nil {
		return nil, err
	}
	setSessionCookie(w, r, token)
	return &sessionView{User: sessionUser{ID: u.ID, Login: u.Login, DisplayName: u.DisplayName, Admin: u.Admin},
		CSRFToken: identity.CSRFToken(token), ExpiresAt: sess.ExpiresAt}, nil
}

var errBadCredentials = &authError{http.StatusUnauthorized, "invalid_credentials", "账号或口令错误"}

func tooManyAttempts(w http.ResponseWriter, wait time.Duration) {
	secs := int(wait.Seconds()) + 1
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "登录尝试过多，请稍后再试", "code": "login_throttled", "retry_after_seconds": secs})
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeAuthError(w, &authError{http.StatusForbidden, "csrf_origin", "跨站请求被拒绝"})
		return
	}
	var req struct {
		Login    string `json:"login"`
		Password string `json:"password"`
	}
	if err := decodeBodyLimit(r, &req, 4096); err != nil {
		fail(w, err)
		return
	}
	addr := clientAddr(r)
	if blocked, wait := a.limiter.blocked(addr); blocked {
		tooManyAttempts(w, wait)
		return
	}
	cred, err := a.pg.UserCredentialByLogin(r.Context(), db.DefaultTenant, req.Login)
	if errors.Is(err, db.ErrNotFound) {
		identity.BurnVerify(req.Password)
		a.limiter.fail(addr)
		writeAuthError(w, errBadCredentials)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	if cred.LockedUntil != nil && cred.LockedUntil.After(time.Now()) {
		identity.BurnVerify(req.Password)
		tooManyAttempts(w, time.Until(*cred.LockedUntil))
		return
	}
	if identity.VerifyPassword(cred.PasswordHash, req.Password) != nil || cred.Status != "active" {
		a.limiter.fail(addr)
		if cred.Status == "active" {
			if _, err := a.pg.RecordLoginFailure(r.Context(), cred.ID, a.cfg.LoginFailures); err != nil {
				fail(w, err)
				return
			}
		}
		writeAuthError(w, errBadCredentials)
		return
	}
	if err := a.pg.ResetLoginFailures(r.Context(), cred.ID); err != nil {
		fail(w, err)
		return
	}
	view, err := a.issueSession(w, r, &cred.User)
	if errors.Is(err, db.ErrSessionInvalid) {
		writeAuthError(w, errBadCredentials)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r.Context())
	if id != nil && id.SessionID > 0 {
		if err := a.pg.RevokeSession(r.Context(), id.SessionID, "logout"); err != nil {
			fail(w, err)
			return
		}
	}
	clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) currentSession(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r.Context())
	writeJSON(w, http.StatusOK, sessionView{User: sessionUser{ID: id.UserID, Login: id.Login, DisplayName: id.DisplayName, Admin: id.Admin},
		CSRFToken: id.csrf, ExpiresAt: id.ExpiresAt})
}

// changeOwnPassword 由本人修改口令：校验当前口令，吊销该账号的全部会话，并为本次请求签发新会话。
func (a *API) changeOwnPassword(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r.Context())
	var req struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := decodeBodyLimit(r, &req, 4096); err != nil {
		fail(w, err)
		return
	}
	cred, err := a.pg.UserCredentialByLogin(r.Context(), id.Tenant, id.Login)
	if err != nil {
		fail(w, err)
		return
	}
	if identity.VerifyPassword(cred.PasswordHash, req.Current) != nil {
		writeAuthError(w, &authError{http.StatusForbidden, "invalid_credentials", "当前口令不正确"})
		return
	}
	if err := identity.CheckPassword(id.Login, req.New); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "口令须为 10~256 个字符，且不能与账号相同", "code": "weak_password"})
		return
	}
	hash, err := identity.HashPassword(req.New)
	if err != nil {
		fail(w, err)
		return
	}
	if err := a.pg.SetUserPassword(r.Context(), id.Tenant, id.UserID, hash, "password_changed", id.Login); err != nil {
		fail(w, err)
		return
	}
	view, err := a.issueSession(w, r, &cred.User)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
