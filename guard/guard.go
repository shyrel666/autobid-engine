// Package guard 是挂在 harness PreToolUse / PostToolUse 上的控制面适配层（§6.2）：
// Pre 把启用拦截的工具交给 Interceptor 判定（拒绝变成当前工具的错误结果，模型可调整，不取消整个 Worker）；
// Post 维护执行结果归因计数，并回填经人工审批放行调用的实际执行结果。Guard 不注册 Stop 语义。
package guard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/autobid/autobid-engine/intercept"
	"github.com/autobid/autobid-engine/internal/llm"
)

// maxAudit 是内存审计列表上限（§6.2：最多保留 2000 条）。
const maxAudit = 2000

// AuditEntry 是内存审计条目，仅供观察；审批证明以 intercept_audit / intercept_pending 为准。
type AuditEntry struct {
	At        time.Time
	Tool      string
	Digest    string
	Decision  string // allow / deny / ask:allow / ask:deny；未启用拦截的工具记 pass
	Source    string
	PendingID int64
	Message   string
}

// Guard 实现 harness.HookRunner。蓝图中 Guard 经 hook.Registry 注册，本实现直接实现 HookRunner 接口。
type Guard struct {
	mu          sync.Mutex
	audit       []AuditEntry
	attrib      map[string]int
	awaiting    map[string][]int64 // 调用指纹 -> 审批放行、待回填执行结果的 pendingID（FIFO）
	interceptor *intercept.Interceptor
}

func New(ic *intercept.Interceptor) *Guard {
	return &Guard{attrib: map[string]int{}, awaiting: map[string][]int64{}, interceptor: ic}
}

// callKey 关联同一次调用的 Pre 与 Post：Pre/Post 使用不同 Context（工具 Context / 父 Context），
// 因此用工具名 + 入参指纹关联；同指纹的并发调用按 FIFO 对应。
func callKey(name string, input []byte) string {
	sum := sha256.Sum256(input)
	return name + "\x00" + hex.EncodeToString(sum[:])
}

func (g *Guard) record(e AuditEntry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.audit = append(g.audit, e)
	if n := len(g.audit) - maxAudit; n > 0 {
		g.audit = append([]AuditEntry(nil), g.audit[n:]...)
	}
}

func (g *Guard) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	if g.interceptor == nil {
		return false, "", nil
	}
	sum := sha256.Sum256(input)
	e := AuditEntry{At: time.Now(), Tool: name, Digest: hex.EncodeToString(sum[:8])}
	v := g.interceptor.Check(ctx, name, input)
	switch {
	case !v.Enabled:
		e.Decision = "pass"
	case v.PendingID != 0:
		e.Decision = "ask:" + map[bool]string{true: intercept.ActionAllow, false: intercept.ActionDeny}[v.Allowed]
	case v.Allowed:
		e.Decision = intercept.ActionAllow
	default:
		e.Decision = intercept.ActionDeny
	}
	e.Source, e.PendingID, e.Message = v.Decision.Source, v.PendingID, v.Message
	g.record(e)
	if !v.Allowed {
		msg := v.Message
		if msg == "" {
			msg = "拦截控制面拒绝执行"
		}
		return true, msg, nil
	}
	if v.PendingID != 0 {
		k := callKey(name, input)
		g.mu.Lock()
		g.awaiting[k] = append(g.awaiting[k], v.PendingID)
		g.mu.Unlock()
	}
	return false, "", nil
}

func (g *Guard) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	k := callKey(name, input)
	g.mu.Lock()
	status := "ok"
	if isErr {
		status = "error"
	}
	g.attrib[name+":"+status]++
	var pendingID int64
	if q := g.awaiting[k]; len(q) > 0 {
		pendingID = q[0]
		if len(q) == 1 {
			delete(g.awaiting, k)
		} else {
			g.awaiting[k] = q[1:]
		}
	}
	g.mu.Unlock()
	if pendingID != 0 && g.interceptor != nil {
		g.interceptor.CompleteExecution(ctx, pendingID, isErr, string(result))
	}
}

// Stop：Guard 没有 Stop Hook 语义（§6.2），恒不干预。
func (g *Guard) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return false, nil, ""
}

// Audit 返回内存审计快照（最旧在前）。
func (g *Guard) Audit() []AuditEntry {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]AuditEntry(nil), g.audit...)
}

// Attribution 返回工具执行结果计数快照，键为 "工具名:ok|error"。
func (g *Guard) Attribution() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]int, len(g.attrib))
	for k, v := range g.attrib {
		out[k] = v
	}
	return out
}

func (e AuditEntry) String() string {
	return fmt.Sprintf("%s %s %s(%s) #%d %s", e.At.Format(time.RFC3339), e.Tool, e.Decision, e.Source, e.PendingID, e.Message)
}
