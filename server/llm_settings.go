package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/bidtools"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/llm/llmhttp"
)

type profileInput struct {
	db.LLMProfile
	APIKey      string `json:"api_key"`
	ClearAPIKey bool   `json:"clear_api_key"`
	ProfileID   int64  `json:"profile_id"`
}

func (a *API) profileInput(r *http.Request, id int64, probe bool) (*db.LLMProfile, error) {
	var in profileInput
	if err := decodeBody(r, &in); err != nil {
		return nil, err
	}
	p := in.LLMProfile
	p.ID = id
	if probe && id == 0 {
		p.ID = in.ProfileID
		if p.ID == 0 {
			p.ID = in.LLMProfile.ID
		}
	}
	p.Name = strings.TrimSpace(p.Name)
	p.Model = strings.TrimSpace(p.Model)
	if probe && p.Name == "" {
		p.Name = "Connection test"
	}
	if p.Format == "" {
		p.Format = "anthropic"
	}
	p.BaseURL = normalizeModelURL(p.Format, p.BaseURL)
	p.Proxy = strings.TrimSpace(p.Proxy)
	p.SessionHeaderKey = strings.TrimSpace(p.SessionHeaderKey)
	if err := validateProfile(&p); err != nil {
		return nil, err
	}
	if p.BaseURL != "" {
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, badRequest("invalid Base URL; use an http(s) API root without credentials or query")
		}
	}
	if p.Proxy != "" {
		u, err := url.Parse(p.Proxy)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return nil, badRequest("invalid proxy URL")
		}
	}
	if p.SessionHeaderKey != "" {
		for _, c := range p.SessionHeaderKey {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
				return nil, badRequest("invalid session header name")
			}
		}
		switch strings.ToLower(p.SessionHeaderKey) {
		case "authorization", "x-api-key", "host", "content-type", "content-length", "connection":
			return nil, badRequest("reserved session header name")
		}
	}
	if p.ID > 0 {
		old, err := a.pg.GetLLMProfile(r.Context(), p.ID)
		if err != nil {
			return nil, err
		}
		if !probe && old.IsDefault && !p.Enabled {
			return nil, badRequest("activate another profile before disabling the default")
		}
		p.APIKeyCiphertext, p.APIKeyHint = old.APIKeyCiphertext, old.APIKeyHint
		if probe && p.APIKeyEnv == "" {
			p.APIKeyEnv = old.APIKeyEnv
		}
	}
	if in.ClearAPIKey {
		p.APIKeyCiphertext, p.APIKeyHint = "", ""
	}
	if key := strings.TrimSpace(in.APIKey); key != "" {
		if probe {
			p.APIKey = key
		} else {
			encrypted, err := a.llm.cfg.Vault.Encrypt(key)
			if err != nil {
				return nil, err
			}
			p.APIKeyCiphertext = encrypted
			if len(key) >= 4 {
				p.APIKeyHint = "…" + key[len(key)-4:]
			} else {
				p.APIKeyHint = "已设置"
			}
		}
	}
	if p.APIKeyEnv == "" {
		p.APIKeyEnv = "ANTHROPIC_API_KEY"
		if p.Format == "openai" || p.Format == "openai-responses" {
			p.APIKeyEnv = "OPENAI_API_KEY"
		}
	}
	p.CreatedBy, p.UpdatedBy = operator(r), operator(r)
	return &p, nil
}
func normalizeModelURL(format, base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	switch format {
	case "openai":
		base = strings.TrimSuffix(base, "/chat/completions")
	case "openai-responses":
		base = strings.TrimSuffix(base, "/responses")
	default:
		base = strings.TrimSuffix(base, "/v1/messages")
	}
	return strings.TrimRight(base, "/")
}
func (a *API) profileView(p *db.LLMProfile) *db.LLMProfile {
	p.KeyConfigured = p.APIKeyCiphertext != "" || os.Getenv(p.APIKeyEnv) != ""
	if p.APIKeyCiphertext == "" {
		if p.KeyConfigured {
			p.APIKeyHint = "环境变量已配置"
		} else {
			p.APIKeyHint = ""
		}
	}
	return p
}
func (s *LLMService) resolveKey(p *db.LLMProfile) error {
	if p.APIKey != "" {
		return nil
	}
	if p.APIKeyCiphertext != "" {
		key, err := s.cfg.Vault.Decrypt(p.APIKeyCiphertext)
		if err != nil {
			return err
		}
		p.APIKey = key
		return nil
	}
	keyEnv := p.APIKeyEnv
	if keyEnv == "" {
		keyEnv = "ANTHROPIC_API_KEY"
		if p.Format == "openai" || p.Format == "openai-responses" {
			keyEnv = "OPENAI_API_KEY"
		}
	}
	p.APIKey = os.Getenv(keyEnv)
	return nil
}
func (a *API) getBindings(w http.ResponseWriter, r *http.Request) {
	v, e := a.pg.AgentLLMBindings(r.Context())
	if e != nil {
		fail(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (a *API) activateProfile(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID int64 `json:"id"`
	}
	if e := decodeBody(r, &in); e != nil {
		fail(w, e)
		return
	}
	if e := a.pg.ActivateLLMProfile(r.Context(), in.ID, operator(r)); e != nil {
		fail(w, e)
		return
	}
	a.llm.InvalidateProviders()
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *API) getLLMSettings(w http.ResponseWriter, r *http.Request) {
	s, e := a.pg.LLMSettings(r.Context())
	if e != nil {
		fail(w, e)
		return
	}
	writeJSON(w, 200, s)
}
func (a *API) putLLMSettings(w http.ResponseWriter, r *http.Request) {
	var s db.LLMSettings
	if e := decodeBody(r, &s); e != nil {
		fail(w, e)
		return
	}
	if e := s.Retry.Validate(); e != nil {
		fail(w, badRequest("%s", e))
		return
	}
	if e := a.pg.SaveLLMSettings(r.Context(), s, operator(r)); e != nil {
		fail(w, e)
		return
	}
	a.llm.InvalidateProviders()
	writeJSON(w, 200, s)
}
func (a *API) resetLLMPool(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID int64 `json:"id"`
	}
	if e := decodeBody(r, &in); e != nil {
		fail(w, e)
		return
	}
	if in.ID < 0 {
		fail(w, badRequest("invalid id"))
		return
	}
	ids := []int64{in.ID}
	if in.ID == 0 {
		ps, e := a.pg.ListLLMProfiles(r.Context())
		if e != nil {
			fail(w, e)
			return
		}
		ids = nil
		for _, p := range ps {
			ids = append(ids, p.ID)
		}
	}
	for _, id := range ids {
		if e := a.llm.health.Reset(id, func(id int64) error { return a.pg.DeleteLLMHealth(r.Context(), id) }); e != nil {
			fail(w, e)
			return
		}
	}
	a.poolStatus(w, r)
}
func (a *API) poolStatus(w http.ResponseWriter, r *http.Request) {
	s, err := a.pg.LLMSettings(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	ps, err := a.pg.ListLLMProfiles(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	hs, err := a.pg.ListLLMHealth(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	hm := map[int64]db.LLMHealth{}
	for _, h := range hs {
		hm[h.ProfileID] = h
	}
	used := map[int64]string{}
	rows, err := a.pg.QueryContext(r.Context(), `SELECT DISTINCT ON (profile_id) profile_id,status FROM llm_usage WHERE profile_id IS NOT NULL ORDER BY profile_id,created_at DESC,id DESC`)
	if err != nil {
		fail(w, err)
		return
	}
	for rows.Next() {
		var id int64
		var status string
		if err = rows.Scan(&id, &status); err != nil {
			break
		}
		used[id] = status
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		fail(w, err)
		return
	}
	if rowErr != nil {
		fail(w, rowErr)
		return
	}
	chain := make([]map[string]any, 0, len(ps))
	order := 0
	for _, p := range ps {
		a.profileView(p)
		h := hm[p.ID]
		if current, ok := a.llm.health.Snapshot(p.ID); ok {
			h.Fails = current.Fails
			h.Trips = current.Trips
			h.OpenUntil = current.OpenUntil
			h.LastError = current.LastError
		}
		state := "untested"
		if used[p.ID] == "ok" {
			state = "ok"
		}
		if h.Fails > 0 || used[p.ID] == "error" {
			state = "degraded"
		}
		if h.OpenUntil.After(time.Now()) {
			state = "tripped"
		}
		if !p.KeyConfigured {
			state = "missing_key"
		}
		if !p.Enabled {
			state = "disabled"
		}
		excluded := !p.Enabled || (!p.InGlobalPool && !p.IsDefault)
		n := 0
		if !excluded {
			order++
			n = order
		}
		chain = append(chain, map[string]any{"profile_id": p.ID, "name": p.Name, "model": p.Model, "active": p.IsDefault, "priority": p.Priority, "excluded": excluded, "order": n, "state": state, "fails": h.Fails, "trips": h.Trips, "cooldown_secs": max(0, int(time.Until(h.OpenUntil).Seconds())), "last_error": h.LastError})
	}
	writeJSON(w, 200, map[string]any{"enabled": s.PoolEnabled, "bind_fallback": s.BindFallback, "chain": chain})
}
func (a *API) testLLM(w http.ResponseWriter, r *http.Request) {
	p, err := a.profileInput(r, 0, true)
	if err != nil {
		fail(w, err)
		return
	}
	if err = a.llm.resolveKey(p); err != nil {
		fail(w, err)
		return
	}
	if p.APIKey == "" {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "未配置 API Key"})
		return
	}
	c, err := llmhttp.NewClient(llmhttp.Options{Proxy: p.Proxy, SessionHeaderKey: p.SessionHeaderKey})
	if err != nil {
		fail(w, err)
		return
	}
	prov, err := a.llm.cfg.Factory(p, c)
	if err != nil {
		fail(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(llm.WithSessionID(r.Context(), fmt.Sprintf("connection-test-%d", time.Now().UnixNano())), 30*time.Second)
	defer cancel()
	ctx = llm.WithSingleAttempt(ctx)
	req := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("Reply with OK.")}}
	start := time.Now()
	var msg llm.Message
	var stop string
	if p.Streams() {
		acc := llm.NewAccumulator()
		for ev, e := range prov.Stream(ctx, req) {
			if e != nil {
				err = e
				break
			}
			acc.Add(ev)
		}
		msg, stop, _ = acc.Result()
	} else {
		msg, stop, _, err = prov.Complete(ctx, req)
	}
	if err == nil && strings.TrimSpace(msg.Text()) == "" {
		err = errors.New("模型未返回正文，请检查思考参数和输出上限（" + stop + "）")
	}
	out := map[string]any{"ok": err == nil, "latency_ms": time.Since(start).Milliseconds(), "model": p.Model}
	if err != nil {
		out["error"] = strings.ReplaceAll(err.Error(), p.APIKey, "[redacted]")
	} else {
		reply := []rune(msg.Text())
		out["reply"] = string(reply[:min(len(reply), 200)])
	}
	writeJSON(w, 200, out)
}
func (a *API) listModels(w http.ResponseWriter, r *http.Request) {
	// Model discovery uses the same credential/proxy resolution as completion testing.
	var in profileInput
	if err := decodeBody(r, &in); err != nil {
		fail(w, err)
		return
	}
	p := in.LLMProfile
	p.APIKey = in.APIKey
	if in.ProfileID > 0 {
		old, e := a.pg.GetLLMProfile(r.Context(), in.ProfileID)
		if e != nil {
			fail(w, e)
			return
		}
		p.APIKeyCiphertext = old.APIKeyCiphertext
		if p.APIKeyEnv == "" {
			p.APIKeyEnv = old.APIKeyEnv
		}
	}
	if in.ClearAPIKey {
		p.APIKeyCiphertext = ""
	}
	if err := a.llm.resolveKey(&p); err != nil {
		fail(w, err)
		return
	}
	if p.APIKey == "" {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "未配置 API Key"})
		return
	}
	base := normalizeModelURL(p.Format, p.BaseURL)
	if base == "" {
		if p.Format == "anthropic" {
			base = "https://api.anthropic.com"
		} else {
			base = "https://api.openai.com/v1"
		}
	}
	paths := []string{base + "/models"}
	if p.Format == "anthropic" {
		paths = []string{base + "/v1/models"}
		if root := strings.TrimSuffix(base, "/anthropic"); root != base {
			paths = append(paths, root+"/models", root+"/v1/models")
		}
	}
	c, err := llmhttp.NewClient(llmhttp.Options{Proxy: p.Proxy})
	if err != nil {
		fail(w, err)
		return
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	last := "未获取到模型"
	for i, path := range paths {
		req, e := http.NewRequestWithContext(ctx, "GET", path, nil)
		if e != nil {
			last = "无效的 Base URL"
			break
		}
		if p.Format == "anthropic" && i == 0 {
			req.Header.Set("x-api-key", p.APIKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		} else {
			req.Header.Set("Authorization", "Bearer "+p.APIKey)
		}
		resp, e := c.Do(req)
		if e != nil {
			last = "模型列表请求失败"
			continue
		}
		raw, e := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if e != nil || resp.StatusCode != 200 {
			last = fmt.Sprintf("模型列表返回 HTTP %d", resp.StatusCode)
			continue
		}
		var body struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &body) != nil {
			last = "模型列表格式无效"
			continue
		}
		models := []string{}
		seen := map[string]bool{}
		for _, m := range body.Data {
			if m.ID != "" && !seen[m.ID] {
				models = append(models, m.ID)
				seen[m.ID] = true
			}
		}
		sort.Strings(models)
		writeJSON(w, 200, map[string]any{"ok": true, "models": models})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": false, "error": last})
}

func (a *API) listLLMRecords(w http.ResponseWriter, r *http.Request) {
	var f db.LLMRecordFilter
	if v := r.URL.Query().Get("task_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			fail(w, badRequest("invalid task_id"))
			return
		}
		f.TaskID = id
	}
	f.SessionID = strings.TrimSpace(r.URL.Query().Get("session_id"))
	f.ProfileName = strings.TrimSpace(r.URL.Query().Get("profile_name"))
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Offset = n
		}
	}
	items, total, err := a.pg.ListLLMRecords(r.Context(), f)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

func (a *API) getLLMRecord(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "recordID")
	if err != nil {
		fail(w, err)
		return
	}
	rec, err := a.pg.GetLLMRecord(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (a *API) deleteLLMRecords(w http.ResponseWriter, r *http.Request) {
	var taskID int64
	if v := r.URL.Query().Get("task_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			fail(w, badRequest("invalid task_id"))
			return
		}
		taskID = id
	}
	deleted, err := a.pg.DeleteLLMRecordsByTask(r.Context(), taskID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

func (a *API) systemCatalog(w http.ResponseWriter, r *http.Request) {
	bindings, _ := a.pg.AgentLLMBindings(r.Context())
	kit := bidtools.New(nil, 0, nil, nil)
	skills := bidtools.Skills()
	unlockMap := map[string]string{}
	for _, sk := range skills {
		for _, tn := range sk.Tools {
			unlockMap[tn] = sk.Name
		}
	}
	type toolEntry struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Category    string         `json:"category"` // resident / deferred / system
		UnlockedBy  string         `json:"unlocked_by,omitempty"`
		InputSchema map[string]any `json:"input_schema,omitempty"`
	}
	tools := make([]toolEntry, 0)
	for _, ct := range kit.ResidentTools() {
		tools = append(tools, toolEntry{
			Name:        ct.Name(),
			Description: ct.Description(),
			Category:    "resident",
			InputSchema: ct.InputSchema(),
		})
	}
	for _, ct := range kit.DeferredTools() {
		tools = append(tools, toolEntry{
			Name:        ct.Name(),
			Description: ct.Description(),
			Category:    "deferred",
			UnlockedBy:  unlockMap[ct.Name()],
			InputSchema: ct.InputSchema(),
		})
	}
	agents := []map[string]any{
		{
			"key":              "goals",
			"name":             "目标分解 Agent (Goals)",
			"description":      "解析招标文件（RFP），拆解资格/否决项、★实质性条款与技术偏离要求、评分标准分档判据，跨四域同步静态要素并初始化推演图谱四大 Goal 根节点（G-VETO / G-DEVIATION / G-SCORING / G-OUTLINE）与技术标目录大纲草案。",
			"bound_profile_id": bindings["goals"],
		},
		{
			"key":              "planner",
			"name":             "推演规划 Agent (Planner)",
			"description":      "统筹招标要求拆解、推演前沿调度、发现缺口并派生带 Mandate 边界约束的 Worker 工作意图与修订指令。",
			"bound_profile_id": bindings["planner"],
		},
		{
			"key":              "worker",
			"name":             "任务执行 Agent (Worker)",
			"description":      "携带招投标常驻工具与按需解锁 Skill，在 Inline & Spill 上下文预算内执行条款精读、资信核对、证据检索、章节撰写与合规核验。",
			"bound_profile_id": bindings["worker"],
		},
		{
			"key":              "watchdog",
			"name":             "跑偏监控 Agent (Watchdog)",
			"description":      "在 Worker 工具调用生命周期（PreToolUse / PostToolUse / Stop）实时校验 Mandate 硬件边界，监控重复调用、连续报错与空转轨迹，自动注入 Steer 纠偏或触发熔断。",
			"bound_profile_id": bindings["watchdog"],
		},
		{
			"key":              "judge",
			"name":             "安全裁判 Agent (LLM Judge)",
			"description":      "对未命中静态拦截规则的受控工具调用执行无工具安全语义裁决（allow / ask / deny）。",
			"bound_profile_id": bindings["judge"],
		},
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agents": agents,
		"skills": skills,
		"tools":  tools,
	})
}
