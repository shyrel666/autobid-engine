package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/ops"
)

type history struct {
	Task, Section, File, Parse, Chart int64
	Exports                           []int64
	ExportHashes                      map[int64]string
}

func taskPath(id int64) string { return fmt.Sprintf("/api/tasks/%d", id) }
func (r *rehearsal) waitJob(p *process, path string, wrapped bool) map[string]any {
	r.t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		v := decode[map[string]any](r, r.api(p, "GET", path, nil))
		if wrapped {
			v = v["job"].(map[string]any)
		}
		s, _ := v["status"].(string)
		if s == "succeeded" {
			return v
		}
		if s != "queued" && s != "running" {
			r.must(fmt.Errorf("作业未成功 %s: %s", path, s))
		}
		select {
		case <-r.ctx.Done():
			r.must(r.ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	r.must(fmt.Errorf("等待作业超时 %s", path))
	return nil
}
func (r *rehearsal) upload(p *process, task int64, name, body string) int64 {
	q, e := http.NewRequestWithContext(r.ctx, "POST", p.base+taskPath(task)+"/files?filename="+name, strings.NewReader(body))
	r.must(e)
	q.Header.Set("Authorization", "Bearer "+r.token)
	q.Header.Set("Content-Type", "text/plain")
	resp, e := http.DefaultClient.Do(q)
	r.must(e)
	defer resp.Body.Close()
	b, e := io.ReadAll(resp.Body)
	r.must(e)
	r.assert(resp.StatusCode == 201, "上传历史原件失败："+string(b))
	v := decode[struct {
		ID int64 `json:"id"`
	}](r, b)
	return v.ID
}
func (r *rehearsal) download(p *process, task, job int64, current bool) string {
	q, e := http.NewRequestWithContext(r.ctx, "GET", p.base+fmt.Sprintf("%s/exports/%d/download", taskPath(task), job), nil)
	r.must(e)
	q.Header.Set("Authorization", "Bearer "+r.token)
	resp, e := http.DefaultClient.Do(q)
	r.must(e)
	defer resp.Body.Close()
	b, e := io.ReadAll(resp.Body)
	r.must(e)
	r.assert(resp.StatusCode == 200 && len(b) > 500, "历史导出下载失败或内容为空")
	r.assert(bytes.HasPrefix(b, []byte("PK")) || bytes.HasPrefix(b, []byte("%PDF-")), "导出魔数无效")
	if current {
		r.assert(resp.Header.Get("X-Export-Business-State") == "preview_only", "旧导出未标记为兼容预览结果")
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (r *rehearsal) seed(p *process) history {
	h := history{ExportHashes: map[int64]string{}}
	created := decode[struct {
		Task struct {
			ID int64 `json:"id"`
		} `json:"task"`
		ID int64 `json:"id"`
	}](r, r.api(p, "POST", "/api/tasks", map[string]any{"name": "S7 升级历史项目", "goal": "完成历史技术方案", "start": false}))
	h.Task = created.Task.ID
	if h.Task == 0 {
		h.Task = created.ID
	}
	r.assert(h.Task > 0, "旧项目 ID 缺失")
	base := taskPath(h.Task)
	tender := "历史招标文件\n第一章 技术要求\n1.1 系统应支持统一运维管理。\n1.2 系统应支持运行状态展示。\n第二章 实施要求\n2.1 提供安装部署方案与操作培训方案。\n"
	h.File = r.upload(p, h.Task, "legacy-tender.txt", tender)
	parse := decode[struct {
		ID int64 `json:"id"`
	}](r, r.api(p, "POST", base+"/file-jobs", map[string]any{"file_id": h.File, "request_key": "legacy-import", "tender_no": "S7-LEGACY", "title": "历史招标文件"}))
	h.Parse = parse.ID
	r.waitJob(p, fmt.Sprintf("%s/file-jobs/%d", base, h.Parse), false)
	r.api(p, "POST", base+"/requirements/extract", nil)
	reqs := decode[struct {
		Manifest struct {
			Epoch int64 `json:"epoch"`
		} `json:"manifest"`
		Items []struct {
			ID       int64  `json:"id"`
			Category string `json:"category"`
		} `json:"items"`
	}](r, r.api(p, "GET", base+"/requirements", nil))
	for _, q := range reqs.Items {
		if q.Category == "veto_item" {
			r.api(p, "PUT", fmt.Sprintf("%s/requirements/%d/veto", base, q.ID), map[string]any{"status": "passed", "note": "合成历史夹具已核对，不构成真实资格审定"})
		}
	}
	reqs = decode[struct {
		Manifest struct {
			Epoch int64 `json:"epoch"`
		} `json:"manifest"`
		Items []struct {
			ID       int64  `json:"id"`
			Category string `json:"category"`
		} `json:"items"`
	}](r, r.api(p, "GET", base+"/requirements", nil))
	r.api(p, "POST", base+"/requirements/confirm", map[string]any{"epoch": reqs.Manifest.Epoch})
	outline := decode[struct {
		Outline struct {
			Version int `json:"version"`
		} `json:"outline"`
	}](r, r.api(p, "POST", base+"/outlines", map[string]any{}))
	op := fmt.Sprintf("%s/outlines/%d", base, outline.Outline.Version)
	r.api(p, "POST", op+"/submit", nil)
	approved := decode[struct {
		Sections []struct {
			ID int64 `json:"id"`
		} `json:"sections"`
	}](r, r.api(p, "POST", op+"/approve", nil))
	r.assert(len(approved.Sections) > 0, "旧大纲没有章节")
	for _, s := range approved.Sections {
		d := decode[struct {
			Section struct {
				CurrentRev int    `json:"current_rev_no"`
				Version    int    `json:"outline_version"`
				Title      string `json:"title"`
			} `json:"section"`
			Requirements []struct {
				Code        string `json:"code"`
				Title       string `json:"title"`
				Description string `json:"description"`
			} `json:"requirements"`
		}](r, r.api(p, "GET", fmt.Sprintf("%s/sections/%d", base, s.ID), nil))
		body := "## " + d.Section.Title + "\n\n本章说明本项目的实施方法，按要求逐项响应。\n"
		for _, q := range d.Requirements {
			body += "\n响应 " + q.Code + "：" + q.Title + "。本项目方案满足：" + q.Description + "。\n"
		}
		v := decode[struct {
			Revision struct {
				Rev int `json:"rev_no"`
			} `json:"revision"`
		}](r, r.api(p, "POST", fmt.Sprintf("%s/sections/%d/revisions", base, s.ID), map[string]any{"base_rev_no": d.Section.CurrentRev, "based_on_outline_version": d.Section.Version, "content_md": body, "summary_note": "合成历史正文"}))
		r.api(p, "POST", fmt.Sprintf("%s/sections/%d/revisions/%d/approve", base, s.ID, v.Revision.Rev), nil)
	}
	h.Section = approved.Sections[0].ID
	chart := decode[struct {
		ID           int64  `json:"id"`
		RenderStatus string `json:"render_status"`
		RenderError  string `json:"render_error"`
	}](r, r.api(p, "POST", base+"/charts", map[string]any{"section_id": h.Section, "chart_type": "architecture", "title": "历史运维架构图", "spec": map[string]any{"layers": []any{map[string]any{"name": "管理层", "nodes": []string{"运维工作台", "状态展示"}}}}}))
	h.Chart = chart.ID
	r.assert(chart.RenderStatus == "rendered", "旧版图表必须由真实 Mermaid 渲染成功："+chart.RenderStatus+" "+chart.RenderError)
	r.api(p, "POST", base+"/compliance/verify", map[string]any{})
	r.api(p, "POST", base+"/consistency/runs", nil)
	status := decode[struct {
		Office bool `json:"office"`
	}](r, r.api(p, "GET", "/api/export/status", nil))
	r.assert(status.Office, "真实文档引擎不可用，DOCX/PDF 演练不能算通过")
	jobs := decode[struct {
		Jobs []struct {
			ID int64 `json:"id"`
		} `json:"jobs"`
	}](r, r.api(p, "POST", base+"/exports", map[string]any{"formats": []string{"docx", "pdf"}, "bidder": "合成演练单位"}))
	r.assert(len(jobs.Jobs) == 2, "旧版未创建双格式导出作业")
	for _, j := range jobs.Jobs {
		r.waitJob(p, fmt.Sprintf("%s/exports/%d", base, j.ID), false)
		h.Exports = append(h.Exports, j.ID)
		h.ExportHashes[j.ID] = r.download(p, h.Task, j.ID, false)
	}
	r.api(p, "POST", "/api/knowledge/documents", map[string]any{"category": "whitepaper", "title": "历史运维知识", "text": "历史运维知识：统一运维管理与运行状态展示，安装部署方案、操作培训与工作交接。"})
	index := decode[struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}](r, r.api(p, "POST", "/api/knowledge/indexes", map[string]any{"dimension": 3}))
	r.assert(index.Status == "ready", "旧知识索引未建成")
	r.api(p, "POST", fmt.Sprintf("/api/knowledge/indexes/%d/activate", index.ID), nil)
	r.pass("旧程序通过公开 API 创建历史数据与真实 DOCX/PDF、Mermaid 产物", h)
	return h
}

type snapshot struct {
	Tables  map[string][]map[string]any `json:"tables"`
	Columns map[string][]string         `json:"columns"`
}

func (r *rehearsal) snapshot(d *db.DB, columns map[string][]string) snapshot {
	s := snapshot{Tables: map[string][]map[string]any{}, Columns: map[string][]string{}}
	for _, table := range []string{"tasks", "tender_documents", "document_chunks", "requirement_manifests", "requirement_items", "proposal_outlines", "proposal_sections", "section_revisions", "proposal_charts", "export_jobs", "knowledge_documents", "knowledge_chunks", "knowledge_indexes", "knowledge_embeddings", "task_file_objects", "fixed_file_jobs", "file_processing_records"} {
		rows, e := d.QueryContext(r.ctx, `SELECT to_jsonb(t)::text FROM `+table+` t`)
		r.must(e)
		var list []map[string]any
		for rows.Next() {
			var b string
			r.must(rows.Scan(&b))
			v := decode[map[string]any](r, []byte(b))
			if columns != nil {
				keep := map[string]bool{}
				for _, k := range columns[table] {
					keep[k] = true
				}
				for k := range v {
					if !keep[k] {
						delete(v, k)
					}
				}
			}
			list = append(list, v)
		}
		r.must(rows.Err())
		rows.Close()
		if len(list) > 0 {
			for k := range list[0] {
				s.Columns[table] = append(s.Columns[table], k)
			}
			sort.Strings(s.Columns[table])
		}
		sort.Slice(list, func(i, j int) bool {
			a, _ := json.Marshal(list[i])
			b, _ := json.Marshal(list[j])
			return string(a) < string(b)
		})
		s.Tables[table] = list
	}
	return s
}
func (r *rehearsal) compare(before, after snapshot, allowInterrupted bool) {
	if allowInterrupted {
		for _, row := range before.Tables["knowledge_indexes"] {
			if row["status"] == "building" {
				row["status"] = "failed"
				row["error"] = "interrupted before upgrade"
				row["active"] = false
			}
		}
	}
	for table, want := range before.Tables {
		got := after.Tables[table]
		if !reflect.DeepEqual(want, got) {
			r.write("snapshot-difference-"+table+".json", map[string]any{"before": want, "after": got})
			r.must(fmt.Errorf("历史表 %s 在迁移后发生非默认列变化", table))
		}
	}
}
func (r *rehearsal) upgrade() {
	dsn := r.create("history")
	data := filepath.Join(r.tmp, "history-data")
	old, e := r.start(r.oldBin, dsn, data, "legacy")
	r.must(e)
	defer old.stop()
	h := r.seed(old)
	// Interrupt an actual old HTTP index build while the scripted model is blocked.
	r.block.Store(true)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		q, _ := http.NewRequestWithContext(r.ctx, "POST", old.base+"/api/knowledge/indexes", strings.NewReader(`{"dimension":3}`))
		q.Header.Set("Content-Type", "application/json")
		q.Header.Set("Authorization", "Bearer "+r.token)
		resp, e := http.DefaultClient.Do(q)
		if e == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-r.entered:
	case <-time.After(20 * time.Second):
		r.must(fmt.Errorf("未进入旧索引构建中断窗口"))
	}
	old.stop()
	r.block.Store(false)
	<-finished
	d, e := ops.Open(r.ctx, dsn)
	r.must(e)
	defer d.Close()
	before := r.snapshot(d, nil)
	r.write("legacy-snapshot.json", before)
	var building int
	r.must(d.QueryRowContext(r.ctx, `SELECT count(*) FROM knowledge_indexes WHERE status='building'`).Scan(&building))
	r.assert(building == 1, "旧程序强停未留下一个 building 索引")
	r.pass("真实结束旧服务留下 building 索引", building)
	backup := filepath.Join(r.out, "legacy-backup")
	r.ops(dsn, data, "backup", "--out", backup)
	r.ops(dsn, data, "verify", "--dir", backup)
	manifest, e := ops.Verify(backup)
	r.must(e)
	r.pass("旧库数据库先备份、三类文件后复制、全量哈希校验", manifest)
	current, e := r.start(r.newBin, dsn, data, "upgrade-first")
	r.must(e)
	current.stop()
	after := r.snapshot(d, before.Columns)
	r.compare(before, after, true)
	sum, e := d.AppliedSchemaChecksum(r.ctx)
	r.must(e)
	r.assert(sum == db.SchemaChecksum(), "当前 schema 校验和不一致")
	var status, code string
	r.must(d.QueryRowContext(r.ctx, `SELECT status,error_code FROM knowledge_indexes WHERE status='failed'`).Scan(&status, &code))
	r.assert(status == "failed" && code == "INTERRUPTED", "孤立索引没有按 S6 中断规则恢复")
	var grounded bool
	r.must(d.QueryRowContext(r.ctx, `SELECT grounded_writing_required FROM tasks WHERE id=$1`, h.Task).Scan(&grounded))
	r.assert(!grounded, "历史任务被误开启 D3")
	r.pass("自动迁移保留历史行，仅新增默认列；孤立索引 failed/INTERRUPTED", sum)
	current, e = r.start(r.newBin, dsn, data, "upgrade-second")
	r.must(e)
	defer current.stop()
	r.compare(after, r.snapshot(d, before.Columns), false)
	for _, f := range manifest.Files {
		got, e := ops.HashFile(data, f.Path)
		r.must(e)
		r.assert(got == f, "迁移改变历史产物文件")
	}
	for _, j := range h.Exports {
		r.assert(r.download(current, h.Task, j, true) == h.ExportHashes[j], "迁移后历史导出内容变更")
		job := decode[map[string]any](r, r.api(current, "GET", fmt.Sprintf("%s/exports/%d", taskPath(h.Task), j), nil))
		r.assert(job["business_state"] == "preview_only", "旧作业详情冒充最终稿")
	}
	r.api(current, "GET", taskPath(h.Task)+"/exports", nil)
	r.api(current, "GET", taskPath(h.Task)+"/file-jobs", nil)
	r.api(current, "GET", fmt.Sprintf("%s/file-jobs/%d", taskPath(h.Task), h.Parse), nil)
	r.api(current, "GET", fmt.Sprintf("%s/charts/%d", taskPath(h.Task), h.Chart), nil)
	r.pass("重复启动幂等、历史作业列表详情与预览下载、全部产物哈希不变", nil)
	r.newJobs(current, h)
	r.browser(current, h)
	current.stop()
	r.checkCLI(dsn, data)
	// Restore the pre-upgrade backup into a fresh database and fresh file root.
	rollback := r.create("restore")
	restored := filepath.Join(r.tmp, "restored-data")
	cmd := exec.CommandContext(r.ctx, r.opsBin, "restore", "--dir", backup, "--target-database-url-env", "AUTOBID_S7_RESTORE_URL", "--data-root", restored)
	cmd.Dir = r.root
	cmd.Env = append(r.env(dsn, data), "AUTOBID_S7_RESTORE_URL="+rollback)
	b, e := cmd.CombinedOutput()
	if e != nil {
		r.must(fmt.Errorf("恢复命令失败：%s", string(b)))
	}
	restoredDB, e := ops.Open(r.ctx, rollback)
	r.must(e)
	defer restoredDB.Close()
	// Compare the original snapshot saved before the expected INTERRUPTED change.
	original := decode[snapshot](r, mustRead(r, filepath.Join(r.out, "legacy-snapshot.json")))
	r.compare(original, r.snapshot(restoredDB, original.Columns), false)
	back, e := r.start(r.oldBin, rollback, restored, "rollback-a-old")
	r.must(e)
	defer back.stop()
	r.api(back, "GET", taskPath(h.Task)+"/documents", nil)
	r.api(back, "GET", taskPath(h.Task)+"/outlines", nil)
	for _, j := range h.Exports {
		r.assert(r.download(back, h.Task, j, false) == h.ExportHashes[j], "恢复备份后旧版下载内容不同")
	}
	back.stop()
	r.pass("路径 A：恢复备份到空库与空目录，旧程序真实启动读历史与下载", nil)
	// A second restore must refuse both non-empty targets, without a destructive option.
	cmd = exec.CommandContext(r.ctx, r.opsBin, "restore", "--dir", backup, "--target-database-url-env", "AUTOBID_S7_RESTORE_URL", "--data-root", restored)
	cmd.Env = append(r.env(dsn, data), "AUTOBID_S7_RESTORE_URL="+rollback)
	r.assert(cmd.Run() != nil, "恢复错误地覆盖了非空目录")
	cmd = exec.CommandContext(r.ctx, r.opsBin, "restore", "--dir", backup, "--target-database-url-env", "AUTOBID_S7_RESTORE_URL", "--data-root", filepath.Join(r.tmp, "another-empty-root"))
	cmd.Env = append(r.env(dsn, data), "AUTOBID_S7_RESTORE_URL="+rollback)
	r.assert(cmd.Run() != nil, "恢复错误地覆盖了非空库")
	r.pass("恢复拒绝非空目录与非空数据库", nil)
}
func mustRead(r *rehearsal, p string) []byte { b, e := os.ReadFile(p); r.must(e); return b }
func (r *rehearsal) newJobs(p *process, h history) {
	base := taskPath(h.Task)
	j := decode[struct {
		ID int64 `json:"id"`
	}](r, r.api(p, "POST", base+"/file-jobs", map[string]any{"file_id": h.File, "request_key": "new-candidate", "tender_no": "S7-LEGACY", "title": "升级后解析候选", "candidate_only": true, "parser_profile": "native"}))
	r.waitJob(p, fmt.Sprintf("%s/file-jobs/%d", base, j.ID), false)
	c := decode[struct {
		Job struct {
			ID int64 `json:"id"`
		} `json:"job"`
	}](r, r.api(p, "POST", base+"/chart-jobs", map[string]any{"request_key": "new-chart", "section_id": h.Section, "expected_rev_no": 1, "chart_type": "architecture", "title": "升级后运维架构图", "spec": map[string]any{"layers": []any{map[string]any{"name": "运维层", "nodes": []string{"统一工作台", "告警记录"}}}}}))
	r.waitJob(p, fmt.Sprintf("%s/chart-jobs/%d", base, c.Job.ID), true)
	r.api(p, "POST", base+"/compliance/verify", map[string]any{})
	r.api(p, "POST", base+"/consistency/runs", nil)
	raw := r.api(p, "POST", base+"/exports", map[string]any{"request_key": "new-final", "formats": []string{"docx", "pdf"}, "mode": "final", "bidder": "合成演练单位"})
	var sub struct {
		Request struct {
			Jobs []struct {
				ID int64 `json:"id"`
			} `json:"jobs"`
		} `json:"request"`
		Jobs []struct {
			ID int64 `json:"id"`
		} `json:"jobs"`
	}
	r.must(json.Unmarshal(raw, &sub))
	jobs := sub.Jobs
	if len(jobs) == 0 {
		jobs = sub.Request.Jobs
	}
	r.assert(len(jobs) == 2, "新最终稿未返回双格式作业")
	for _, j := range jobs {
		v := r.waitJob(p, fmt.Sprintf("%s/exports/%d", base, j.ID), false)
		r.assert(v["business_state"] == "final_ready", "新严格导出未达到 final_ready")
		r.download(p, h.Task, j.ID, false)
	}
	r.pass("升级后新解析候选、真实图表与严格 DOCX/PDF 最终稿和旧作业并存", nil)
}
func (r *rehearsal) browser(p *process, h history) {
	if os.Getenv("PLAYWRIGHT_MODULE") == "" {
		r.must(fmt.Errorf("历史浏览器验收缺少 PLAYWRIGHT_MODULE，未执行"))
	}
	c := exec.CommandContext(r.ctx, "node", filepath.Join(r.root, "web", "e2e", "s7-history.mjs"))
	c.Dir = r.root
	c.Env = append(os.Environ(), "E2E_BASE="+p.base, "E2E_TOKEN_FILE="+r.tokenFile, "E2E_OUT="+r.out, "S7_HISTORY_TASK="+fmt.Sprint(h.Task), "S7_HISTORY_SECTION="+fmt.Sprint(h.Section))
	b, e := c.CombinedOutput()
	r.must(os.WriteFile(filepath.Join(r.out, "browser.log"), b, 0600))
	if e != nil {
		r.must(fmt.Errorf("历史浏览器验收失败：%s", string(b)))
	}
	r.pass("浏览器历史导出、文档、大纲、写作、知识库与诊断版本（替身模型，非真实模型）", nil)
}
func (r *rehearsal) checkCLI(dsn, data string) {
	r.ops(dsn, data, "check")
	b := r.ops(dsn, data, "check", "--json")
	var report ops.CheckReport
	r.must(json.Unmarshal(b, &report))
	r.assert(report.CanStop && report.Schema == db.SchemaChecksum(), "运维 check 未报告就绪")
	r.write("ops-check.json", report)
	r.pass("运维 check 只读检查、JSON 输出、无在途作业", nil)
}

// Keep explicit imports honest when the drill grows; every process and DB above
// belongs to this invocation, and only those resources are stopped/dropped.
var _ context.Context
