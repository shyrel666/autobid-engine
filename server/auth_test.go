package server

// R2 可信身份与项目角色：以生产的 sessionAuth（真实登录、Cookie、CSRF）在真实 PostgreSQL 上验证验收性质。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
	"github.com/autobid/autobid-engine/internal/identity"
)

var r2Seq atomic.Int64

// r2Login 返回本次运行唯一的登录名（账号表跨测试运行共享 default 租户）。
func r2Login(name string) string {
	return fmt.Sprintf("%s-%s%d", name, strconv.FormatInt(time.Now().UnixNano()%1e10, 36), r2Seq.Add(1))
}

const r2Password = "correct horse battery staple"

type r2Env struct {
	t   *testing.T
	pg  *db.DB
	api *API
	url string
}

// newSessionAPI 用生产鉴权（不替换 authn）启动 API。
func newSessionAPI(t *testing.T, cfg APIConfig) *r2Env {
	t.Helper()
	pg := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(pg)
	e := NewEngine(m, func(*Task) (*agent.Planner, *agent.Worker) { return nil, nil },
		Config{Workers: 1, Debounce: 20 * time.Millisecond, WorkerIdlePoll: 20 * time.Millisecond})
	api := NewAPI(ctx, m, e, NewLLMService(pg, LLMServiceConfig{}), nil, NewHub(), cfg)
	if _, ok := api.authn.(*sessionAuth); !ok {
		t.Fatal("NewAPI must use the production session authenticator")
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		srv.Close()
		e.StopAll()
		cancel()
	})
	return &r2Env{t: t, pg: pg, api: api, url: srv.URL}
}

func (env *r2Env) user(name string, admin bool) string {
	env.t.Helper()
	login := r2Login(name)
	hash, err := identity.HashPassword(r2Password)
	if err != nil {
		env.t.Fatal(err)
	}
	if _, err := env.pg.CreateUser(context.Background(), db.DefaultTenant, db.NewUser{Login: login, DisplayName: name, PasswordHash: hash, Admin: admin}, "ops"); err != nil {
		env.t.Fatal(err)
	}
	return login
}

// sessClient 是一个已登录的浏览器式客户端：Cookie 罐 + CSRF 请求头。
type sessClient struct {
	t     *testing.T
	base  string
	hc    *http.Client
	csrf  string
	login string
}

type r2Resp struct {
	Error         string   `json:"error"`
	Code          string   `json:"code"`
	RequiredRoles []string `json:"required_roles"`
}

func newJar(t *testing.T) *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 30 * time.Second}
}

func (env *r2Env) loginAs(login string) *sessClient {
	env.t.Helper()
	c := &sessClient{t: env.t, base: env.url, hc: newJar(env.t), login: login}
	var out sessionView
	if code, raw := c.send("POST", "/api/auth/login", map[string]string{"login": login, "password": r2Password}, nil, &out); code != 200 {
		env.t.Fatalf("login %s: %d %s", login, code, raw)
	}
	if out.User.Login != login || out.CSRFToken == "" {
		env.t.Fatalf("login view: %+v", out)
	}
	c.csrf = out.CSRFToken
	return c
}

// send 发送请求；非 GET 自动带 CSRF 头（headers 可覆盖或追加）。返回状态码与原始响应体。
func (c *sessClient) send(method, path string, body any, headers map[string]string, out any) (int, string) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != "GET" && c.csrf != "" {
		req.Header.Set(CSRFHeader, c.csrf)
	}
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, out)
	}
	return resp.StatusCode, string(raw)
}

func (c *sessClient) do(method, path string, body any, out any) int {
	c.t.Helper()
	code, _ := c.send(method, path, body, nil, out)
	return code
}

func (c *sessClient) expect(method, path string, body any, status int, code string) {
	c.t.Helper()
	var r r2Resp
	got, raw := c.send(method, path, body, nil, &r)
	if got != status || (code != "" && r.Code != code) {
		c.t.Fatalf("%s %s as %s: %d %s, want %d %q", method, path, c.login, got, raw, status, code)
	}
}

func (c *sessClient) createTask(name string) int64 {
	c.t.Helper()
	var task struct {
		ID      int64    `json:"id"`
		MyRoles []string `json:"my_roles"`
	}
	if code, raw := c.send("POST", "/api/tasks", map[string]any{"name": name, "goal": "R2 验收", "legacy_writing": true}, nil, &task); code != 201 {
		c.t.Fatalf("create task: %d %s", code, raw)
	}
	if len(task.MyRoles) != 1 || task.MyRoles[0] != db.RoleBidManager {
		c.t.Fatalf("creator roles: %v", task.MyRoles)
	}
	return task.ID
}

// 性质 1 / 2 / 3 / 7：身份只来自服务端会话；没有匿名模式；Cookie、CSRF、登出、改口令、停用。
func TestR2SessionIdentity(t *testing.T) {
	env := newSessionAPI(t, APIConfig{})
	anon := &sessClient{t: t, base: env.url, hc: newJar(t)}

	// 没有匿名模式：/api 除登录外全部 401；健康检查不需要会话。
	for _, p := range []string{"/api/tasks", "/api/auth/session", "/api/runtime/version", "/api/tasks/1/events", "/api/llm/health"} {
		anon.expect("GET", p, nil, 401, "unauthenticated")
	}
	anon.expect("POST", "/api/tasks", map[string]any{"name": "x", "goal": "y"}, 401, "unauthenticated")
	if code, _ := anon.send("GET", "/healthz", nil, nil, nil); code != 200 {
		t.Fatalf("healthz: %d", code)
	}
	// 旧的共享令牌与身份请求头都不能让匿名请求通过。
	for _, h := range []map[string]string{
		{"Authorization": "Bearer test-token"},
		{"X-Autobid-User": "admin"},
		{testActorHeader: "tester"},
	} {
		if code, _ := anon.send("GET", "/api/tasks", nil, h, nil); code != 401 {
			t.Fatalf("anonymous with %v: %d", h, code)
		}
	}
	if code, _ := anon.send("GET", "/api/tasks/1/events?token=test-token", nil, nil, nil); code != 401 {
		t.Fatalf("SSE ?token= must not authenticate: %d", code)
	}
	// 没有默认账号：常见默认口令组合登录失败；schema 不预置账号。
	for _, l := range []string{"admin", "root", "autobid"} {
		for _, p := range []string{"admin", "password", "autobid", "123456"} {
			var r r2Resp
			if code, raw := anon.send("POST", "/api/auth/login", map[string]string{"login": l, "password": p}, nil, &r); code != 401 || r.Code != "invalid_credentials" ||
				strings.Contains(raw, p) {
				t.Fatalf("default credential %s/%s: %d %s", l, p, code, raw)
			}
		}
	}
	schema, err := os.ReadFile("../db/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToUpper(string(schema)), "INSERT INTO USERS") {
		t.Fatal("schema.sql must not seed any account")
	}

	alice := env.user("alice", false)
	admin := env.user("admin", true)
	c := env.loginAs(alice)

	// Cookie：HttpOnly + SameSite=Strict；明文 HTTP 下不设 Secure。
	resp, err := http.Post(env.url+"/api/auth/login", "application/json", strings.NewReader(fmt.Sprintf(`{"login":%q,"password":%q}`, alice, r2Password)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var sc *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == SessionCookie {
			sc = ck
		}
	}
	if sc == nil || !sc.HttpOnly || sc.SameSite != http.SameSiteStrictMode || sc.Secure || len(sc.Value) < 40 {
		t.Fatalf("session cookie: %+v", sc)
	}

	// CSRF：改变状态的请求缺令牌、令牌错误或跨站 Origin 一律 403。
	body := map[string]any{"name": "r2-csrf", "goal": "g", "legacy_writing": true}
	for name, h := range map[string]map[string]string{
		"no token":      {CSRFHeader: ""},
		"wrong token":   {CSRFHeader: "forged"},
		"other session": {CSRFHeader: identity.CSRFToken("someone-elses-token")},
		"cross origin":  {"Origin": "http://evil.example"},
	} {
		var r r2Resp
		if code, raw := c.send("POST", "/api/tasks", body, h, &r); code != 403 || !strings.HasPrefix(r.Code, "csrf_") {
			t.Fatalf("%s: %d %s", name, code, raw)
		}
	}

	// 性质 1：请求头、查询参数都不能指定或改变"我是谁"。
	spoof := map[string]string{"X-Autobid-User": admin, testActorHeader: admin, "X-Forwarded-User": admin, "Remote-User": admin}
	var created struct {
		ID int64 `json:"id"`
	}
	if code, raw := c.send("POST", "/api/tasks?user="+admin+"&operator="+admin, body, spoof, &created); code != 201 {
		t.Fatalf("create task: %d %s", code, raw)
	}
	var members []db.ProjectMember
	if code := c.do("GET", fmt.Sprintf("/api/tasks/%d/members", created.ID), nil, &members); code != 200 || len(members) != 1 || members[0].Login != alice {
		t.Fatalf("owner must be the session account: %d %+v", code, members)
	}
	var who sessionView
	if code, _ := c.send("GET", "/api/auth/session", nil, spoof, &who); code != 200 || who.User.Login != alice || who.User.Admin {
		t.Fatalf("session identity with spoofing headers: %+v", who)
	}
	// 请求体里的身份字段被拒绝（未知字段），不会被采纳。
	c.expect("POST", "/api/tasks", map[string]any{"name": "x", "goal": "y", "created_by": admin}, 400, "")
	var pv qualityPolicyView
	c.do("GET", fmt.Sprintf("/api/tasks/%d/quality-policy", created.ID), nil, &pv)
	if len(pv.Events) != 1 || pv.Events[0].CreatedBy != alice {
		t.Fatalf("policy event author: %+v", pv.Events)
	}

	// SSE 走同一会话（Cookie），不需要 URL 令牌。
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/tasks/%d/events", env.url, created.ID), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sresp, err := c.hc.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if sresp.StatusCode != 200 || !strings.HasPrefix(sresp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("SSE with session: %d %s", sresp.StatusCode, sresp.Header.Get("Content-Type"))
	}
	_, _ = bufio.NewReader(sresp.Body).ReadString('\n')
	sresp.Body.Close()
	cancel()

	// 登出后旧 Cookie 立即失效（即使客户端保留了它）。
	c2 := env.loginAs(alice)
	oldCookie := cookieOf(c2)
	if code := c2.do("POST", "/api/auth/logout", nil, nil); code != 204 {
		t.Fatalf("logout: %d", code)
	}
	replay := &sessClient{t: t, base: env.url, hc: newJar(t)}
	replay.hc.Jar.SetCookies(mustURL(env.url), []*http.Cookie{{Name: SessionCookie, Value: oldCookie, Path: "/"}})
	replay.expect("GET", "/api/auth/session", nil, 401, "session_expired")

	// 改口令：其他会话立即失效，本次请求换发新会话；旧口令不能再登录。
	other := env.loginAs(alice)
	var changed sessionView
	if code, raw := c.send("POST", "/api/auth/password", map[string]string{"current_password": "wrong password!!", "new_password": "another long passphrase"}, nil, nil); code != 403 {
		t.Fatalf("change with wrong current password: %d %s", code, raw)
	}
	if code, raw := c.send("POST", "/api/auth/password", map[string]string{"current_password": r2Password, "new_password": alice}, nil, nil); code != 400 {
		t.Fatalf("password equal to login: %d %s", code, raw)
	}
	if code, raw := c.send("POST", "/api/auth/password", map[string]string{"current_password": r2Password, "new_password": "another long passphrase"}, nil, &changed); code != 200 {
		t.Fatalf("change password: %d %s", code, raw)
	}
	c.csrf = changed.CSRFToken
	other.expect("GET", "/api/auth/session", nil, 401, "session_expired")
	if code := c.do("GET", "/api/auth/session", nil, nil); code != 200 {
		t.Fatalf("the changing session gets a fresh cookie: %d", code)
	}
	anon.expect("POST", "/api/auth/login", map[string]string{"login": alice, "password": r2Password}, 401, "invalid_credentials")

	// 停用账号：现有会话立即失效，不能再登录。
	ad := env.loginAs(admin)
	u, err := env.pg.GetUserByLogin(context.Background(), db.DefaultTenant, alice)
	if err != nil {
		t.Fatal(err)
	}
	if code, raw := ad.send("PATCH", fmt.Sprintf("/api/users/%d", u.ID), map[string]string{"status": "disabled"}, nil, nil); code != 200 {
		t.Fatalf("disable: %d %s", code, raw)
	}
	c.expect("GET", "/api/auth/session", nil, 401, "session_expired")
	anon.expect("POST", "/api/auth/login", map[string]string{"login": alice, "password": "another long passphrase"}, 401, "invalid_credentials")

	// 口令与会话令牌不出现在审计事件里。
	evs, err := env.pg.ListUserEvents(context.Background(), db.DefaultTenant, u.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if s := string(e.Detail); strings.Contains(s, "passphrase") || strings.Contains(s, "argon2") || strings.Contains(s, r2Password) {
			t.Fatalf("secret in user event: %s", s)
		}
	}
}

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }

func cookieOf(c *sessClient) string {
	for _, ck := range c.hc.Jar.Cookies(mustURL(c.base)) {
		if ck.Name == SessionCookie {
			return ck.Value
		}
	}
	return ""
}

// 性质 8：登录失败限速（账号锁定）且错误响应不回显口令。
func TestR2LoginThrottle(t *testing.T) {
	env := newSessionAPI(t, APIConfig{LoginFailures: db.LoginFailurePolicy{Free: 3, Base: time.Minute, Max: time.Minute}})
	bob := env.user("bob", false)
	anon := &sessClient{t: t, base: env.url, hc: newJar(t)}
	for i := 0; i < 3; i++ {
		var r r2Resp
		code, raw := anon.send("POST", "/api/auth/login", map[string]string{"login": bob, "password": fmt.Sprintf("guess-%d-secret", i)}, nil, &r)
		if code != 401 || strings.Contains(raw, "guess-") {
			t.Fatalf("attempt %d: %d %s", i, code, raw)
		}
	}
	// 已锁定：即使口令正确也要等待，响应带 Retry-After。
	anon.expect("POST", "/api/auth/login", map[string]string{"login": bob, "password": r2Password}, 429, "login_throttled")
	u, _ := env.pg.GetUserByLogin(context.Background(), db.DefaultTenant, bob)
	if u.LockedUntil == nil || time.Until(*u.LockedUntil) < 30*time.Second {
		t.Fatalf("account lock: %+v", u.LockedUntil)
	}
	// 运维重置口令解除锁定（ops.bat user reset-password 走同一存储函数）。
	hash, _ := identity.HashPassword(r2Password)
	if err := env.pg.SetUserPassword(context.Background(), db.DefaultTenant, u.ID, hash, "password_reset", "ops"); err != nil {
		t.Fatal(err)
	}
	env.loginAs(bob)

	// 按来源地址的限速：同一来源大量失败后，连不存在的账号也直接 429。
	for i := 0; i < 25; i++ {
		anon.send("POST", "/api/auth/login", map[string]string{"login": r2Login("nobody"), "password": "x"}, nil, nil)
	}
	anon.expect("POST", "/api/auth/login", map[string]string{"login": r2Login("nobody"), "password": "x"}, 429, "login_throttled")
}

// 性质 6：每条注册的路由都在权限表里声明；表里没有已失效的路由。
func TestEveryRouteHasPermission(t *testing.T) {
	api := &API{cfg: APIConfig{WebDir: t.TempDir()}, authn: stubAuth{}}
	_ = os.WriteFile(api.cfg.WebDir+"/index.html", []byte("x"), 0o644)
	api.Handler()
	patterns := api.routes.registeredPatterns()
	if len(patterns) < 190 {
		t.Fatalf("expected the full route set, got %d", len(patterns))
	}
	seen := map[string]bool{}
	for _, p := range patterns {
		seen[p] = true
		perm, ok := routePerms[p]
		if !ok {
			t.Errorf("route %q has no declared permission", p)
			continue
		}
		if strings.Contains(p, "/api/tasks/{id}") && perm.kind != permProject {
			t.Errorf("project route %q must be a project permission", p)
		}
		if perm.kind == permPublic && p != "GET /healthz" && p != "/" && p != "POST /api/auth/login" {
			t.Errorf("route %q must not be public", p)
		}
		for _, r := range perm.roles {
			if !db.ValidRole(r) {
				t.Errorf("route %q names unknown role %q", p, r)
			}
		}
	}
	for p := range routePerms {
		if !seen[p] {
			t.Errorf("permission declared for unregistered route %q", p)
		}
	}
	t.Logf("%d routes, all declared", len(patterns))
}

// 性质 6：非成员 404、角色不足 403 + code；管理员身份本身不能读项目内容；作者与批准人分离不被角色替代。
func TestR2RolePermissions(t *testing.T) {
	env := newSessionAPI(t, APIConfig{})
	adminL, leadL, writerL, reviewerL, bizL, outsiderL := env.user("admin", true), env.user("lead", false), env.user("writer", false),
		env.user("reviewer", false), env.user("biz", false), env.user("outsider", false)
	admin, lead, writer, reviewer, biz, outsider := env.loginAs(adminL), env.loginAs(leadL), env.loginAs(writerL), env.loginAs(reviewerL),
		env.loginAs(bizL), env.loginAs(outsiderL)
	id := lead.createTask("r2-roles")
	base := fmt.Sprintf("/api/tasks/%d", id)
	for login, roles := range map[string][]string{writerL: {db.RoleTechWriter}, reviewerL: {db.RoleReviewer}, bizL: {db.RoleCommercial}} {
		if code, raw := lead.send("PUT", base+"/members/"+login, map[string]any{"roles": roles}, nil, nil); code != 200 {
			t.Fatalf("add member %s: %d %s", login, code, raw)
		}
	}
	writer.expect("PUT", base+"/members/"+outsiderL, map[string]any{"roles": []string{db.RoleReviewer}}, 403, "forbidden_role")

	// 非成员：项目一律 404，列表里看不到。
	outsider.expect("GET", base, nil, 404, "not_found")
	outsider.expect("GET", base+"/requirements", nil, 404, "not_found")
	outsider.expect("POST", base+"/pause", nil, 404, "not_found")
	outsider.expect("GET", "/api/tasks/999999999", nil, 404, "not_found")
	var list []struct {
		ID int64 `json:"id"`
	}
	outsider.do("GET", "/api/tasks", nil, &list)
	for _, x := range list {
		if x.ID == id {
			t.Fatal("non-member must not see the project in the list")
		}
	}
	// 来源项目也必须是成员。
	outsider.expect("POST", "/api/tasks", map[string]any{"name": "copy", "goal": "g", "source_task_ids": []int64{id}}, 404, "not_found")

	// 成员都可读；角色不足 403 并带所需角色。
	var view struct {
		MyRoles []string `json:"my_roles"`
	}
	if code := writer.do("GET", base, nil, &view); code != 200 || len(view.MyRoles) != 1 || view.MyRoles[0] != db.RoleTechWriter {
		t.Fatalf("writer reads project: %d %+v", code, view)
	}
	var r r2Resp
	if code, _ := writer.send("PUT", base+"/requirements/bid-scope", map[string]any{"lots": []string{}}, nil, &r); code != 403 || r.Code != "forbidden_role" ||
		len(r.RequiredRoles) != 1 || r.RequiredRoles[0] != db.RoleBidManager {
		t.Fatalf("writer sets bid scope: %d %+v", code, r)
	}
	writer.expect("POST", base+"/pause", nil, 403, "forbidden_role")
	writer.expect("POST", base+"/sections/1/storyboard/approve", map[string]any{"note": "x"}, 403, "forbidden_role")
	reviewer.expect("PUT", base+"/sections/1/storyboard", map[string]any{}, 403, "forbidden_role")
	reviewer.expect("PUT", base+"/quality-policy", map[string]any{"policy": db.QualityPolicyGrounded, "reason": "x"}, 403, "forbidden_role")
	biz.expect("POST", base+"/sections/1/dispatch", map[string]any{}, 403, "forbidden_role")
	writer.expect("PUT", base+"/commitments/C1", map[string]any{}, 403, "forbidden_role")
	// 最终稿只能由投标经理提交（处理函数内的附加判定）；预览稿撰写人也可以。
	writer.expect("POST", base+"/exports", map[string]any{"mode": "final", "request_key": "r2-final"}, 403, "forbidden_role")
	if code, raw := lead.send("POST", base+"/exports", map[string]any{"mode": "final", "request_key": "r2-final"}, nil, &r); code == 403 {
		t.Fatalf("manager final export must pass the role check: %s", raw)
	}

	// 质量策略：投标经理修改并留痕。
	var pv qualityPolicyView
	if code, raw := lead.send("PUT", base+"/quality-policy", map[string]any{"policy": db.QualityPolicyGrounded, "reason": "改为落证写作"}, nil, &pv); code != 200 ||
		pv.Policy != db.QualityPolicyGrounded || pv.Events[0].CreatedBy != leadL || pv.Events[0].Reason != "改为落证写作" {
		t.Fatalf("set policy: %d %s", code, raw)
	}
	lead.expect("PUT", base+"/quality-policy", map[string]any{"policy": "strict/v9", "reason": "x"}, 400, "invalid_input")

	// 租户级资料库：只有在任一项目中担任商务专员（或管理员）才能写资信。
	writer.expect("POST", "/api/company/credentials", map[string]any{}, 403, "forbidden_role")
	if code, raw := biz.send("POST", "/api/company/credentials", map[string]any{}, nil, &r); code == 403 {
		t.Fatalf("commercial member writes credentials: %s", raw)
	}
	outsider.expect("POST", "/api/knowledge/indexes", map[string]any{}, 403, "admin_required")
	writer.expect("GET", "/api/llm/settings", nil, 403, "admin_required")
	writer.expect("GET", "/api/users", nil, 403, "admin_required")
	if code := writer.do("GET", "/api/users/directory", nil, nil); code != 200 {
		t.Fatalf("directory for members: %d", code)
	}

	// 管理员：不是成员时读不到项目内容，但能管理成员（留痕）与模型配置。
	admin.expect("GET", base, nil, 404, "not_found")
	admin.expect("GET", base+"/requirements", nil, 404, "not_found")
	if code, raw := admin.send("PUT", base+"/members/"+outsiderL, map[string]any{"roles": []string{db.RoleReviewer}}, nil, nil); code != 200 {
		t.Fatalf("admin manages members: %d %s", code, raw)
	}
	if code := admin.do("GET", "/api/llm/settings", nil, nil); code != 200 {
		t.Fatalf("admin reads model settings: %d", code)
	}
	if code := outsider.do("GET", base, nil, nil); code != 200 {
		t.Fatalf("added member reads: %d", code)
	}
	var mev []db.MemberEvent
	lead.do("GET", base+"/member-events", nil, &mev)
	if len(mev) == 0 || mev[0].Login != outsiderL || mev[0].CreatedBy != adminL || mev[0].Action != "added" {
		t.Fatalf("member events: %+v", mev)
	}
	// 不能移除最后一名投标经理。
	lead.expect("DELETE", base+"/members/"+leadL, nil, 409, "last_manager")
}

// 后台作业：提交时检查权限，作业按记录的身份跑完——提交人随后被停用，AI 稿作者仍是 "ai:<派发人>"。
func TestR2BackgroundJobKeepsSubmitter(t *testing.T) {
	writerStub := newGatedWriter()
	env := newSessionAPI(t, APIConfig{SectionWriter: writerStub})
	leadL, writerL, adminL := env.user("lead", false), env.user("writer", false), env.user("admin", true)
	lead, writer, admin := env.loginAs(leadL), env.loginAs(writerL), env.loginAs(adminL)
	id := lead.createTask("r2-job")
	p := dbtest.ReadyTask(t, env.pg, id, dbtest.DefaultSpec(2))
	if code, raw := lead.send("PUT", fmt.Sprintf("/api/tasks/%d/members/%s", id, writerL), map[string]any{"roles": []string{db.RoleTechWriter}}, nil, nil); code != 200 {
		t.Fatalf("add writer: %d %s", code, raw)
	}
	var sec int64
	for _, v := range p.Sections {
		sec = v
		break
	}
	var det db.SectionDetail
	if code := writer.do("GET", fmt.Sprintf("/api/tasks/%d/sections/%d", id, sec), nil, &det); code != 200 {
		t.Fatalf("get section: %d", code)
	}
	var claim db.SectionClaim
	if code, raw := writer.send("POST", fmt.Sprintf("/api/tasks/%d/sections/%d/dispatch", id, sec), map[string]any{"base_rev_no": det.Section.CurrentRevNo, "instruction": "（重写）"}, nil, &claim); code != 202 {
		t.Fatalf("dispatch: %d %s", code, raw)
	}
	<-writerStub.started
	u, _ := env.pg.GetUserByLogin(context.Background(), db.DefaultTenant, writerL)
	if code, raw := admin.send("PATCH", fmt.Sprintf("/api/users/%d", u.ID), map[string]string{"status": "disabled"}, nil, nil); code != 200 {
		t.Fatalf("disable writer: %d %s", code, raw)
	}
	writer.expect("GET", "/api/auth/session", nil, 401, "session_expired")
	writerStub.release <- "AI 重写稿"
	deadline := time.Now().Add(10 * time.Second)
	for {
		if code := lead.do("GET", fmt.Sprintf("/api/tasks/%d/sections/%d", id, sec), nil, &det); code != 200 {
			t.Fatalf("lead reads section: %d", code)
		}
		if det.Current != nil && det.Current.ContentMD == "AI 重写稿（重写）" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not finish: %+v", det.Section)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if det.Current.CreatedBy != "ai:"+writerL {
		t.Fatalf("AI revision author: %q", det.Current.CreatedBy)
	}
}
