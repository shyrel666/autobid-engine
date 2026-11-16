package db

// R2 可信身份：账号、服务端会话、项目成员与角色、遗留姓名映射、质量策略。
//
// 存储层只保存口令哈希与会话令牌的 SHA-256，不接触明文；哈希与校验在 internal/identity。
// 写进各业务表 *_by 列的是 users.login（创建后不可修改、租户内唯一），显示名只用于展示。
// 所有账号 / 成员 / 策略变更与对应的审计事件在同一事务内写入；事件表只追加。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// 项目角色（ROADMAP Phase 7 定义的四类）。
const (
	RoleBidManager = "bid_manager" // 投标经理
	RoleTechWriter = "tech_writer" // 技术撰写人
	RoleCommercial = "commercial"  // 商务专员
	RoleReviewer   = "reviewer"    // 审核专家
)

// AllRoles 按展示顺序列出全部项目角色。
var AllRoles = []string{RoleBidManager, RoleTechWriter, RoleCommercial, RoleReviewer}

// 质量策略（tasks.quality_policy）。版本号写在标识里：修改门禁语义时新增版本，不改写旧版本的含义。
const (
	QualityPolicyGrounded = "grounded/v1" // D3 故事板、粉队 / 红队与句子级落证门禁强制生效
	QualityPolicyLegacy   = "legacy/v1"   // 不启用 D3 门禁；不允许新建故事板
)

var (
	ErrInvalidAccount     = errors.New("identity: invalid account input")
	ErrAccountConflict    = errors.New("identity: login already exists")
	ErrLastAdmin          = errors.New("identity: at least one active administrator must remain")
	ErrLastManager        = errors.New("identity: a project must keep at least one active bid manager")
	ErrSessionInvalid     = errors.New("identity: session is missing, expired or revoked")
	ErrLegacyActor        = errors.New("identity: record was written before account login (declared name); it cannot take part in a separation-of-duties check until re-saved by its author or mapped to an account by an administrator")
	ErrInvalidPolicy      = errors.New("identity: unknown quality policy")
	ErrPolicyConflict     = errors.New("identity: quality policy change conflicts with existing records")
	ErrLegacyPolicyActive = errors.New("identity: task uses quality policy legacy/v1; a bid manager must switch it to grounded/v1 before storyboards can be created")
)

var loginRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,31}$`)

// NormalizeLogin 返回规范化（小写、去空白）的登录名；不合法时返回 ErrInvalidAccount。
func NormalizeLogin(login string) (string, error) {
	l := strings.ToLower(strings.TrimSpace(login))
	if !loginRe.MatchString(l) {
		return "", fmt.Errorf("%w: login must be 2-32 characters of a-z, 0-9, '.', '_' or '-', starting with a letter or digit", ErrInvalidAccount)
	}
	return l, nil
}

// ValidRole 判断是否为项目角色。
func ValidRole(r string) bool { return slices.Contains(AllRoles, r) }

// User 是账号的公开视图（不含口令哈希）。
type User struct {
	ID                int64      `json:"id"`
	TenantID          string     `json:"tenant_id"`
	Login             string     `json:"login"`
	DisplayName       string     `json:"display_name"`
	Admin             bool       `json:"is_admin"`
	Status            string     `json:"status"`
	LockedUntil       *time.Time `json:"locked_until,omitempty"`
	PasswordChangedAt time.Time  `json:"password_changed_at"`
	CreatedBy         string     `json:"created_by"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// UserCredential 是登录校验用的内部视图。
type UserCredential struct {
	User
	PasswordHash string
	FailedLogins int
}

const userCols = `id, tenant_id, login, display_name, is_admin, status, locked_until, password_changed_at, created_by, created_at, updated_at`

func scanUser(r rowScanner) (*User, error) {
	var u User
	var locked sql.NullTime
	if err := r.Scan(&u.ID, &u.TenantID, &u.Login, &u.DisplayName, &u.Admin, &u.Status, &locked, &u.PasswordChangedAt,
		&u.CreatedBy, &u.CreatedAt, &u.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: user", ErrNotFound)
		}
		return nil, err
	}
	u.LockedUntil = nullTimePtr(locked)
	return &u, nil
}

// NewUser 是创建账号的输入；PasswordHash 由调用方（internal/identity）生成。
type NewUser struct {
	Login        string
	DisplayName  string
	PasswordHash string
	Admin        bool
}

func userEventTx(ctx context.Context, q queryer, tenant string, userID int64, login, action string, detail any, actor string) error {
	if detail == nil {
		detail = map[string]any{}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO user_events(tenant_id, user_id, login, action, detail, created_by)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6)`, tenant, userID, login, action, string(b), actor)
	return err
}

// CreateUser 新建账号并写 created 事件。actor 为空表示本机运维工具（记为 "ops"）。
func (d *DB) CreateUser(ctx context.Context, tenant string, in NewUser, actor string) (*User, error) {
	tenant = tenantOr(tenant)
	login, err := NormalizeLogin(in.Login)
	if err != nil {
		return nil, err
	}
	display := strings.TrimSpace(in.DisplayName)
	if display == "" {
		display = login
	}
	if len([]rune(display)) > 64 {
		return nil, fmt.Errorf("%w: display name is longer than 64 characters", ErrInvalidAccount)
	}
	if in.PasswordHash == "" {
		return nil, fmt.Errorf("%w: password is required", ErrInvalidAccount)
	}
	if actor = strings.TrimSpace(actor); actor == "" {
		actor = "ops"
	}
	var out *User
	err = d.InTx(ctx, func(tx *sql.Tx) error {
		u, err := scanUser(tx.QueryRowContext(ctx, `INSERT INTO users(tenant_id, login, display_name, password_hash, is_admin, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $6) ON CONFLICT (tenant_id, login) DO NOTHING RETURNING `+userCols,
			tenant, login, display, in.PasswordHash, in.Admin, actor))
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: %s", ErrAccountConflict, login)
		}
		if err != nil {
			return err
		}
		out = u
		return userEventTx(ctx, tx, tenant, u.ID, u.Login, "created", map[string]any{"is_admin": in.Admin, "display_name": display}, actor)
	})
	return out, err
}

// GetUser 按 ID 读取账号。
func (d *DB) GetUser(ctx context.Context, tenant string, id int64) (*User, error) {
	return scanUser(d.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE tenant_id = $1 AND id = $2`, tenantOr(tenant), id))
}

// GetUserByLogin 按登录名读取账号。
func (d *DB) GetUserByLogin(ctx context.Context, tenant, login string) (*User, error) {
	return scanUser(d.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE tenant_id = $1 AND login = $2`,
		tenantOr(tenant), strings.ToLower(strings.TrimSpace(login))))
}

// UserCredentialByLogin 读取登录校验所需的口令哈希与失败计数。
func (d *DB) UserCredentialByLogin(ctx context.Context, tenant, login string) (*UserCredential, error) {
	var c UserCredential
	var locked sql.NullTime
	err := d.QueryRowContext(ctx, `SELECT `+userCols+`, password_hash, failed_logins FROM users WHERE tenant_id = $1 AND login = $2`,
		tenantOr(tenant), strings.ToLower(strings.TrimSpace(login))).Scan(&c.ID, &c.TenantID, &c.Login, &c.DisplayName, &c.Admin, &c.Status,
		&locked, &c.PasswordChangedAt, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt, &c.PasswordHash, &c.FailedLogins)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: user", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	c.LockedUntil = nullTimePtr(locked)
	return &c, nil
}

// ListUsers 列出租户内全部账号。
func (d *DB) ListUsers(ctx context.Context, tenant string) ([]*User, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+userCols+` FROM users WHERE tenant_id = $1 ORDER BY login`, tenantOr(tenant))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountActiveAdmins 返回租户内可用的管理员数量（ops check 用）。
func (d *DB) CountActiveAdmins(ctx context.Context, tenant string) (int, error) {
	var n int
	err := d.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE tenant_id = $1 AND is_admin AND status = 'active'`, tenantOr(tenant)).Scan(&n)
	return n, err
}

// UserUpdate 是管理员修改账号的输入；nil 字段不改。
type UserUpdate struct {
	DisplayName *string `json:"display_name,omitempty"`
	Admin       *bool   `json:"is_admin,omitempty"`
	Status      *string `json:"status,omitempty"`
}

func lockUserTx(ctx context.Context, tx *sql.Tx, tenant string, id int64) (*User, error) {
	return scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenant, id))
}

func ensureAdminRemainsTx(ctx context.Context, tx *sql.Tx, tenant string, excluding int64) error {
	// 锁住全部管理员行，避免两个并发请求各自停用"另一个"最后的管理员。
	rows, err := tx.QueryContext(ctx, `SELECT id FROM users WHERE tenant_id = $1 AND is_admin AND status = 'active' AND id <> $2 FOR UPDATE`, tenant, excluding)
	if err != nil {
		return err
	}
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if n == 0 {
		return ErrLastAdmin
	}
	return nil
}

// UpdateUser 修改显示名、管理员标记或状态；停用账号时同事务吊销其全部会话。
func (d *DB) UpdateUser(ctx context.Context, tenant string, id int64, in UserUpdate, actor string) (*User, error) {
	tenant = tenantOr(tenant)
	var out *User
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		u, err := lockUserTx(ctx, tx, tenant, id)
		if err != nil {
			return err
		}
		if in.DisplayName != nil {
			name := strings.TrimSpace(*in.DisplayName)
			if name == "" || len([]rune(name)) > 64 {
				return fmt.Errorf("%w: display name must be 1-64 characters", ErrInvalidAccount)
			}
			if name != u.DisplayName {
				if _, err := tx.ExecContext(ctx, `UPDATE users SET display_name = $2, updated_by = $3 WHERE id = $1`, id, name, actor); err != nil {
					return err
				}
				if err := userEventTx(ctx, tx, tenant, id, u.Login, "display_name_changed", map[string]any{"from": u.DisplayName, "to": name}, actor); err != nil {
					return err
				}
			}
		}
		if in.Status != nil && *in.Status != u.Status {
			switch *in.Status {
			case "disabled":
				if u.Admin {
					if err := ensureAdminRemainsTx(ctx, tx, tenant, id); err != nil {
						return err
					}
				}
				if _, err := tx.ExecContext(ctx, `UPDATE users SET status = 'disabled', updated_by = $2 WHERE id = $1`, id, actor); err != nil {
					return err
				}
				if err := revokeUserSessionsTx(ctx, tx, id, "account_disabled"); err != nil {
					return err
				}
				if err := userEventTx(ctx, tx, tenant, id, u.Login, "disabled", nil, actor); err != nil {
					return err
				}
			case "active":
				if _, err := tx.ExecContext(ctx, `UPDATE users SET status = 'active', failed_logins = 0, locked_until = NULL, updated_by = $2 WHERE id = $1`, id, actor); err != nil {
					return err
				}
				if err := userEventTx(ctx, tx, tenant, id, u.Login, "enabled", nil, actor); err != nil {
					return err
				}
			default:
				return fmt.Errorf("%w: status must be active or disabled", ErrInvalidAccount)
			}
		}
		if in.Admin != nil && *in.Admin != u.Admin {
			action := "admin_granted"
			if !*in.Admin {
				action = "admin_revoked"
				if err := ensureAdminRemainsTx(ctx, tx, tenant, id); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE users SET is_admin = $2, updated_by = $3 WHERE id = $1`, id, *in.Admin, actor); err != nil {
				return err
			}
			if err := userEventTx(ctx, tx, tenant, id, u.Login, action, nil, actor); err != nil {
				return err
			}
		}
		out, err = scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
		return err
	})
	return out, err
}

// SetUserPassword 更换口令哈希并吊销该账号的全部会话。action 为 password_changed（本人）或 password_reset（管理员 / 运维）。
// 运维重置同时解除锁定；被停用的账号不因重置而恢复。
func (d *DB) SetUserPassword(ctx context.Context, tenant string, id int64, hash, action, actor string) error {
	tenant = tenantOr(tenant)
	if hash == "" {
		return fmt.Errorf("%w: password is required", ErrInvalidAccount)
	}
	if action != "password_changed" && action != "password_reset" {
		return fmt.Errorf("%w: unknown password action %q", ErrInvalidAccount, action)
	}
	if actor = strings.TrimSpace(actor); actor == "" {
		actor = "ops"
	}
	return d.InTx(ctx, func(tx *sql.Tx) error {
		u, err := lockUserTx(ctx, tx, tenant, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = $2, password_changed_at = now(), failed_logins = 0,
			locked_until = NULL, updated_by = $3 WHERE id = $1`, id, hash, actor); err != nil {
			return err
		}
		if err := revokeUserSessionsTx(ctx, tx, id, action); err != nil {
			return err
		}
		return userEventTx(ctx, tx, tenant, id, u.Login, action, nil, actor)
	})
}

// LoginFailurePolicy 决定连续失败后的锁定时长：前 Free 次不锁定，之后每次翻倍，封顶 Max。
type LoginFailurePolicy struct {
	Free int
	Base time.Duration
	Max  time.Duration
}

// DefaultLoginFailurePolicy：连续失败 5 次后锁定 30 秒，之后翻倍，最长 15 分钟。
var DefaultLoginFailurePolicy = LoginFailurePolicy{Free: 5, Base: 30 * time.Second, Max: 15 * time.Minute}

// RecordLoginFailure 累加账号的失败次数，并按策略设置 locked_until。返回新的锁定截止时间（未锁定为零值）。
func (d *DB) RecordLoginFailure(ctx context.Context, userID int64, p LoginFailurePolicy) (time.Time, error) {
	var n int
	if err := d.QueryRowContext(ctx, `UPDATE users SET failed_logins = failed_logins + 1 WHERE id = $1 RETURNING failed_logins`, userID).Scan(&n); err != nil {
		return time.Time{}, err
	}
	if n < p.Free {
		return time.Time{}, nil
	}
	lock := p.Base
	for i := p.Free; i < n && lock < p.Max; i++ {
		lock *= 2
	}
	if lock > p.Max {
		lock = p.Max
	}
	var until time.Time
	err := d.QueryRowContext(ctx, `UPDATE users SET locked_until = now() + make_interval(secs => $2) WHERE id = $1 RETURNING locked_until`,
		userID, lock.Seconds()).Scan(&until)
	return until, err
}

// ResetLoginFailures 在登录成功后清零失败计数。
func (d *DB) ResetLoginFailures(ctx context.Context, userID int64) error {
	_, err := d.ExecContext(ctx, `UPDATE users SET failed_logins = 0, locked_until = NULL WHERE id = $1 AND (failed_logins <> 0 OR locked_until IS NOT NULL)`, userID)
	return err
}

// ---------------------------------------------------------------------------
// 会话
// ---------------------------------------------------------------------------

// SessionIdentity 是一次有效会话对应的已核验身份。
type SessionIdentity struct {
	SessionID   int64
	UserID      int64
	Tenant      string
	Login       string
	DisplayName string
	Admin       bool
	ExpiresAt   time.Time
}

// SessionTTL 是会话的空闲有效期与绝对上限。
type SessionTTL struct {
	Idle     time.Duration
	Absolute time.Duration
}

// CreateSession 为已通过口令校验的账号登记会话；tokenHash 是令牌的 SHA-256。
func (d *DB) CreateSession(ctx context.Context, u *User, tokenHash []byte, ttl SessionTTL, addr, userAgent string) (*SessionIdentity, error) {
	if len(tokenHash) != 32 {
		return nil, errors.New("identity: token hash must be 32 bytes")
	}
	if len(userAgent) > 256 {
		userAgent = userAgent[:256]
	}
	out := &SessionIdentity{UserID: u.ID, Tenant: u.TenantID, Login: u.Login, DisplayName: u.DisplayName, Admin: u.Admin}
	err := d.QueryRowContext(ctx, `INSERT INTO auth_sessions(tenant_id, user_id, token_hash, client_addr, user_agent, expires_at, absolute_expires_at)
		SELECT $1, $2, $3, $4, $5, now() + make_interval(secs => $6), now() + make_interval(secs => $7)
		  FROM users WHERE id = $2 AND status = 'active'
		RETURNING id, expires_at`, u.TenantID, u.ID, tokenHash, addr, userAgent, ttl.Idle.Seconds(), ttl.Absolute.Seconds()).Scan(&out.SessionID, &out.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionInvalid
	}
	return out, err
}

// SessionByTokenHash 核验会话：未吊销、未过期、账号可用。空闲期过半时顺延 expires_at（不超过绝对上限）。
func (d *DB) SessionByTokenHash(ctx context.Context, tokenHash []byte, idle time.Duration) (*SessionIdentity, error) {
	if len(tokenHash) != 32 {
		return nil, ErrSessionInvalid
	}
	var s SessionIdentity
	var lastSeen time.Time
	err := d.QueryRowContext(ctx, `SELECT s.id, u.id, u.tenant_id, u.login, u.display_name, u.is_admin, s.expires_at, s.last_seen_at
		FROM auth_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > now() AND s.absolute_expires_at > now()
		  AND u.status = 'active'`, tokenHash).Scan(&s.SessionID, &s.UserID, &s.Tenant, &s.Login, &s.DisplayName, &s.Admin, &s.ExpiresAt, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionInvalid
	}
	if err != nil {
		return nil, err
	}
	if time.Until(s.ExpiresAt) < idle/2 {
		if err := d.QueryRowContext(ctx, `UPDATE auth_sessions SET last_seen_at = now(),
				expires_at = LEAST(now() + make_interval(secs => $2), absolute_expires_at)
			WHERE id = $1 AND revoked_at IS NULL RETURNING expires_at`, s.SessionID, idle.Seconds()).Scan(&s.ExpiresAt); err != nil &&
			!errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return &s, nil
}

// RevokeSession 吊销一个会话（登出）。
func (d *DB) RevokeSession(ctx context.Context, sessionID int64, reason string) error {
	_, err := d.ExecContext(ctx, `UPDATE auth_sessions SET revoked_at = now(), revoke_reason = $2 WHERE id = $1 AND revoked_at IS NULL`, sessionID, reason)
	return err
}

func revokeUserSessionsTx(ctx context.Context, q queryer, userID int64, reason string) error {
	_, err := q.ExecContext(ctx, `UPDATE auth_sessions SET revoked_at = now(), revoke_reason = $2 WHERE user_id = $1 AND revoked_at IS NULL`, userID, reason)
	return err
}

// PurgeExpiredSessions 删除已过期或吊销超过 retain 的会话行（只是清理，不影响判定）。
func (d *DB) PurgeExpiredSessions(ctx context.Context, retain time.Duration) (int64, error) {
	res, err := d.ExecContext(ctx, `DELETE FROM auth_sessions
		WHERE COALESCE(revoked_at, LEAST(expires_at, absolute_expires_at)) < now() - make_interval(secs => $1)`, retain.Seconds())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// 项目成员与角色
// ---------------------------------------------------------------------------

// ProjectMember 是项目成员视图。
type ProjectMember struct {
	TaskID      int64     `json:"task_id"`
	UserID      int64     `json:"user_id"`
	Login       string    `json:"login"`
	DisplayName string    `json:"display_name"`
	Status      string    `json:"status"`
	Roles       []string  `json:"roles"`
	CreatedBy   string    `json:"created_by"`
	UpdatedBy   string    `json:"updated_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// NormalizeRoles 去重、按 AllRoles 排序并校验角色。
func NormalizeRoles(roles []string) ([]string, error) {
	out := []string{}
	for _, r := range AllRoles {
		for _, x := range roles {
			if strings.TrimSpace(x) == r {
				out = append(out, r)
				break
			}
		}
	}
	for _, x := range roles {
		if !ValidRole(strings.TrimSpace(x)) {
			return nil, fmt.Errorf("%w: unknown role %q", ErrInvalidAccount, x)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: at least one role is required", ErrInvalidAccount)
	}
	return out, nil
}

func decodeRoles(raw []byte) []string {
	var out []string
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = []string{}
	}
	return out
}

// MemberRoles 返回账号在项目中的角色；非成员返回空切片。
func (d *DB) MemberRoles(ctx context.Context, tenant string, taskID, userID int64) ([]string, error) {
	var raw []byte
	err := d.QueryRowContext(ctx, `SELECT array_to_json(roles) FROM project_members WHERE tenant_id = $1 AND task_id = $2 AND user_id = $3`,
		tenantOr(tenant), taskID, userID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeRoles(raw), nil
}

// HasRoleInAnyProject 判断账号是否在任一项目中担任 roles 之一（企业资料库等租户级资源的写入权限）。
func (d *DB) HasRoleInAnyProject(ctx context.Context, tenant string, userID int64, roles []string) (bool, error) {
	var ok bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM project_members m JOIN tasks t ON t.id = m.task_id
		WHERE m.tenant_id = $1 AND m.user_id = $2 AND m.roles && $3::text[])`, tenantOr(tenant), userID, roles).Scan(&ok)
	return ok, err
}

// MemberTaskIDs 返回账号所属的全部项目 ID。
func (d *DB) MemberTaskIDs(ctx context.Context, tenant string, userID int64) (map[int64][]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT task_id, array_to_json(roles) FROM project_members WHERE tenant_id = $1 AND user_id = $2`, tenantOr(tenant), userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		out[id] = decodeRoles(raw)
	}
	return out, rows.Err()
}

// ListProjectMembers 列出项目成员。
func (d *DB) ListProjectMembers(ctx context.Context, tenant string, taskID int64) ([]*ProjectMember, error) {
	rows, err := d.QueryContext(ctx, `SELECT m.task_id, m.user_id, u.login, u.display_name, u.status, array_to_json(m.roles),
			m.created_by, m.updated_by, m.created_at, m.updated_at
		FROM project_members m JOIN users u ON u.id = m.user_id
		WHERE m.tenant_id = $1 AND m.task_id = $2 ORDER BY u.login`, tenantOr(tenant), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ProjectMember{}
	for rows.Next() {
		var m ProjectMember
		var raw []byte
		if err := rows.Scan(&m.TaskID, &m.UserID, &m.Login, &m.DisplayName, &m.Status, &raw, &m.CreatedBy, &m.UpdatedBy, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		m.Roles = decodeRoles(raw)
		out = append(out, &m)
	}
	return out, rows.Err()
}

func memberEventTx(ctx context.Context, q queryer, tenant string, taskID, userID int64, login, action string, before, after []string, actor string) error {
	if before == nil {
		before = []string{}
	}
	if after == nil {
		after = []string{}
	}
	_, err := q.ExecContext(ctx, `INSERT INTO project_member_events(tenant_id, task_id, user_id, login, action, roles_before, roles_after, created_by)
		VALUES ($1, $2, $3, $4, $5, $6::text[], $7::text[], $8)`, tenant, taskID, userID, login, action, before, after, actor)
	return err
}

func addOwnerTx(ctx context.Context, tx *sql.Tx, tenant string, taskID int64, owner string) error {
	var uid int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE tenant_id = $1 AND login = $2 AND status = 'active'`, tenant, owner).Scan(&uid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: owner account %q", ErrNotFound, owner)
		}
		return err
	}
	roles := []string{RoleBidManager}
	if _, err := tx.ExecContext(ctx, `INSERT INTO project_members(tenant_id, task_id, user_id, roles, created_by, updated_by)
		VALUES ($1, $2, $3, $4::text[], $5, $5)`, tenant, taskID, uid, roles, owner); err != nil {
		return err
	}
	return memberEventTx(ctx, tx, tenant, taskID, uid, owner, "added", nil, roles, owner)
}

// ensureManagerRemainsTx 在移除 / 降级成员前确认项目仍有一名可用的投标经理（锁住全部经理行）。
func ensureManagerRemainsTx(ctx context.Context, tx *sql.Tx, tenant string, taskID, excludingUser int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT m.id FROM project_members m JOIN users u ON u.id = m.user_id
		WHERE m.tenant_id = $1 AND m.task_id = $2 AND m.user_id <> $3 AND 'bid_manager' = ANY(m.roles) AND u.status = 'active'
		FOR UPDATE OF m`, tenant, taskID, excludingUser)
	if err != nil {
		return err
	}
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if n == 0 {
		return ErrLastManager
	}
	return nil
}

// SetProjectMember 添加成员或替换其角色（整组替换）。
func (d *DB) SetProjectMember(ctx context.Context, tenant string, taskID int64, login string, roles []string, actor string) (*ProjectMember, error) {
	tenant = tenantOr(tenant)
	roles, err := NormalizeRoles(roles)
	if err != nil {
		return nil, err
	}
	err = d.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT 1 FROM tasks WHERE id = $1 FOR SHARE`, taskID); err != nil {
			return err
		}
		var uid int64
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT id, status FROM users WHERE tenant_id = $1 AND login = $2`, tenant,
			strings.ToLower(strings.TrimSpace(login))).Scan(&uid, &status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: account %q", ErrNotFound, login)
			}
			return err
		}
		var raw []byte
		err := tx.QueryRowContext(ctx, `SELECT array_to_json(roles) FROM project_members WHERE tenant_id = $1 AND task_id = $2 AND user_id = $3 FOR UPDATE`,
			tenant, taskID, uid).Scan(&raw)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if status != "active" {
				return fmt.Errorf("%w: account %q is disabled", ErrInvalidAccount, login)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO project_members(tenant_id, task_id, user_id, roles, created_by, updated_by)
				VALUES ($1, $2, $3, $4::text[], $5, $5)`, tenant, taskID, uid, roles, actor); err != nil {
				return err
			}
			return memberEventTx(ctx, tx, tenant, taskID, uid, login, "added", nil, roles, actor)
		case err != nil:
			return err
		}
		before := decodeRoles(raw)
		if slices.Equal(before, roles) {
			return nil
		}
		if slices.Contains(before, RoleBidManager) && !slices.Contains(roles, RoleBidManager) {
			if err := ensureManagerRemainsTx(ctx, tx, tenant, taskID, uid); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE project_members SET roles = $4::text[], updated_by = $5
			WHERE tenant_id = $1 AND task_id = $2 AND user_id = $3`, tenant, taskID, uid, roles, actor); err != nil {
			return err
		}
		return memberEventTx(ctx, tx, tenant, taskID, uid, login, "roles_changed", before, roles, actor)
	})
	if err != nil {
		return nil, err
	}
	members, err := d.ListProjectMembers(ctx, tenant, taskID)
	if err != nil {
		return nil, err
	}
	for _, m := range members {
		if m.Login == strings.ToLower(strings.TrimSpace(login)) {
			return m, nil
		}
	}
	return nil, ErrNotFound
}

// RemoveProjectMember 移除成员；不能移除最后一名可用的投标经理。
func (d *DB) RemoveProjectMember(ctx context.Context, tenant string, taskID int64, login, actor string) error {
	tenant = tenantOr(tenant)
	return d.InTx(ctx, func(tx *sql.Tx) error {
		var uid int64
		var raw []byte
		err := tx.QueryRowContext(ctx, `SELECT m.user_id, array_to_json(m.roles) FROM project_members m JOIN users u ON u.id = m.user_id
			WHERE m.tenant_id = $1 AND m.task_id = $2 AND u.login = $3 FOR UPDATE OF m`, tenant, taskID, strings.ToLower(strings.TrimSpace(login))).Scan(&uid, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: member %q", ErrNotFound, login)
		}
		if err != nil {
			return err
		}
		before := decodeRoles(raw)
		if slices.Contains(before, RoleBidManager) {
			if err := ensureManagerRemainsTx(ctx, tx, tenant, taskID, uid); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM project_members WHERE tenant_id = $1 AND task_id = $2 AND user_id = $3`, tenant, taskID, uid); err != nil {
			return err
		}
		return memberEventTx(ctx, tx, tenant, taskID, uid, strings.ToLower(strings.TrimSpace(login)), "removed", before, nil, actor)
	})
}

// MemberEvent 是成员变更审计记录。
type MemberEvent struct {
	ID          int64     `json:"id"`
	TaskID      int64     `json:"task_id"`
	UserID      int64     `json:"user_id"`
	Login       string    `json:"login"`
	Action      string    `json:"action"`
	RolesBefore []string  `json:"roles_before"`
	RolesAfter  []string  `json:"roles_after"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

// ListMemberEvents 列出项目的成员变更审计（新到旧）。
func (d *DB) ListMemberEvents(ctx context.Context, tenant string, taskID int64, limit int) ([]*MemberEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := d.QueryContext(ctx, `SELECT id, task_id, user_id, login, action, array_to_json(roles_before), array_to_json(roles_after), created_by, created_at
		FROM project_member_events WHERE tenant_id = $1 AND task_id = $2 ORDER BY id DESC LIMIT $3`, tenantOr(tenant), taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*MemberEvent{}
	for rows.Next() {
		var e MemberEvent
		var before, after []byte
		if err := rows.Scan(&e.ID, &e.TaskID, &e.UserID, &e.Login, &e.Action, &before, &after, &e.CreatedBy, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.RolesBefore, e.RolesAfter = decodeRoles(before), decodeRoles(after)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// UserEvent 是账号审计记录。
type UserEvent struct {
	ID        int64           `json:"id"`
	UserID    *int64          `json:"user_id,omitempty"`
	Login     string          `json:"login"`
	Action    string          `json:"action"`
	Detail    json.RawMessage `json:"detail"`
	CreatedBy string          `json:"created_by"`
	CreatedAt time.Time       `json:"created_at"`
}

// ListUserEvents 列出账号审计（新到旧）；userID 为 0 时列出全部。
func (d *DB) ListUserEvents(ctx context.Context, tenant string, userID int64, limit int) ([]*UserEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := d.QueryContext(ctx, `SELECT id, user_id, login, action, detail, created_by, created_at FROM user_events
		WHERE tenant_id = $1 AND ($2 = 0 OR user_id = $2) ORDER BY id DESC LIMIT $3`, tenantOr(tenant), userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*UserEvent{}
	for rows.Next() {
		var e UserEvent
		var uid sql.NullInt64
		var detail []byte
		if err := rows.Scan(&e.ID, &uid, &e.Login, &e.Action, &detail, &e.CreatedBy, &e.CreatedAt); err != nil {
			return nil, err
		}
		if uid.Valid {
			e.UserID = &uid.Int64
		}
		e.Detail = json.RawMessage(detail)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 遗留姓名：升级前写入职责分离判定列的声明式姓名
// ---------------------------------------------------------------------------

// LegacyActor 是一个升级前出现过的声明式姓名及其映射状态。
type LegacyActor struct {
	Name        string `json:"name"`
	Storyboards int    `json:"storyboards"`
	Revisions   int    `json:"revisions"`
	Commitments int    `json:"commitments"`
	MappedTo    string `json:"mapped_to,omitempty"`
	MappedBy    string `json:"mapped_by,omitempty"`
}

// stripAIPrefix 把 AI 代写的作者 "ai:<派发人>" 还原为派发人：派发人对 AI 稿负责，算作者。
func stripAIPrefix(name string) string {
	name = strings.TrimSpace(name)
	if rest, ok := strings.CutPrefix(name, "ai:"); ok {
		return strings.TrimSpace(rest)
	}
	return name
}

// SeparationPrincipal 把职责分离判定列中的记录还原为"是谁"：AI 代写算派发人；
// 升级前的声明式姓名（legacy）只有经管理员映射到账号后才算数，否则返回 ErrLegacyActor。空姓名返回空串。
func separationPrincipalQ(ctx context.Context, q queryer, tenant, name string, legacy bool) (string, error) {
	name = stripAIPrefix(name)
	if name == "" || !legacy {
		return name, nil
	}
	var login string
	err := q.QueryRowContext(ctx, `SELECT u.login FROM legacy_actor_mappings m JOIN users u ON u.id = m.user_id
		WHERE m.tenant_id = $1 AND m.legacy_name = $2`, tenant, name).Scan(&login)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w (declared name %q)", ErrLegacyActor, name)
	}
	return login, err
}

// ListLegacyActors 列出职责分离判定列中出现过的全部声明式姓名（含 AI 代写的派发人）与映射状态。
func (d *DB) ListLegacyActors(ctx context.Context, tenant string) ([]*LegacyActor, error) {
	tenant = tenantOr(tenant)
	rows, err := d.QueryContext(ctx, `WITH names AS (
			SELECT regexp_replace(btrim(created_by), '^ai:\s*', '') AS name, 'storyboard' AS kind FROM section_storyboards WHERE tenant_id = $1 AND actor_legacy
			UNION ALL SELECT btrim(submitted_by), 'storyboard' FROM section_storyboards WHERE tenant_id = $1 AND actor_legacy AND btrim(submitted_by) <> ''
			UNION ALL SELECT regexp_replace(btrim(created_by), '^ai:\s*', ''), 'revision' FROM section_revisions WHERE tenant_id = $1 AND actor_legacy
			UNION ALL SELECT btrim(created_by), 'commitment' FROM commitment_ledger WHERE tenant_id = $1 AND actor_legacy
		)
		SELECT n.name,
			count(*) FILTER (WHERE n.kind = 'storyboard'), count(*) FILTER (WHERE n.kind = 'revision'), count(*) FILTER (WHERE n.kind = 'commitment'),
			COALESCE(u.login, ''), COALESCE(m.created_by, '')
		FROM names n
		LEFT JOIN legacy_actor_mappings m ON m.tenant_id = $1 AND m.legacy_name = n.name
		LEFT JOIN users u ON u.id = m.user_id
		WHERE n.name <> ''
		GROUP BY n.name, u.login, m.created_by ORDER BY n.name`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*LegacyActor{}
	for rows.Next() {
		var a LegacyActor
		if err := rows.Scan(&a.Name, &a.Storyboards, &a.Revisions, &a.Commitments, &a.MappedTo, &a.MappedBy); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// MapLegacyActor 由管理员声明某个升级前的姓名就是某个账号；映射写入 user_events（记在目标账号下）。已映射的姓名不能改映射。
func (d *DB) MapLegacyActor(ctx context.Context, tenant, legacyName, login, actor string) error {
	tenant = tenantOr(tenant)
	legacyName = stripAIPrefix(legacyName)
	if legacyName == "" {
		return fmt.Errorf("%w: legacy name is required", ErrInvalidAccount)
	}
	return d.InTx(ctx, func(tx *sql.Tx) error {
		u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE tenant_id = $1 AND login = $2`, tenant, strings.ToLower(strings.TrimSpace(login))))
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO legacy_actor_mappings(tenant_id, legacy_name, user_id, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $4) ON CONFLICT (tenant_id, legacy_name) DO NOTHING`, tenant, legacyName, u.ID, actor)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: legacy name %q is already mapped", ErrAccountConflict, legacyName)
		}
		return userEventTx(ctx, tx, tenant, u.ID, u.Login, "legacy_name_mapped", map[string]any{"legacy_name": legacyName}, actor)
	})
}

// UnmapLegacyActor 撤销映射（之后该姓名重新不参与判定）；已据此作出的批准不回溯撤销。
func (d *DB) UnmapLegacyActor(ctx context.Context, tenant, legacyName, actor string) error {
	tenant = tenantOr(tenant)
	legacyName = stripAIPrefix(legacyName)
	return d.InTx(ctx, func(tx *sql.Tx) error {
		var uid int64
		var login string
		err := tx.QueryRowContext(ctx, `DELETE FROM legacy_actor_mappings m USING users u
			WHERE m.tenant_id = $1 AND m.legacy_name = $2 AND u.id = m.user_id RETURNING u.id, u.login`, tenant, legacyName).Scan(&uid, &login)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: legacy name %q is not mapped", ErrNotFound, legacyName)
		}
		if err != nil {
			return err
		}
		return userEventTx(ctx, tx, tenant, uid, login, "legacy_name_unmapped", map[string]any{"legacy_name": legacyName}, actor)
	})
}

// ---------------------------------------------------------------------------
// 质量策略
// ---------------------------------------------------------------------------

// QualityPolicyEvent 是质量策略变更审计记录。
type QualityPolicyEvent struct {
	ID         int64     `json:"id"`
	TaskID     int64     `json:"task_id"`
	FromPolicy string    `json:"from_policy"`
	ToPolicy   string    `json:"to_policy"`
	Reason     string    `json:"reason"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
}

// taskQualityPolicyQ 读取任务的质量策略（D3 门禁的唯一判据）。
func taskQualityPolicyQ(ctx context.Context, q queryer, taskID int64) (string, error) {
	var p string
	err := q.QueryRowContext(ctx, `SELECT quality_policy FROM tasks WHERE id = $1`, taskID).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: task %d", ErrNotFound, taskID)
	}
	return p, err
}

// SetQualityPolicy 修改任务的质量策略并留痕。已有故事板或写作核验记录的任务不能降回 legacy/v1：
// legacy/v1 下不存在任何 D3 记录，门禁不再需要从"有没有记录"推断。
func (d *DB) SetQualityPolicy(ctx context.Context, tenant string, taskID int64, policy, reason, actor string) (*Task, error) {
	tenant = tenantOr(tenant)
	if policy != QualityPolicyGrounded && policy != QualityPolicyLegacy {
		return nil, fmt.Errorf("%w: %q", ErrInvalidPolicy, policy)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, fmt.Errorf("%w: a reason is required", ErrInvalidPolicy)
	}
	var out *Task
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		var cur string
		if err := tx.QueryRowContext(ctx, `SELECT quality_policy FROM tasks WHERE id = $1 FOR UPDATE`, taskID).Scan(&cur); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: task %d", ErrNotFound, taskID)
			}
			return err
		}
		if cur != policy {
			if policy == QualityPolicyLegacy {
				var has bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM section_storyboards WHERE task_id = $1)
					OR EXISTS (SELECT 1 FROM section_writing_verifications WHERE task_id = $1)`, taskID).Scan(&has); err != nil {
					return err
				}
				if has {
					return fmt.Errorf("%w: task %d already has storyboards or writing verifications; it cannot go back to %s", ErrPolicyConflict, taskID, QualityPolicyLegacy)
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET quality_policy = $2 WHERE id = $1`, taskID, policy); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO quality_policy_events(tenant_id, task_id, from_policy, to_policy, reason, created_by)
				VALUES ($1, $2, $3, $4, $5, $6)`, tenant, taskID, cur, policy, reason, actor); err != nil {
				return err
			}
		}
		t, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = $1`, taskID))
		out = t
		return err
	})
	return out, err
}

// ListQualityPolicyEvents 列出任务的质量策略变更（新到旧）。
func (d *DB) ListQualityPolicyEvents(ctx context.Context, tenant string, taskID int64) ([]*QualityPolicyEvent, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, task_id, from_policy, to_policy, reason, created_by, created_at
		FROM quality_policy_events WHERE tenant_id = $1 AND task_id = $2 ORDER BY id DESC`, tenantOr(tenant), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*QualityPolicyEvent{}
	for rows.Next() {
		var e QualityPolicyEvent
		if err := rows.Scan(&e.ID, &e.TaskID, &e.FromPolicy, &e.ToPolicy, &e.Reason, &e.CreatedBy, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}
