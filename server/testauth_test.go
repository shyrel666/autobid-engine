package server

// 测试替身鉴权（只存在于测试代码，不编译进服务程序）。
//
// 大多数服务端测试关注业务行为而不是登录流程：testAPI 构造的 API 用 testAuth 替代会话鉴权，
// 身份取自测试专用请求头 X-Test-Actor（缺省 "tester"），首次出现的账号自动建号并设为管理员，
// 被访问的项目自动补齐四个角色。身份、会话、CSRF、成员 404 / 角色 403 等 R2 性质由 auth_test.go
// 用生产的 sessionAuth 与真实登录覆盖。

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"sync"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/intercept"
)

// testActorHeader 是测试替身读取身份的请求头；生产鉴权不读它。
const testActorHeader = "X-Test-Actor"

type testAuth struct {
	pg    *db.DB
	mu    sync.Mutex
	users map[string]*db.User
}

var testTaskPath = regexp.MustCompile(`^/api/tasks/(\d+)(/|$)`)

func (t *testAuth) user(ctx context.Context, login string) (*db.User, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if u := t.users[login]; u != nil {
		return u, nil
	}
	u, err := t.pg.GetUserByLogin(ctx, db.DefaultTenant, login)
	if err != nil {
		u, err = t.pg.CreateUser(ctx, db.DefaultTenant, db.NewUser{Login: login, PasswordHash: "test-double-no-password", Admin: true}, "test")
		if err != nil {
			if u, err = t.pg.GetUserByLogin(ctx, db.DefaultTenant, login); err != nil {
				return nil, err
			}
		}
	}
	t.users[login] = u
	return u, nil
}

func (t *testAuth) authenticate(_ http.ResponseWriter, r *http.Request) (*Identity, error) {
	login := r.Header.Get(testActorHeader)
	if login == "" {
		login = "tester"
	}
	u, err := t.user(r.Context(), login)
	if err != nil {
		return nil, err
	}
	if m := testTaskPath.FindStringSubmatch(r.URL.Path); m != nil {
		id, _ := strconv.ParseInt(m[1], 10, 64)
		if _, err := t.pg.ExecContext(r.Context(), `INSERT INTO project_members(tenant_id, task_id, user_id, roles, created_by, updated_by)
			SELECT 'default', $1, $2, ARRAY['bid_manager','tech_writer','commercial','reviewer'], 'test', 'test'
			WHERE EXISTS (SELECT 1 FROM tasks WHERE id = $1)
			ON CONFLICT (tenant_id, task_id, user_id) DO UPDATE SET roles = EXCLUDED.roles`, id, u.ID); err != nil {
			return nil, err
		}
	}
	return &Identity{UserID: u.ID, Tenant: u.TenantID, Login: u.Login, DisplayName: u.DisplayName, Admin: true}, nil
}

// useTestAuth 把 API 的鉴权换成测试替身。
func useTestAuth(a *API) *API {
	a.authn = &testAuth{pg: a.pg, users: map[string]*db.User{}}
	return a
}

// testAPI 等同于 NewAPI，但使用测试替身鉴权。
func testAPI(runCtx context.Context, m *Manager, e *Engine, svc *LLMService, ic *intercept.Interceptor, hub *Hub, cfg APIConfig) *API {
	return useTestAuth(NewAPI(runCtx, m, e, svc, ic, hub, cfg))
}
