package main

import (
	"bytes"
	"context"
	"crypto/rand"
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
	// LegacyTask / LegacySection hold a storyboard the old program recorded under the declared name 张三 (R2).
	LegacyTask, LegacySection int64
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
	q.Header.Set("Content-Type", "text/plain")
	resp, e := r.do(p, q)
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
	resp, e := r.do(p, q)
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
	r.seedLegacyActor(p, &h)
	r.pass("旧程序通过公开 API 创建历史数据与真实 DOCX/PDF、Mermaid 产物", h)
	return h
}

// seedLegacyActor: in a second project the old program records a storyboard written and submitted
// under the declared name 张三 (any string the page sent). After the upgrade nobody may treat that
// name as "another person" until an administrator maps it to an account.
func (r *rehearsal) seedLegacyActor(p *process, h *history) {
	created := decode[struct {
		Task struct {
			ID int64 `json:"id"`
		} `json:"task"`
		ID int64 `json:"id"`
	}](r, r.api(p, "POST", "/api/tasks", map[string]any{"name": "S7 升级遗留姓名项目", "goal": "验证升级前声明式姓名", "start": false}))
	h.LegacyTask = created.Task.ID
	if h.LegacyTask == 0 {
		h.LegacyTask = created.ID
	}
	base := taskPath(h.LegacyTask)
	r.api(p, "POST", base+"/documents", map[string]any{"tender_no": "S7-LEGACY-ACTOR", "title": "遗留姓名招标文件",
		"text": "遗留姓名招标文件\n第一章 技术要求\n1.1 系统应支持统一运维管理。\n1.2 系统应提供运行状态展示。\n"})
	r.api(p, "POST", base+"/requirements/extract", nil)
	view := decode[struct {
		Manifest struct {
			Epoch int64 `json:"epoch"`
		} `json:"manifest"`
		Items []struct {
			ID       int64  `json:"id"`
			Category string `json:"category"`
		} `json:"items"`
	}](r, r.api(p, "GET", base+"/requirements", nil))
	for _, q := range view.Items {
		if q.Category == "veto_item" {
			r.api(p, "PUT", fmt.Sprintf("%s/requirements/%d/veto", base, q.ID), map[string]any{"status": "passed", "note": "合成历史夹具"})
		}
	}
	view.Manifest.Epoch = decode[struct {
		Manifest struct {
			Epoch int64 `json:"epoch"`
		} `json:"manifest"`
	}](r, r.api(p, "GET", base+"/requirements", nil)).Manifest.Epoch
	r.api(p, "POST", base+"/requirements/confirm", map[string]any{"epoch": view.Manifest.Epoch})
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
	r.assert(len(approved.Sections) > 0, "遗留姓名项目没有章节")
	h.LegacySection = approved.Sections[0].ID
	sec := decode[struct {
		Requirements []struct {
			Code string `json:"code"`
		} `json:"requirements"`
	}](r, r.api(p, "GET", fmt.Sprintf("%s/sections/%d", base, h.LegacySection), nil))
	points := []map[string]string{}
	for _, q := range sec.Requirements {
		points = append(points, map[string]string{"requirement_code": q.Code, "scoring_point": q.Code, "our_approach": "按统一运维平台集中管理", "buyer_benefit": "采购人可集中查看运行状态"})
	}
	if len(points) == 0 {
		points = append(points, map[string]string{"scoring_point": "总体", "our_approach": "按统一运维平台集中管理", "buyer_benefit": "采购人可集中查看运行状态"})
	}
	storyboard := fmt.Sprintf("%s/sections/%d/storyboard", base, h.LegacySection)
	for _, step := range []struct {
		method, path string
		body         any
	}{
		{"PUT", storyboard, map[string]any{"theme_sentence": "统一运维平台集中管理全部设备并展示运行状态，便于采购人逐项核对", "scoring_points": points}},
		{"POST", storyboard + "/submit", nil},
	} {
		b, e := json.Marshal(step.body)
		r.must(e)
		req, e := http.NewRequestWithContext(r.ctx, step.method, p.base+step.path, bytes.NewReader(b))
		r.must(e)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Autobid-User", "张三") // 旧页面上随手填写的"当前操作人"
		resp, e := r.do(p, req)
		r.must(e)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		r.assert(resp.StatusCode < 300, fmt.Sprintf("旧程序记录故事板失败 %s %s：%d %s", step.method, step.path, resp.StatusCode, raw))
	}
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
	var policy, legacyPolicy string
	r.must(d.QueryRowContext(r.ctx, `SELECT quality_policy FROM tasks WHERE id=$1`, h.Task).Scan(&policy))
	r.must(d.QueryRowContext(r.ctx, `SELECT quality_policy FROM tasks WHERE id=$1`, h.LegacyTask).Scan(&legacyPolicy))
	r.assert(policy == db.QualityPolicyLegacy, "没有故事板的历史任务应映射为 legacy/v1，实际 "+policy)
	r.assert(legacyPolicy == db.QualityPolicyGrounded, "已有故事板的历史任务应映射为 grounded/v1（与旧的隐含规则一致），实际 "+legacyPolicy)
	var legacyRows, users int
	r.must(d.QueryRowContext(r.ctx, `SELECT count(*) FROM section_storyboards WHERE actor_legacy`).Scan(&legacyRows))
	r.must(d.QueryRowContext(r.ctx, `SELECT count(*) FROM users`).Scan(&users))
	r.assert(legacyRows == 1 && users == 0, fmt.Sprintf("升级前的故事板应标为声明式姓名、且没有任何预置账号：%d %d", legacyRows, users))
	r.pass("自动迁移保留历史行，仅新增默认列；孤立索引 failed/INTERRUPTED；质量策略按旧隐含规则映射；无预置账号", map[string]any{"checksum": sum, "policies": []string{policy, legacyPolicy}})
	current, e = r.start(r.newBin, dsn, data, "upgrade-second")
	r.must(e)
	defer current.stop()
	lead := r.firstAdminAndTeam(current, dsn, data, h)
	server := current
	current = lead
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
	server.stop()
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
	c.Env = append(os.Environ(), "E2E_BASE="+p.base, "E2E_ACCOUNTS_FILE="+r.accountsFile, "E2E_OUT="+r.out, "S7_HISTORY_TASK="+fmt.Sprint(h.Task), "S7_HISTORY_SECTION="+fmt.Sprint(h.Section))
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

// firstAdminAndTeam covers the upgrade steps docs/OPERATIONS.md asks for: create the first
// administrator with the operations tool, sign in, create accounts, add members to the old projects,
// and resolve a pre-upgrade declared name. It returns the bid manager's session for the later steps.
func (r *rehearsal) firstAdminAndTeam(p *process, dsn, data string, h history) *process {
	r.t.Helper()
	pw := map[string]string{}
	for _, l := range []string{"s7-admin", "s7-lead", "zhangsan", "lisi"} {
		b := make([]byte, 12)
		_, e := rand.Read(b)
		r.must(e)
		pw[l] = hex.EncodeToString(b)
	}
	c := exec.CommandContext(r.ctx, r.opsBin, "user", "create-admin", "--login", "s7-admin", "--display-name", "演练管理员", "--password-stdin")
	c.Dir = r.root
	c.Env = r.env(dsn, data)
	c.Stdin = strings.NewReader(pw["s7-admin"] + "\n")
	if out, e := c.CombinedOutput(); e != nil {
		r.must(fmt.Errorf("ops user create-admin 失败：%s", out))
	}
	admin := r.as(p, "s7-admin", pw["s7-admin"])
	// Old projects have no members after the upgrade: even the administrator cannot read them.
	code, _ := r.status(admin, "GET", taskPath(h.Task), nil, nil)
	r.assert(code == 404, fmt.Sprintf("升级后的旧项目在添加成员前应对管理员返回 404，实际 %d", code))
	for _, u := range []struct{ login, name string }{{"s7-lead", "投标经理"}, {"zhangsan", "张三"}, {"lisi", "李四"}} {
		r.api(admin, "POST", "/api/users", map[string]any{"login": u.login, "display_name": u.name, "password": pw[u.login]})
	}
	all := []string{db.RoleBidManager, db.RoleTechWriter, db.RoleCommercial, db.RoleReviewer}
	r.api(admin, "PUT", taskPath(h.Task)+"/members/s7-lead", map[string]any{"roles": all})
	r.api(admin, "PUT", taskPath(h.LegacyTask)+"/members/s7-lead", map[string]any{"roles": []string{db.RoleBidManager}})
	r.api(admin, "PUT", taskPath(h.LegacyTask)+"/members/zhangsan", map[string]any{"roles": []string{db.RoleTechWriter, db.RoleReviewer}})
	r.api(admin, "PUT", taskPath(h.LegacyTask)+"/members/lisi", map[string]any{"roles": []string{db.RoleReviewer}})
	r.pass("升级后用运维工具创建第一个管理员并登录，建账号、给旧项目添加成员", []string{"s7-admin", "s7-lead", "zhangsan", "lisi"})

	zs, ls := r.as(p, "zhangsan", pw["zhangsan"]), r.as(p, "lisi", pw["lisi"])
	approve := fmt.Sprintf("%s/sections/%d/storyboard/approve", taskPath(h.LegacyTask), h.LegacySection)
	note := map[string]string{"review_note": "升级演练"}
	steps := map[string]any{}
	for _, who := range []struct {
		name string
		p    *process
	}{{"zhangsan", zs}, {"lisi", ls}} {
		code, errCode := r.status(who.p, "POST", approve, note, nil)
		steps["unmapped_"+who.name] = []any{code, errCode}
		r.assert(code == 409 && errCode == "legacy_actor", fmt.Sprintf("未映射的旧姓名不能参与判定（%s）：%d %s", who.name, code, errCode))
	}
	r.api(admin, "POST", "/api/legacy-actors", map[string]any{"legacy_name": "张三", "login": "zhangsan"})
	code, errCode := r.status(zs, "POST", approve, note, map[string]string{"X-Autobid-User": "lisi"})
	steps["mapped_self"] = []any{code, errCode}
	r.assert(code == 422 && errCode == "pink_self_approval", fmt.Sprintf("映射后本人（即使请求头冒充他人）也不能批准自己的故事板：%d %s", code, errCode))
	r.api(ls, "POST", approve, note)
	d, e := ops.Open(r.ctx, dsn)
	r.must(e)
	defer d.Close()
	var by string
	r.must(d.QueryRowContext(r.ctx, `SELECT approved_by FROM section_storyboards WHERE section_id=$1 AND status='pink_approved'`, h.LegacySection).Scan(&by))
	r.assert(by == "lisi", "批准人应记为登录账号 lisi，实际 "+by)
	r.pass("遗留姓名：未映射时任何人都不能据此批准；管理员映射后本人不能批准、另一账号可以", steps)

	lead := r.as(p, "s7-lead", pw["s7-lead"])
	b, e := json.Marshal(map[string]any{"lead": map[string]string{"login": "s7-lead", "password": pw["s7-lead"], "display_name": "投标经理"}})
	r.must(e)
	r.accountsFile = filepath.Join(r.tmp, "accounts.json")
	r.must(os.WriteFile(r.accountsFile, b, 0600))
	r.t.Cleanup(func() { _ = os.Remove(r.accountsFile) })
	return lead
}
