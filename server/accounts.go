package server

// R2 账号、项目成员、遗留姓名映射与质量策略接口。权限见 permissions.go 的 routePerms。

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/identity"
)

func (a *API) registerIdentityRoutes(mux *routeMux) {
	mux.HandleFunc("POST /api/auth/login", a.login)
	mux.HandleFunc("POST /api/auth/logout", a.logout)
	mux.HandleFunc("GET /api/auth/session", a.currentSession)
	mux.HandleFunc("POST /api/auth/password", a.changeOwnPassword)

	mux.HandleFunc("GET /api/users/directory", a.userDirectory)
	mux.HandleFunc("GET /api/users", a.listUsers)
	mux.HandleFunc("POST /api/users", a.createUser)
	mux.HandleFunc("PATCH /api/users/{userID}", a.updateUser)
	mux.HandleFunc("POST /api/users/{userID}/password", a.resetUserPassword)
	mux.HandleFunc("GET /api/user-events", a.listUserEvents)
	mux.HandleFunc("GET /api/legacy-actors", a.listLegacyActors)
	mux.HandleFunc("POST /api/legacy-actors", a.mapLegacyActor)
	mux.HandleFunc("DELETE /api/legacy-actors/{name}", a.unmapLegacyActor)

	mux.HandleFunc("GET /api/tasks/{id}/members", a.withTask(a.listMembers))
	mux.HandleFunc("PUT /api/tasks/{id}/members/{login}", a.withTask(a.setMember))
	mux.HandleFunc("DELETE /api/tasks/{id}/members/{login}", a.withTask(a.removeMember))
	mux.HandleFunc("GET /api/tasks/{id}/member-events", a.withTask(a.listMemberEvents))
	mux.HandleFunc("GET /api/tasks/{id}/quality-policy", a.withTask(a.getQualityPolicy))
	mux.HandleFunc("PUT /api/tasks/{id}/quality-policy", a.withTask(a.setQualityPolicy))
}

// identityError 把账号相关的领域错误映射为带 code 的响应。
func identityError(w http.ResponseWriter, err error) {
	code, status := "", 0
	switch {
	case errors.Is(err, db.ErrInvalidAccount), errors.Is(err, db.ErrInvalidPolicy):
		code, status = "invalid_input", http.StatusBadRequest
	case errors.Is(err, db.ErrAccountConflict):
		code, status = "conflict", http.StatusConflict
	case errors.Is(err, db.ErrLastAdmin):
		code, status = "last_admin", http.StatusConflict
	case errors.Is(err, db.ErrLastManager):
		code, status = "last_manager", http.StatusConflict
	case errors.Is(err, db.ErrPolicyConflict):
		code, status = "policy_conflict", http.StatusConflict
	case errors.Is(err, db.ErrNotFound):
		code, status = "not_found", http.StatusNotFound
	default:
		fail(w, err)
		return
	}
	writeJSON(w, status, map[string]string{"error": err.Error(), "code": code})
}

// directoryEntry 是添加项目成员时可选的账号（只含登录名、显示名与状态）。
type directoryEntry struct {
	Login       string `json:"login"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
}

func (a *API) userDirectory(w http.ResponseWriter, r *http.Request) {
	users, err := a.pg.ListUsers(r.Context(), tenantOf(r))
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]directoryEntry, 0, len(users))
	for _, u := range users {
		out = append(out, directoryEntry{Login: u.Login, DisplayName: u.DisplayName, Status: u.Status})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.pg.ListUsers(r.Context(), tenantOf(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, users)
}

func (a *API) createUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Login       string `json:"login"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
		Admin       bool   `json:"is_admin"`
	}
	if err := decodeBodyLimit(r, &req, 8192); err != nil {
		fail(w, err)
		return
	}
	login, err := db.NormalizeLogin(req.Login)
	if err != nil {
		identityError(w, err)
		return
	}
	if err := identity.CheckPassword(login, req.Password); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "口令须为 10~256 个字符，且不能与账号相同", "code": "weak_password"})
		return
	}
	hash, err := identity.HashPassword(req.Password)
	if err != nil {
		fail(w, err)
		return
	}
	u, err := a.pg.CreateUser(r.Context(), tenantOf(r), db.NewUser{Login: login, DisplayName: req.DisplayName, PasswordHash: hash, Admin: req.Admin}, operator(r))
	if err != nil {
		identityError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (a *API) updateUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "userID")
	if err != nil {
		fail(w, err)
		return
	}
	var in db.UserUpdate
	if err := decodeBodyLimit(r, &in, 4096); err != nil {
		fail(w, err)
		return
	}
	u, err := a.pg.UpdateUser(r.Context(), tenantOf(r), id, in, operator(r))
	if err != nil {
		identityError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// resetUserPassword 由管理员为他人设置新口令（吊销该账号全部会话）。本人改口令走 /api/auth/password。
func (a *API) resetUserPassword(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "userID")
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := decodeBodyLimit(r, &req, 4096); err != nil {
		fail(w, err)
		return
	}
	u, err := a.pg.GetUser(r.Context(), tenantOf(r), id)
	if err != nil {
		identityError(w, err)
		return
	}
	if err := identity.CheckPassword(u.Login, req.Password); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "口令须为 10~256 个字符，且不能与账号相同", "code": "weak_password"})
		return
	}
	hash, err := identity.HashPassword(req.Password)
	if err != nil {
		fail(w, err)
		return
	}
	if err := a.pg.SetUserPassword(r.Context(), tenantOf(r), id, hash, "password_reset", operator(r)); err != nil {
		identityError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) listUserEvents(w http.ResponseWriter, r *http.Request) {
	var uid int64
	if v := r.URL.Query().Get("user_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			fail(w, badRequest("invalid user_id"))
			return
		}
		uid = id
	}
	ev, err := a.pg.ListUserEvents(r.Context(), tenantOf(r), uid, 200)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (a *API) listLegacyActors(w http.ResponseWriter, r *http.Request) {
	list, err := a.pg.ListLegacyActors(r.Context(), tenantOf(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *API) mapLegacyActor(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LegacyName string `json:"legacy_name"`
		Login      string `json:"login"`
	}
	if err := decodeBodyLimit(r, &req, 4096); err != nil {
		fail(w, err)
		return
	}
	if err := a.pg.MapLegacyActor(r.Context(), tenantOf(r), req.LegacyName, req.Login, operator(r)); err != nil {
		identityError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) unmapLegacyActor(w http.ResponseWriter, r *http.Request) {
	name, err := url.PathUnescape(r.PathValue("name"))
	if err != nil {
		fail(w, badRequest("invalid name"))
		return
	}
	if err := a.pg.UnmapLegacyActor(r.Context(), tenantOf(r), name, operator(r)); err != nil {
		identityError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) listMembers(w http.ResponseWriter, r *http.Request, t *Task) {
	list, err := a.pg.ListProjectMembers(r.Context(), tenantOf(r), t.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *API) setMember(w http.ResponseWriter, r *http.Request, t *Task) {
	var req struct {
		Roles []string `json:"roles"`
	}
	if err := decodeBodyLimit(r, &req, 4096); err != nil {
		fail(w, err)
		return
	}
	m, err := a.pg.SetProjectMember(r.Context(), tenantOf(r), t.ID, r.PathValue("login"), req.Roles, operator(r))
	if err != nil {
		identityError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (a *API) removeMember(w http.ResponseWriter, r *http.Request, t *Task) {
	if err := a.pg.RemoveProjectMember(r.Context(), tenantOf(r), t.ID, r.PathValue("login"), operator(r)); err != nil {
		identityError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) listMemberEvents(w http.ResponseWriter, r *http.Request, t *Task) {
	ev, err := a.pg.ListMemberEvents(r.Context(), tenantOf(r), t.ID, 200)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

type qualityPolicyView struct {
	Policy   string                   `json:"policy"`
	Policies []string                 `json:"policies"`
	Events   []*db.QualityPolicyEvent `json:"events"`
}

func (a *API) qualityPolicyView(r *http.Request, taskID int64) (*qualityPolicyView, error) {
	row, err := a.pg.GetTask(r.Context(), taskID)
	if err != nil {
		return nil, err
	}
	ev, err := a.pg.ListQualityPolicyEvents(r.Context(), tenantOf(r), taskID)
	if err != nil {
		return nil, err
	}
	return &qualityPolicyView{Policy: row.QualityPolicy, Policies: []string{db.QualityPolicyGrounded, db.QualityPolicyLegacy}, Events: ev}, nil
}

func (a *API) getQualityPolicy(w http.ResponseWriter, r *http.Request, t *Task) {
	v, err := a.qualityPolicyView(r, t.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) setQualityPolicy(w http.ResponseWriter, r *http.Request, t *Task) {
	var req struct {
		Policy string `json:"policy"`
		Reason string `json:"reason"`
	}
	if err := decodeBodyLimit(r, &req, 8192); err != nil {
		fail(w, err)
		return
	}
	if _, err := a.pg.SetQualityPolicy(r.Context(), tenantOf(r), t.ID, strings.TrimSpace(req.Policy), req.Reason, operator(r)); err != nil {
		identityError(w, err)
		return
	}
	v, err := a.qualityPolicyView(r, t.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
