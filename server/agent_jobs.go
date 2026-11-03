package server

// S2：后台作业事件的投递格式、唤醒协调与 /agent-jobs 查询（AGENT_TOOL_SKILLS_PLAN 5.3 / 8）。
// 唤醒以数据库扫描为准；WakeAgentJobs 只是作业终态后的加速信号。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/toolhealth"
)

// jobWakeInput 是被作业事件唤醒的回合输入；事件正文随后附上，每条都带可确认的事件标记。
const jobWakeInput = "你提交的后台作业已有结果，当前意图被重新唤醒。请阅读下列作业事件：成功则核对结果并写回结论；" +
	"失败或取消则如实 record_fact 记录缺口与原因，不要以相同参数重复提交；全部处理后总结并结束。"

// formatAgentJobEvent 把终态事件渲染为 Worker 消息；错误原因脱敏后再交给模型。
func formatAgentJobEvent(d *db.AgentJobDelivery) string {
	var payload map[string]any
	_ = json.Unmarshal(d.Payload, &payload)
	if e, ok := payload["error"].(string); ok {
		if e = strings.TrimSpace(toolhealth.Sanitize(e)); e == "" {
			delete(payload, "error")
		} else {
			payload["error"] = e
		}
	}
	detail, _ := json.Marshal(payload)
	target := "{}"
	if len(d.TargetRef) > 0 {
		target = string(d.TargetRef)
	}
	return fmt.Sprintf("[%s] job_ref={\"kind\":%q,\"id\":%d} target_ref=%s 状态=%s 业务状态=%s 详情=%s",
		db.AgentJobEventMarker(d.EventID), d.Job.Kind, d.Job.ID, target, d.TerminalStatus, d.BusinessState, detail)
}

// deliveredEvents 记录本次执行投递的事件 ID；收尾时只确认其中已写入已保存 transcript 的事件。
type deliveredEvents struct {
	mu  sync.Mutex
	ids []int64
}

func (d *deliveredEvents) add(id int64) {
	d.mu.Lock()
	d.ids = append(d.ids, id)
	d.mu.Unlock()
}

func (d *deliveredEvents) list() []int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.ids)
}

// savedJobEvents 返回本次投递且确已保存在 Worker transcript 中的事件；读取失败时一律不确认（事件随后重放）。
func savedJobEvents(ctx context.Context, worker *agent.Worker, expID, intentID int64, delivered []int64) []int64 {
	if len(delivered) == 0 {
		return nil
	}
	msgs, err := worker.SavedTranscript(ctx, expID, intentID)
	if err != nil {
		return nil
	}
	raw, err := json.Marshal(msgs)
	if err != nil {
		return nil
	}
	var out []int64
	for _, id := range db.AgentJobEventIDsIn(string(raw)) {
		if slices.Contains(delivered, id) {
			out = append(out, id)
		}
	}
	return out
}

// WakeAgentJobs 在作业终态后非阻塞地加速一次唤醒扫描。
func (e *Engine) WakeAgentJobs() {
	select {
	case e.jobWake <- struct{}{}:
	default:
	}
}

// jobWakeLoop 周期扫描有未消费作业事件的等待中 Intent 并重开（启动即扫一次，恢复重启前的等待）。
func (e *Engine) jobWakeLoop(ctx context.Context) {
	ticker := time.NewTicker(e.cfg.JobWakePoll)
	defer ticker.Stop()
	for {
		e.wakeJobWaitersOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-e.jobWake:
		}
	}
}

// wakeJobWaitersOnce 重开一批可唤醒的 Intent；部分失败时已重开的照常上报，其余留给下一次扫描。
func (e *Engine) wakeJobWaitersOnce(ctx context.Context) {
	woken, _ := e.m.pg.WakeWaitingIntents(ctx, 100)
	for _, w := range woken {
		e.emitWoken(w)
	}
}

func (e *Engine) emitWoken(w db.WokenIntent) {
	t := e.m.taskByExploration(w.ExplorationID)
	if t == nil {
		return
	}
	id := w.IntentID
	e.emit(t, db.Activity{Worker: "agent-jobs", Kind: "state", NodeID: &id, Summary: fmt.Sprintf("后台作业已有结果，意图 #%d 重新排队", id)})
}

// ---------------------------------------------------------------------------
// HTTP：GET /api/tasks/{id}/agent-jobs
// ---------------------------------------------------------------------------

func (a *API) registerAgentJobRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tasks/{id}/agent-jobs", a.withTask(a.listAgentJobs))
}

// listAgentJobs 返回 Agent 订阅的后台作业（含作业当前状态与终态事件），供执行页恢复等待状态；SSE 只提示重读。
func (a *API) listAgentJobs(w http.ResponseWriter, r *http.Request, t *Task) {
	q := r.URL.Query()
	var f db.AgentJobFilter
	var err error
	parse := func(name string) int64 {
		if err != nil || q.Get(name) == "" {
			return 0
		}
		var v int64
		if v, err = strconv.ParseInt(q.Get(name), 10, 64); err == nil && v < 0 {
			err = fmt.Errorf("%s must be non-negative", name)
		}
		return v
	}
	f.IntentID, f.Before, f.Job.ID = parse("intent_id"), parse("before"), parse("job_id")
	f.Limit = int(parse("limit"))
	f.Job.Kind = q.Get("kind")
	if err != nil {
		fail(w, badRequest("%s", err.Error()))
		return
	}
	if f.Job.Kind != "" && !db.ValidAgentJobKind(f.Job.Kind) {
		fail(w, badRequest("kind must be one of %s", strings.Join(db.AgentJobKinds(), ", ")))
		return
	}
	jobs, err := a.pg.ListAgentJobs(r.Context(), db.DefaultTenant, t.ID, f)
	if err != nil {
		fail(w, err)
		return
	}
	for _, j := range jobs {
		j.JobError = toolhealth.Sanitize(j.JobError)
	}
	writeJSON(w, http.StatusOK, jobs)
}
