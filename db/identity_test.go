package db

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"
)

func r2User(t *testing.T, d *DB, tenant, login string, admin bool) *User {
	t.Helper()
	u, err := d.CreateUser(context.Background(), tenant, NewUser{Login: login, DisplayName: login + "（测试）", PasswordHash: "$argon2id$test", Admin: admin}, "ops")
	if err != nil {
		t.Fatalf("create %s: %v", login, err)
	}
	return u
}

func tokenHash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

var r2TTL = SessionTTL{Idle: time.Hour, Absolute: 2 * time.Hour}

// R2 性质 4 / 7 / 9：账号标识不可变、会话只存令牌哈希且停用 / 改口令 / 登出立即失效、变更写只追加事件。
func TestR2AccountsSessionsAndEvents(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	tenant := uniq("r2-acct")
	admin := r2User(t, d, tenant, "admin1", true)
	alice := r2User(t, d, tenant, "alice", false)

	if _, err := d.CreateUser(ctx, tenant, NewUser{Login: "Alice", PasswordHash: "x"}, "admin1"); !errors.Is(err, ErrAccountConflict) {
		t.Fatalf("duplicate login (case-insensitive) must conflict, got %v", err)
	}
	for _, bad := range []string{"", "a", "张三", "has space", "-lead"} {
		if _, err := d.CreateUser(ctx, tenant, NewUser{Login: bad, PasswordHash: "x"}, "admin1"); !errors.Is(err, ErrInvalidAccount) {
			t.Fatalf("login %q must be rejected, got %v", bad, err)
		}
	}
	// 登录名不可修改（数据库触发器），显示名可以。
	if _, err := d.ExecContext(ctx, `UPDATE users SET login = 'mallory' WHERE id = $1`, alice.ID); err == nil {
		t.Fatal("users.login must be immutable")
	}
	name := "Alice Wang"
	if u, err := d.UpdateUser(ctx, tenant, alice.ID, UserUpdate{DisplayName: &name}, "admin1"); err != nil || u.DisplayName != name || u.Login != "alice" {
		t.Fatalf("display name: %+v %v", u, err)
	}

	// 会话：只存哈希；登出、停用、改口令后立即失效。
	s1, err := d.CreateSession(ctx, alice, tokenHash(tenant+"tok-1"), r2TTL, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := d.SessionByTokenHash(ctx, tokenHash(tenant+"tok-1"), r2TTL.Idle); err != nil || got.Login != "alice" || got.UserID != alice.ID {
		t.Fatalf("session lookup: %+v %v", got, err)
	}
	var stored int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM auth_sessions WHERE user_id = $1 AND position(convert_to($2,'UTF8') in token_hash) > 0`, alice.ID, tenant+"tok-1").Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("raw token must not be stored: %d %v", stored, err)
	}
	if err := d.RevokeSession(ctx, s1.SessionID, "logout"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SessionByTokenHash(ctx, tokenHash(tenant+"tok-1"), r2TTL.Idle); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("logged-out session must be invalid, got %v", err)
	}
	if _, err := d.CreateSession(ctx, alice, tokenHash(tenant+"tok-2"), r2TTL, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.SetUserPassword(ctx, tenant, alice.ID, "$argon2id$new", "password_changed", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SessionByTokenHash(ctx, tokenHash(tenant+"tok-2"), r2TTL.Idle); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("password change must revoke sessions, got %v", err)
	}
	if _, err := d.CreateSession(ctx, alice, tokenHash(tenant+"tok-3"), r2TTL, "", ""); err != nil {
		t.Fatal(err)
	}
	disabled := "disabled"
	if _, err := d.UpdateUser(ctx, tenant, alice.ID, UserUpdate{Status: &disabled}, "admin1"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SessionByTokenHash(ctx, tokenHash(tenant+"tok-3"), r2TTL.Idle); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("disabled account's session must be invalid, got %v", err)
	}
	if _, err := d.CreateSession(ctx, alice, tokenHash(tenant+"tok-4"), r2TTL, "", ""); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("disabled account must not get a session, got %v", err)
	}
	// 过期：直接把到期时间改到过去。
	bob := r2User(t, d, tenant, "bob", false)
	s5, err := d.CreateSession(ctx, bob, tokenHash(tenant+"tok-5"), r2TTL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE auth_sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, s5.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SessionByTokenHash(ctx, tokenHash(tenant+"tok-5"), r2TTL.Idle); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("expired session must be invalid, got %v", err)
	}

	// 至少保留一名可用管理员。
	no := false
	if _, err := d.UpdateUser(ctx, tenant, admin.ID, UserUpdate{Admin: &no}, "admin1"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("last admin must stay, got %v", err)
	}
	if _, err := d.UpdateUser(ctx, tenant, admin.ID, UserUpdate{Status: &disabled}, "admin1"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("last admin cannot be disabled, got %v", err)
	}

	// 登录失败锁定：前 Free 次不锁，之后锁定并翻倍。
	pol := LoginFailurePolicy{Free: 2, Base: time.Minute, Max: 4 * time.Minute}
	for i := 1; i <= 4; i++ {
		until, err := d.RecordLoginFailure(ctx, bob.ID, pol)
		if err != nil {
			t.Fatal(err)
		}
		if (i < 2) != until.IsZero() {
			t.Fatalf("failure %d: locked until %v", i, until)
		}
	}
	if err := d.ResetLoginFailures(ctx, bob.ID); err != nil {
		t.Fatal(err)
	}

	// 事件只追加。
	ev, err := d.ListUserEvents(ctx, tenant, alice.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"created": false, "display_name_changed": false, "password_changed": false, "disabled": false}
	for _, e := range ev {
		if _, ok := want[e.Action]; ok {
			want[e.Action] = true
		}
	}
	for k, ok := range want {
		if !ok {
			t.Fatalf("missing user event %s in %+v", k, ev)
		}
	}
	if _, err := d.ExecContext(ctx, `UPDATE user_events SET action = 'created' WHERE tenant_id = $1`, tenant); err == nil {
		t.Fatal("user_events must be append-only (update)")
	}
	if _, err := d.ExecContext(ctx, `DELETE FROM user_events WHERE tenant_id = $1`, tenant); err == nil {
		t.Fatal("user_events must be append-only (delete)")
	}
}

// R2 性质 6 / 9：项目成员与角色、最后一名投标经理、成员事件只追加。
func TestR2ProjectMembers(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	tenant := uniq("r2-mem")
	r2User(t, d, tenant, "lead", false)
	writer := r2User(t, d, tenant, "writer", false)
	task, err := d.CreateTask(ctx, CreateTaskInput{Name: uniq("r2-members"), Goal: "成员测试", Owner: "lead", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	if task.QualityPolicy != QualityPolicyGrounded || !task.GroundedWritingRequired {
		t.Fatalf("new task policy: %s", task.QualityPolicy)
	}
	members, err := d.ListProjectMembers(ctx, tenant, task.ID)
	if err != nil || len(members) != 1 || members[0].Login != "lead" || members[0].Roles[0] != RoleBidManager {
		t.Fatalf("owner membership: %+v %v", members, err)
	}
	if _, err := d.SetProjectMember(ctx, tenant, task.ID, "writer", []string{RoleReviewer, RoleTechWriter, RoleTechWriter}, "lead"); err != nil {
		t.Fatal(err)
	}
	roles, err := d.MemberRoles(ctx, tenant, task.ID, writer.ID)
	if err != nil || fmt.Sprint(roles) != "[tech_writer reviewer]" {
		t.Fatalf("roles normalised: %v %v", roles, err)
	}
	if ok, _ := d.HasRoleInAnyProject(ctx, tenant, writer.ID, []string{RoleCommercial}); ok {
		t.Fatal("writer is not commercial anywhere")
	}
	if _, err := d.SetProjectMember(ctx, tenant, task.ID, "writer", []string{"owner"}, "lead"); !errors.Is(err, ErrInvalidAccount) {
		t.Fatalf("unknown role must be refused, got %v", err)
	}
	if _, err := d.SetProjectMember(ctx, tenant, task.ID, "lead", []string{RoleTechWriter}, "lead"); !errors.Is(err, ErrLastManager) {
		t.Fatalf("demoting the last manager must fail, got %v", err)
	}
	if err := d.RemoveProjectMember(ctx, tenant, task.ID, "lead", "lead"); !errors.Is(err, ErrLastManager) {
		t.Fatalf("removing the last manager must fail, got %v", err)
	}
	if _, err := d.SetProjectMember(ctx, tenant, task.ID, "writer", []string{RoleBidManager}, "lead"); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveProjectMember(ctx, tenant, task.ID, "lead", "writer"); err != nil {
		t.Fatalf("a second manager allows removal: %v", err)
	}
	ev, err := d.ListMemberEvents(ctx, tenant, task.ID, 50)
	if err != nil || len(ev) != 4 {
		t.Fatalf("member events: %d %v", len(ev), err)
	}
	if ev[0].Action != "removed" || ev[0].Login != "lead" || ev[0].CreatedBy != "writer" {
		t.Fatalf("latest event: %+v", ev[0])
	}
	if _, err := d.ExecContext(ctx, `DELETE FROM project_member_events WHERE task_id = $1`, task.ID); err == nil {
		t.Fatal("project_member_events must be append-only")
	}
	pe, err := d.ListQualityPolicyEvents(ctx, tenant, task.ID)
	if err != nil || len(pe) != 1 || pe[0].ToPolicy != QualityPolicyGrounded || pe[0].CreatedBy != "lead" {
		t.Fatalf("creation policy event: %+v %v", pe, err)
	}
}

// R2 性质 5：职责分离按账号判定；升级前的声明式姓名不算"另一个人"，AI 代写算派发人。
func TestR2SeparationPrincipals(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	tenant := uniq("r2-sep")
	r2User(t, d, tenant, "zhangsan", false)
	for _, tc := range []struct {
		name   string
		legacy bool
		want   string
		err    error
	}{
		{"alice", false, "alice", nil},
		{"ai:alice", false, "alice", nil},
		{"", true, "", nil},
		{"张三", true, "", ErrLegacyActor},
		{"zhangsan", true, "", ErrLegacyActor}, // 与账号同名的旧声明也不算数，除非显式映射
	} {
		got, err := separationPrincipalQ(ctx, d, tenant, tc.name, tc.legacy)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Fatalf("%q legacy=%v: got %q %v", tc.name, tc.legacy, got, err)
		}
	}
	if err := d.MapLegacyActor(ctx, tenant, "张三", "zhangsan", "admin1"); err != nil {
		t.Fatal(err)
	}
	if err := d.MapLegacyActor(ctx, tenant, "张三", "zhangsan", "admin1"); !errors.Is(err, ErrAccountConflict) {
		t.Fatalf("a legacy name maps once, got %v", err)
	}
	if got, err := separationPrincipalQ(ctx, d, tenant, "ai:张三", true); err != nil || got != "zhangsan" {
		t.Fatalf("mapped legacy AI author: %q %v", got, err)
	}
	if err := d.UnmapLegacyActor(ctx, tenant, "张三", "admin1"); err != nil {
		t.Fatal(err)
	}
	if _, err := separationPrincipalQ(ctx, d, tenant, "张三", true); !errors.Is(err, ErrLegacyActor) {
		t.Fatalf("unmapped again: %v", err)
	}
	ev, _ := d.ListUserEvents(ctx, tenant, 0, 50)
	var mapped, unmapped bool
	for _, e := range ev {
		mapped = mapped || e.Action == "legacy_name_mapped"
		unmapped = unmapped || e.Action == "legacy_name_unmapped"
	}
	if !mapped || !unmapped {
		t.Fatalf("mapping events: %+v", ev)
	}
}

// R2 性质 5 + 质量策略：粉队 / 红队 / 承诺会签的职责分离在存储层按账号判定。
func TestR2SeparationOfDutiesInStorage(t *testing.T) {
	f, tenant, _, _ := d2Setup(t)
	d := f.d
	ctx := context.Background()
	r2User(t, d, tenant, "alice", false)

	// 质量策略：legacy/v1 → grounded/v1 留痕；有 D3 记录后不能降回。
	if _, err := d.SetQualityPolicy(ctx, tenant, f.task.ID, "strict/v9", "x", "lead"); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("unknown policy: %v", err)
	}
	if _, err := d.SetQualityPolicy(ctx, tenant, f.task.ID, QualityPolicyGrounded, "", "lead"); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("reason required: %v", err)
	}
	if task, err := d.SetQualityPolicy(ctx, tenant, f.task.ID, QualityPolicyGrounded, "启用落证写作", "lead"); err != nil || task.QualityPolicy != QualityPolicyGrounded {
		t.Fatalf("switch policy: %v", err)
	}

	// 红队：AI 代写的修订（作者 ai:alice）不能由派发人 alice 批准；声明式旧作者未映射时不能参与判定。
	s := &ProposalSection{TaskID: f.task.ID, ID: -1, Code: "X"}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := d3CheckApproveSectionRevisionTx(ctx, tx, tenant, s, &SectionRevision{CreatedBy: "ai:alice"}, "alice"); !errors.Is(err, ErrRedSelfApproval) {
		t.Fatalf("dispatcher approving own AI draft: %v", err)
	}
	if err := d3CheckApproveSectionRevisionTx(ctx, tx, tenant, s, &SectionRevision{CreatedBy: "张三", ActorLegacy: true}, "alice"); !errors.Is(err, ErrLegacyActor) {
		t.Fatalf("legacy author: %v", err)
	}
	if err := d.MapLegacyActor(ctx, tenant, "张三", "alice", "admin1"); err != nil {
		t.Fatal(err)
	}
	if err := d3CheckApproveSectionRevisionTx(ctx, tx, tenant, s, &SectionRevision{CreatedBy: "张三", ActorLegacy: true}, "alice"); !errors.Is(err, ErrRedSelfApproval) {
		t.Fatalf("mapped legacy author approving own revision: %v", err)
	}
	if err := d3CheckApproveSectionRevisionTx(ctx, tx, tenant, s, &SectionRevision{CreatedBy: "ai:bob"}, "alice"); errors.Is(err, ErrRedSelfApproval) || errors.Is(err, ErrLegacyActor) {
		t.Fatalf("a different reviewer passes the separation check (later gates may still refuse): %v", err)
	}
	_ = tx.Rollback()

	// 承诺会签：声明式旧创建人未映射时拒绝；映射后按账号判定。
	th, cv := 90.0, 80.0
	if _, err := d.UpsertCommitment(ctx, tenant, f.task.ID, CommitmentInput{Code: "C-R2", Metric: MetricDuration, MetricLabel: "总工期", Unit: "日历天",
		ThresholdCmp: "<=", TenderThresholdRaw: "不超过90日历天", TenderThresholdValue: &th, CommittedRaw: "80日历天", CommittedValue: &cv,
		Owner: "项目经理", Rationale: "R2"}, "carol"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO commitment_ledger(tenant_id, task_id, code, metric, metric_label, unit, threshold_cmp,
			tender_threshold_raw, tender_threshold_value, committed_raw, committed_value, owner, rationale, status, created_by, updated_by, actor_legacy)
		SELECT tenant_id, task_id, 'C-R2-OLD', metric, metric_label, unit, threshold_cmp, tender_threshold_raw, tender_threshold_value,
			committed_raw, committed_value, owner, rationale, status, '李四', '李四', true
		FROM commitment_ledger WHERE tenant_id = $1 AND task_id = $2 AND code = 'C-R2'`, tenant, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SignOffCommitment(ctx, tenant, f.task.ID, "C-R2-OLD", "alice", ""); !errors.Is(err, ErrLegacyActor) {
		t.Fatalf("legacy creator sign-off: %v", err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE commitment_ledger SET actor_legacy = true WHERE tenant_id = $1 AND task_id = $2 AND code = 'C-R2'`, tenant, f.task.ID); err == nil {
		t.Fatal("actor_legacy must not be set again on a verified record")
	}
	if err := d.MapLegacyActor(ctx, tenant, "李四", "alice", "admin1"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SignOffCommitment(ctx, tenant, f.task.ID, "C-R2-OLD", "alice", ""); !errors.Is(err, ErrCommitmentSignoffRequired) {
		t.Fatalf("mapped legacy creator signing off own commitment: %v", err)
	}
}
