package server

// R2 路由权限：每条注册的路由都必须在 routePerms 里声明所需权限，未声明的路由一律拒绝（默认拒绝）。
// TestEveryRouteHasPermission 遍历 Handler() 注册的全部路由，漏写即失败。
//
// 判定顺序：会话（auth 中间件）→ 本表 → 处理函数。项目路由先查成员关系：非成员一律 404（不暴露项目是否存在），
// 成员但角色不足 403 + code。作者与批准人分离等逐对象判定仍在存储层，角色不能替代它。
// Planner / Worker 不经过本表：它们不持有任何人的角色，只受 IntentMandate 的对象范围约束。

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"strconv"

	"github.com/autobid/autobid-engine/db"
)

type permKind int

const (
	permPublic   permKind = iota + 1 // 无需会话（登录接口、健康检查、静态页面）
	permAuthed                       // 任何已登录账号
	permAdmin                        // 账号管理员
	permProject                      // 路径 {id} 项目的成员；roles 非空时须担任其一
	permTenant                       // 管理员，或在任一项目中担任 roles 之一（企业资料库等租户级资源）
	permResolved                     // 由 resolve 从对象 ID 找到项目后按 permProject 判定；找不到项目时按 fallback 判定
)

type perm struct {
	kind  permKind
	roles []string
	// adminOK 允许非成员的管理员访问该项目路由（只用于成员管理，管理员身份本身不能读项目内容）。
	adminOK bool
	// resolve 用于 permResolved：返回对象所属项目（0 表示不属于任何项目）。
	resolve  func(ctx context.Context, pg *db.DB, r *http.Request) (int64, error)
	fallback permKind
}

const (
	rM = db.RoleBidManager
	rW = db.RoleTechWriter
	rC = db.RoleCommercial
	rR = db.RoleReviewer
)

var (
	public = perm{kind: permPublic}
	authed = perm{kind: permAuthed}
	admin  = perm{kind: permAdmin}
	member = perm{kind: permProject}
)

func roles(rs ...string) perm  { return perm{kind: permProject, roles: rs} }
func tenant(rs ...string) perm { return perm{kind: permTenant, roles: rs} }

func retrievalRunTask(ctx context.Context, pg *db.DB, r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("runID"), 10, 64)
	if err != nil || id <= 0 {
		return 0, nil
	}
	var task sql.NullInt64
	err = pg.QueryRowContext(ctx, `SELECT task_id FROM retrieval_runs WHERE tenant_id = $1 AND id = $2`, tenantOf(r), id).Scan(&task)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return task.Int64, err
}

func interceptTask(ctx context.Context, pg *db.DB, r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("pendingID"), 10, 64)
	if err != nil || id <= 0 {
		return 0, nil
	}
	var task sql.NullInt64
	err = pg.QueryRowContext(ctx, `SELECT task_id FROM intercept_pending WHERE id = $1`, id).Scan(&task)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return task.Int64, err
}

func resolved(resolve func(context.Context, *db.DB, *http.Request) (int64, error), fallback permKind, rs ...string) perm {
	return perm{kind: permResolved, roles: rs, resolve: resolve, fallback: fallback}
}

// routePerms 是全部路由的权限矩阵。键与 mux 注册的模式逐字相同。
var routePerms = map[string]perm{
	"GET /healthz": public,
	"/":            public, // 前端静态产物与 SPA 回退

	// 登录与账号
	"POST /api/auth/login":                    public,
	"POST /api/auth/logout":                   authed,
	"GET /api/auth/session":                   authed,
	"POST /api/auth/password":                 authed,
	"GET /api/users/directory":                authed,
	"GET /api/users":                          admin,
	"POST /api/users":                         admin,
	"PATCH /api/users/{userID}":               admin,
	"POST /api/users/{userID}/password":       admin,
	"GET /api/user-events":                    admin,
	"GET /api/legacy-actors":                  admin,
	"POST /api/legacy-actors":                 admin,
	"DELETE /api/legacy-actors/{name}":        admin,
	"GET /api/tasks/{id}/members":             {kind: permProject, adminOK: true},
	"PUT /api/tasks/{id}/members/{login}":     {kind: permProject, roles: []string{rM}, adminOK: true},
	"DELETE /api/tasks/{id}/members/{login}":  {kind: permProject, roles: []string{rM}, adminOK: true},
	"GET /api/tasks/{id}/member-events":       {kind: permProject, adminOK: true},
	"GET /api/tasks/{id}/quality-policy":      member,
	"PUT /api/tasks/{id}/quality-policy":      roles(rM),

	// 任务生命周期与执行控制
	"GET /api/tasks":                                  authed, // 处理函数只列出本人所属项目（管理员看到全部项目的元数据）
	"POST /api/tasks":                                 authed, // 创建人成为该项目的投标经理
	"GET /api/tasks/{id}":                             member,
	"DELETE /api/tasks/{id}":                          roles(rM),
	"POST /api/tasks/{id}/start":                      roles(rM),
	"POST /api/tasks/{id}/pause":                      roles(rM),
	"POST /api/tasks/{id}/resume":                     roles(rM),
	"POST /api/tasks/{id}/revive":                     roles(rM),
	"GET /api/tasks/{id}/coverage":                    member,
	"POST /api/tasks/{id}/goals":                      roles(rM),
	"POST /api/tasks/{id}/hints":                      roles(rM, rW),
	"POST /api/tasks/{id}/works/{intentID}/control":   roles(rM, rW),
	"POST /api/tasks/{id}/works/{intentID}/steer":     roles(rM, rW),
	"POST /api/tasks/{id}/works/{intentID}/message":   roles(rM, rW),
	"GET /api/tasks/{id}/overview":                    member,
	"GET /api/tasks/{id}/graph":                       member,
	"GET /api/tasks/{id}/nodes/{nodeID}":              member,
	"GET /api/tasks/{id}/findings":                    member,
	"GET /api/tasks/{id}/assets":                      member,
	"GET /api/assets/{assetID}/edges":                 authed, // 招标静态要素按招标编号共享，不属于单个项目
	"GET /api/tasks/{id}/report":                      member,
	"GET /api/tasks/{id}/events":                      member,
	"GET /api/tasks/{id}/agent-jobs":                  member,
	"GET /api/tasks/{id}/llm-chain":                   member,
	"PUT /api/tasks/{id}/llm-chain":                   roles(rM),
	"POST /api/tasks/{id}/llm-chain/reset":            roles(rM),
	"GET /api/tasks/{id}/llm-usage":                   member,
	"GET /api/tasks/{id}/intercept-audit":             member,

	// 招标文件、材料与解析
	"POST /api/tasks/{id}/documents":                                  roles(rM, rW),
	"GET /api/tasks/{id}/documents":                                   member,
	"POST /api/tasks/{id}/documents/{docID}/reparse":                  roles(rM, rW),
	"POST /api/tasks/{id}/files":                                      roles(rM, rW),
	"GET /api/tasks/{id}/files":                                       member,
	"POST /api/tasks/{id}/files/{fileID}/import":                      roles(rM, rW),
	"GET /api/tasks/{id}/file-processing-records":                     member,
	"GET /api/tasks/{id}/file-processing-records/{recordID}":          member,
	"POST /api/tasks/{id}/file-processing-records/{recordID}/adopt":   roles(rM),
	"POST /api/tasks/{id}/file-processing-records/{recordID}/reject":  roles(rM),
	"GET /api/tasks/{id}/materials":                                   member,
	"POST /api/tasks/{id}/materials":                                  roles(rM, rW, rC),
	"POST /api/tasks/{id}/file-jobs":                                  roles(rM, rW),
	"GET /api/tasks/{id}/file-jobs":                                   member,
	"GET /api/tasks/{id}/file-jobs/{jobID}":                           member,
	"POST /api/tasks/{id}/file-jobs/{jobID}/cancel":                   roles(rM, rW),

	// 企业资料库（租户级）
	"GET /api/company/credentials":                                 authed,
	"POST /api/company/credentials":                                tenant(rC),
	"PUT /api/company/credentials/{credID}":                        tenant(rC),
	"DELETE /api/company/credentials/{credID}":                     tenant(rC),
	"GET /api/company/performance":                                 authed,
	"POST /api/company/performance":                                tenant(rC),
	"DELETE /api/company/performance/{perfID}":                     tenant(rC),
	"GET /api/company/evidence/{domain}/{ownerID}/originals":       authed,
	"POST /api/company/evidence/{domain}/{ownerID}/originals":      tenant(rC),

	// 企业方案知识库与检索
	"GET /api/knowledge/documents":                       authed,
	"POST /api/knowledge/documents":                      tenant(rM, rW),
	"GET /api/knowledge/documents/{kid}":                 authed,
	"DELETE /api/knowledge/documents/{kid}":              tenant(rM, rW),
	"GET /api/knowledge/search":                          authed,
	"GET /api/knowledge/indexes":                         authed,
	"GET /api/knowledge/retrieval-status":                authed,
	"GET /api/knowledge/index-events":                    authed,
	"POST /api/knowledge/indexes":                        admin,
	"POST /api/knowledge/indexes/{indexID}/activate":     admin,
	"POST /api/knowledge/indexes/{indexID}/cancel":       admin,
	"DELETE /api/knowledge/indexes/{indexID}":            admin,
	"POST /api/tasks/{id}/research":                      roles(rM, rW),
	"GET /api/tasks/{id}/retrieval-runs":                 member,
	"GET /api/retrieval/runs/{runID}":                    resolved(retrievalRunTask, permAuthed),
	"POST /api/retrieval/runs/{runID}/cancel":            resolved(retrievalRunTask, permAuthed, rM, rW),
	"GET /api/retrieval/runs/{runID}/evidence":           resolved(retrievalRunTask, permAuthed),
	"POST /api/retrieval/runs/{runID}/evidence/{evidenceID}/verify": resolved(retrievalRunTask, permAuthed, rM, rW, rR),

	// 要求清单、否决项与投标范围
	"GET /api/tasks/{id}/requirements":                   member,
	"POST /api/tasks/{id}/requirements/extract":          roles(rM),
	"POST /api/tasks/{id}/goals/bootstrap":               roles(rM),
	"POST /api/tasks/{id}/requirements/confirm":          roles(rM),
	"POST /api/tasks/{id}/requirements":                  roles(rM, rW),
	"PUT /api/tasks/{id}/requirements/{reqID}":           roles(rM, rW),
	"DELETE /api/tasks/{id}/requirements/{reqID}":        roles(rM, rW),
	"PUT /api/tasks/{id}/requirements/{reqID}/veto":      roles(rM),
	"PUT /api/tasks/{id}/requirements/bid-scope":         roles(rM),
	"PUT /api/tasks/{id}/requirements/bid-decision":      roles(rM),

	// 大纲与章节写作
	"GET /api/tasks/{id}/outlines":                                   member,
	"POST /api/tasks/{id}/outlines":                                  roles(rM, rW),
	"GET /api/tasks/{id}/outlines/{version}":                         member,
	"PUT /api/tasks/{id}/outlines/{version}":                         roles(rM, rW),
	"POST /api/tasks/{id}/outlines/{version}/submit":                 roles(rM, rW),
	"POST /api/tasks/{id}/outlines/{version}/approve":                roles(rM),
	"GET /api/tasks/{id}/sections/{sectionID}":                       member,
	"POST /api/tasks/{id}/sections/{sectionID}/dispatch":             roles(rW),
	"POST /api/tasks/{id}/sections/{sectionID}/revisions":            roles(rW),
	"POST /api/tasks/{id}/sections/{sectionID}/revisions/{revNo}/approve":  roles(rR),
	"POST /api/tasks/{id}/sections/{sectionID}/revisions/{revNo}/rollback": roles(rW),
	"GET /api/tasks/{id}/section-jobs":                               member,
	"POST /api/tasks/{id}/section-jobs/{jobID}/cancel":               roles(rM, rW),

	// D3 故事板与写作核验
	"GET /api/tasks/{id}/storyboards":                                     member,
	"GET /api/tasks/{id}/sections/{sectionID}/storyboard":                 member,
	"PUT /api/tasks/{id}/sections/{sectionID}/storyboard":                 roles(rW),
	"POST /api/tasks/{id}/sections/{sectionID}/storyboard/submit":         roles(rW),
	"POST /api/tasks/{id}/sections/{sectionID}/storyboard/approve":        roles(rR),
	"POST /api/tasks/{id}/sections/{sectionID}/storyboard/reject":         roles(rR),
	"POST /api/tasks/{id}/sections/{sectionID}/storyboard/render":         roles(rW),
	"GET /api/tasks/{id}/writing-verifications":                           member,
	"GET /api/tasks/{id}/sections/{sectionID}/writing-verification":       member,
	"POST /api/tasks/{id}/sections/{sectionID}/writing-verification/evaluate": roles(rW, rR),

	// D2 证据、可采性、承诺台账与报价
	"GET /api/tasks/{id}/requirements/{reqID}/evidence-bindings":                         member,
	"POST /api/tasks/{id}/requirements/{reqID}/evidence-bindings":                        roles(rW, rC),
	"DELETE /api/tasks/{id}/requirements/{reqID}/evidence-bindings/{domain}/{evidenceID}": roles(rW, rC),
	"GET /api/tasks/{id}/admissibility":                   member,
	"GET /api/tasks/{id}/admissibility/{runID}":           member,
	"POST /api/tasks/{id}/admissibility/evaluate":         roles(rW, rC, rR),
	"POST /api/tasks/{id}/admissibility/{runID}/resolve":  roles(rR),
	"GET /api/tasks/{id}/commitments":                     member,
	"PUT /api/tasks/{id}/commitments/{code}":              roles(rC),
	"POST /api/tasks/{id}/commitments/{code}/confirm":     roles(rM, rC),
	"POST /api/tasks/{id}/commitments/{code}/signoff":     roles(rM, rR),
	"GET /api/tasks/{id}/quotes":                          member,
	"POST /api/tasks/{id}/quotes":                         roles(rC),
	"POST /api/tasks/{id}/quotes/{quoteID}/validate":      roles(rC),
	"POST /api/tasks/{id}/quotes/{quoteID}/confirm":       roles(rM, rC),
	"GET /api/tasks/{id}/objective-scores":                member,
	"POST /api/tasks/{id}/objective-scores/compute":       roles(rM, rW, rC),

	// 响应矩阵、一致性与导出就绪度
	"POST /api/tasks/{id}/compliance/verify":                           roles(rW, rR),
	"GET /api/tasks/{id}/compliance/matrix":                            member,
	"GET /api/tasks/{id}/compliance/coverage":                          member,
	"POST /api/tasks/{id}/consistency/runs":                            roles(rW, rR),
	"GET /api/tasks/{id}/consistency/runs":                             member,
	"GET /api/tasks/{id}/consistency/findings":                         member,
	"POST /api/tasks/{id}/consistency/findings/{findingID}/resolve":    roles(rR),
	"GET /api/tasks/{id}/export-readiness":                             member,

	// 图表
	"GET /api/tasks/{id}/charts":                        member,
	"POST /api/tasks/{id}/charts":                       roles(rW),
	"GET /api/tasks/{id}/charts/{chartID}":              member,
	"GET /api/tasks/{id}/charts/{chartID}/image":        member,
	"POST /api/tasks/{id}/charts/{chartID}/render":      roles(rW),
	"POST /api/tasks/{id}/charts/{chartID}/rebind":      roles(rW),
	"DELETE /api/tasks/{id}/charts/{chartID}":           roles(rW),
	"POST /api/tasks/{id}/chart-jobs":                   roles(rW),
	"GET /api/tasks/{id}/chart-jobs":                    member,
	"GET /api/tasks/{id}/chart-jobs/{jobID}":            member,
	"POST /api/tasks/{id}/chart-jobs/{jobID}/cancel":    roles(rW),

	// 导出、请求组与提交检查清单
	"GET /api/export/status":                                           authed,
	"GET /api/export/templates":                                        authed,
	"GET /api/export/templates/default/file":                           authed,
	"POST /api/export/templates":                                       tenant(rM),
	"DELETE /api/export/templates/{templateID}":                        tenant(rM),
	"GET /api/tasks/{id}/exports/preflight":                            member,
	"POST /api/tasks/{id}/exports":                                     roles(rM, rW), // 最终稿另要求投标经理（createExport 内判定）
	"GET /api/tasks/{id}/exports":                                      member,
	"GET /api/tasks/{id}/exports/{jobID}":                              member,
	"GET /api/tasks/{id}/exports/{jobID}/download":                     member,
	"POST /api/tasks/{id}/exports/{jobID}/cancel":                      roles(rM, rW),
	"GET /api/tasks/{id}/export-requests":                              member,
	"GET /api/tasks/{id}/export-requests/{requestID}":                  member,
	"POST /api/tasks/{id}/export-requests/{requestID}/cancel":          roles(rM),
	"GET /api/tasks/{id}/export-requests/{requestID}/submission":       member,
	"PUT /api/tasks/{id}/export-requests/{requestID}/submission/{key}": roles(rM),
	"DELETE /api/tasks/{id}/export-requests/{requestID}/submission/{key}": roles(rM),

	// 模型配置与录制（含全部项目的提示与正文，只给管理员）
	"GET /api/llm/profiles":            admin,
	"POST /api/llm/profiles":           admin,
	"GET /api/llm/profiles/{pid}":      admin,
	"PUT /api/llm/profiles/{pid}":      admin,
	"DELETE /api/llm/profiles/{pid}":   admin,
	"PUT /api/llm/bindings/{agent}":    admin,
	"GET /api/llm/health":              authed,
	"GET /api/llm/bindings":            admin,
	"POST /api/llm/profiles/active":    admin,
	"GET /api/llm/settings":            admin,
	"PUT /api/llm/settings":            admin,
	"GET /api/llm/pool":                admin,
	"POST /api/llm/pool/reset":         admin,
	"POST /api/llm/models":             admin,
	"POST /api/llm/test":               admin,
	"GET /api/llm/records":             admin,
	"GET /api/llm/records/{recordID}":  admin,
	"DELETE /api/llm/records":          admin,
	"GET /api/system/catalog":          authed,
	"GET /api/runtime/version":         authed,
	"GET /api/runtime/tools":           authed,

	// 工具调用拦截
	"GET /api/intercepts":                      authed, // 处理函数只列出本人所属项目的待审批
	"POST /api/intercepts/{pendingID}/decide":  resolved(interceptTask, permAdmin, rM),
	"GET /api/intercept/rules":                 admin,
	"POST /api/intercept/rules":                admin,
	"PUT /api/intercept/rules/{ruleID}":        admin,
	"DELETE /api/intercept/rules/{ruleID}":     admin,
	"GET /api/intercept/config":                admin,
	"PUT /api/intercept/config":                admin,
	"GET /api/intercept/approvals":             authed, // 同上，按成员关系过滤
}

// routeMux 包装 ServeMux：注册时为每条路由套上 routePerms 中的守卫，未声明的路由永远拒绝。
type routeMux struct {
	*http.ServeMux
	a        *API
	patterns []string
}

func (m *routeMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.Handle(pattern, http.HandlerFunc(h))
}

func (m *routeMux) Handle(pattern string, h http.Handler) {
	m.patterns = append(m.patterns, pattern)
	p, ok := routePerms[pattern]
	if !ok {
		m.ServeMux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "route has no declared permission", "code": "permission_undeclared"})
		})
		return
	}
	m.ServeMux.Handle(pattern, m.a.guard(p, h))
}

// registeredPatterns 供测试遍历全部注册的路由。
func (m *routeMux) registeredPatterns() []string {
	out := append([]string(nil), m.patterns...)
	sort.Strings(out)
	return out
}

func forbidden(w http.ResponseWriter, code, msg string, required []string) {
	body := map[string]any{"error": msg, "code": code}
	if len(required) > 0 {
		body["required_roles"] = required
	}
	writeJSON(w, http.StatusForbidden, body)
}

func notMember(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found", "code": "not_found"})
}

func (a *API) guard(p perm, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.kind == permPublic {
			next.ServeHTTP(w, r)
			return
		}
		id := identityFrom(r.Context())
		if id == nil {
			writeAuthError(w, errUnauthenticated)
			return
		}
		kind := p.kind
		var taskID int64
		switch kind {
		case permProject:
			v, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil || v <= 0 {
				notMember(w)
				return
			}
			taskID = v
		case permResolved:
			v, err := p.resolve(r.Context(), a.pg, r)
			if err != nil {
				fail(w, err)
				return
			}
			if v > 0 {
				taskID, kind = v, permProject
			} else {
				kind = p.fallback
			}
		}
		switch kind {
		case permAuthed:
		case permAdmin:
			if !id.Admin {
				forbidden(w, "admin_required", "需要管理员账号", nil)
				return
			}
		case permTenant:
			if !id.Admin {
				ok, err := a.pg.HasRoleInAnyProject(r.Context(), id.Tenant, id.UserID, p.roles)
				if err != nil {
					fail(w, err)
					return
				}
				if !ok {
					forbidden(w, "forbidden_role", "需要在任一项目中担任以下角色之一，或管理员账号", p.roles)
					return
				}
			}
		case permProject:
			have, err := a.pg.MemberRoles(r.Context(), id.Tenant, taskID, id.UserID)
			if err != nil {
				fail(w, err)
				return
			}
			if len(have) == 0 {
				if !(p.adminOK && id.Admin) {
					notMember(w)
					return
				}
				if _, err := a.pg.GetTask(r.Context(), taskID); err != nil {
					notMember(w)
					return
				}
			} else if len(p.roles) > 0 && !hasAnyRole(have, p.roles...) && !(p.adminOK && id.Admin) {
				forbidden(w, "forbidden_role", "当前账号在本项目中没有所需角色", p.roles)
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), rolesKey{}, have))
		default:
			forbidden(w, "permission_undeclared", "route has no declared permission", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireProjectRole 是处理函数内的附加角色判定（例如最终稿只能由投标经理提交）；不满足时写 403 并返回 false。
func requireProjectRole(w http.ResponseWriter, r *http.Request, rs ...string) bool {
	if hasAnyRole(projectRoles(r), rs...) {
		return true
	}
	forbidden(w, "forbidden_role", "当前账号在本项目中没有所需角色", rs)
	return false
}
