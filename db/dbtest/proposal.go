// Package dbtest 为跨包集成测试构造「可导出」的标书：要求清单确认 -> 大纲批准 -> 章节正文写回并批准 ->
// 响应矩阵全部核验通过 -> 一致性核验无结论（只经 db 包公开 API，真实 PostgreSQL，需要 AUTOBID_TEST_DATABASE_URL）。
package dbtest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/compliance"
	"github.com/autobid/autobid-engine/db"
)

var (
	once  sync.Once
	pg    *db.DB
	pgErr error
	seq   atomic.Int64
)

// Open 打开并迁移测试库；未设置 AUTOBID_TEST_DATABASE_URL 时跳过并写明缺少的依赖。
func Open(t testing.TB) *db.DB {
	t.Helper()
	dsn := os.Getenv("AUTOBID_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("依赖缺失：AUTOBID_TEST_DATABASE_URL 未设置（需要真实 PostgreSQL）")
	}
	once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		pg, pgErr = db.Open(ctx, dsn)
		if pgErr == nil {
			pgErr = pg.Migrate(ctx)
		}
	})
	if pgErr != nil {
		t.Fatalf("open test db: %v", pgErr)
	}
	return pg
}

// Uniq 返回唯一名称。
func Uniq(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), seq.Add(1))
}

func strp(s string) *string { return &s }

// Req 是一条要求。
type Req struct {
	Code, Category, Title, Description string
	Mandatory                          bool
}

// Section 是一个大纲章节及其正文。
type Section struct {
	Code, Title string
	Level       int
	Body        string
	Reqs        []string
}

// Spec 是一份标书。
type Spec struct {
	Name     string
	Reqs     []Req
	Sections []Section
}

// Proposal 是构造好的可导出标书。
type Proposal struct {
	DB             *db.DB
	TaskID         int64
	OutlineVersion int
	Reqs           map[string]int64
	Sections       map[string]int64 // 章节编码 -> ID
	Spec           Spec
}

var scoringSubjects = []string{"视频监控", "门禁管理", "停车诱导", "能耗监测", "消防联动", "访客管理", "电子巡更", "广播对讲"}
var scoringAspects = []string{"设备接入", "告警处置", "统计分析", "移动应用", "权限管理"}

// DefaultSpec 返回一份含 ★ 条款、评分项与格式项的标书；scoring 为额外评分项条数（用于长偏离表，最多 40 条）。
// 正文中只出现一致的关键数值（总工期 90 天、质保期 5 年、响应 1 小时），保证一致性核验无矛盾。
func DefaultSpec(scoring int) Spec {
	long := strings.Repeat("本系统采用分层解耦架构，感知层、网络层、平台层与应用层之间通过标准接口交互，保证系统的可扩展性与可维护性。", 10)
	s := Spec{Name: Uniq("6B 智慧园区安防项目"),
		Reqs: []Req{
			{"★1", db.ReqMandatoryClause, "★ 总工期", "★ 投标人须承诺总工期不超过120日历天，否则投标无效。", true},
			{"★2", db.ReqMandatoryClause, "★ 售后服务响应", "★ 投标人须提供7×24小时运维服务，故障响应时间不超过2小时。", true},
			{"★3", db.ReqMandatoryClause, "★ 质保期", "★ 投标人须承诺免费质保期不少于3年。", true},
			{"S1", db.ReqScoringItem, "技术方案", "总体架构合理、技术路线先进得10分。", false},
			{"F1", db.ReqFormatAttachment, "投标文件签章", "投标文件须逐页签字盖章。", false},
		},
	}
	var feature strings.Builder
	feature.WriteString("## 功能设计\n\n")
	var featureReqs []string
	for i := 0; i < scoring && i < len(scoringSubjects)*len(scoringAspects); i++ {
		subj, asp := scoringSubjects[i/len(scoringAspects)], scoringAspects[i%len(scoringAspects)]
		code := fmt.Sprintf("S%02d", i+2)
		s.Reqs = append(s.Reqs, Req{code, db.ReqScoringItem, subj + asp, fmt.Sprintf("系统具备%s%s功能，支持分级管理与数据统计分析得1分。", subj, asp), false})
		featureReqs = append(featureReqs, code)
		fmt.Fprintf(&feature, "本方案的%s子系统提供%s能力，采用分布式部署，支持按园区、楼栋两级分级管理，并提供按日、按月的数据统计分析报表，满足%s%s业务需要。\n\n", subj, asp, subj, asp)
	}
	s.Sections = []Section{
		{Code: "1", Title: "总体技术方案", Level: 1, Reqs: []string{"★1", "S1"},
			Body: "## 总体技术方案\n\n本项目采用云边协同总体架构，总体架构合理、技术路线先进，我方承诺总工期 90 天，严格按计划交付。\n\n" + long},
		{Code: "1.1", Title: "系统总体架构", Level: 2, Body: long + "\n\n- **安全可靠**：满足等级保护要求\n- 统一运维\n  - 集中告警\n  - 自动巡检\n\n系统总体架构见下图。"},
		{Code: "1.2", Title: "功能设计", Level: 2, Reqs: featureReqs, Body: feature.String() + long},
		{Code: "2", Title: "售后服务方案", Level: 1, Reqs: []string{"★2"}, Body: "我方承诺提供 7×24 小时运维服务，故障响应时间不超过 1 小时，4 小时内到场。\n\n" + long},
		{Code: "3", Title: "质量保证", Level: 1, Reqs: []string{"★3"}, Body: "我方承诺免费质保期 5 年，优于招标要求。\n\n" + long},
		{Code: "4", Title: "投标文件格式", Level: 1, Reqs: []string{"F1"}, Body: "我方投标文件已逐页签字并加盖公章，正本一份、副本四份。"},
		{Code: "5", Title: "实施进度计划", Level: 1, Body: "实施进度安排：工期 90 天，分三个阶段完成。\n\n" + long + "\n\n实施进度安排见甘特图。"},
		{Code: "6", Title: "项目组织与人员", Level: 1, Body: long},
	}
	if len(featureReqs) == 0 {
		s.Sections[2].Body = long
	}
	return s
}

// WithWideTable 在章节 code 的正文末尾追加一张 cols 列、rows 行的 Markdown 宽表格。
func (s Spec) WithWideTable(code string, cols, rows int) Spec {
	var b strings.Builder
	b.WriteString("\n\n前端设备选型如下表所示：\n\n|")
	heads := []string{"序号", "设备名称", "品牌", "型号", "分辨率", "帧率", "编码", "存储", "接口", "功耗", "防护", "质保", "数量", "备注", "单价"}
	for c := 0; c < cols; c++ {
		h := fmt.Sprintf("参数%d", c+1)
		if c < len(heads) {
			h = heads[c]
		}
		b.WriteString(" " + h + " |")
	}
	b.WriteString("\n|" + strings.Repeat("---|", cols) + "\n")
	vals := []string{"", "高清网络摄像机", "海康威视", "DS-2CD3T47WDV3-L", "400万像素 2560×1440", "25fps", "H.265+", "256GB", "RJ45×1", "6.5W", "IP67", "整机保修", "", "室外枪机", "按清单"}
	for r := 1; r <= rows; r++ {
		b.WriteString("|")
		for c := 0; c < cols; c++ {
			v := fmt.Sprintf("值%d-%d", r, c+1)
			switch {
			case c == 0 || c == 12:
				v = fmt.Sprint(r)
			case c < len(vals):
				v = vals[c]
			}
			b.WriteString(" " + v + " |")
		}
		b.WriteString("\n")
	}
	b.WriteString("\n上述设备均满足招标技术参数要求。")
	for i := range s.Sections {
		if s.Sections[i].Code == code {
			s.Sections[i].Body += b.String()
		}
	}
	return s
}

// Ready 构造可导出的标书；未达到导出就绪时以全部阻断原因 Fatal。
func Ready(t testing.TB, d *db.DB, spec Spec) *Proposal {
	t.Helper()
	ctx := context.Background()
	// 导出夹具构造的是不含故事板的旧基线标书。
	task, err := d.CreateTask(ctx, db.CreateTaskInput{Name: spec.Name, Goal: "技术标导出", LegacyWriting: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	p := &Proposal{DB: d, TaskID: task.ID, Reqs: map[string]int64{}, Sections: map[string]int64{}, Spec: spec}
	for _, r := range spec.Reqs {
		mand := r.Mandatory
		it, err := d.CreateRequirement(ctx, db.DefaultTenant, task.ID, "reviewer", db.RequirementInput{Code: strp(r.Code), Category: strp(r.Category),
			Title: strp(r.Title), Description: strp(r.Description), IsMandatory: &mand})
		if err != nil {
			t.Fatalf("CreateRequirement %s: %v", r.Code, err)
		}
		p.Reqs[r.Code] = it.ID
	}
	if _, err := d.ConfirmRequirements(ctx, db.DefaultTenant, task.ID, "reviewer", 0); err != nil {
		t.Fatalf("ConfirmRequirements: %v", err)
	}
	var secs []db.SectionInput
	for _, s := range spec.Sections {
		in := db.SectionInput{Code: s.Code, Title: s.Title, Level: s.Level}
		for _, c := range s.Reqs {
			in.RequirementIDs = append(in.RequirementIDs, p.Reqs[c])
		}
		secs = append(secs, in)
	}
	det, err := d.CreateOutline(ctx, db.DefaultTenant, task.ID, "editor", db.OutlineInput{Sections: secs})
	if err != nil {
		t.Fatalf("CreateOutline: %v", err)
	}
	if _, err := d.SubmitOutline(ctx, db.DefaultTenant, task.ID, det.Outline.Version, "editor"); err != nil {
		t.Fatalf("SubmitOutline: %v", err)
	}
	if det, err = d.ApproveOutline(ctx, db.DefaultTenant, task.ID, det.Outline.Version, "approver"); err != nil {
		t.Fatalf("ApproveOutline: %v", err)
	}
	p.OutlineVersion = det.Outline.Version
	for _, s := range det.Sections {
		p.Sections[s.Code] = s.ID
	}
	for _, s := range spec.Sections {
		p.Save(t, s.Code, s.Body)
	}
	p.ApproveAll(t)
	if res, err := d.VerifyComplianceMatrix(ctx, db.DefaultTenant, task.ID, 0, compliance.RuleVerifier{}, "verifier"); err != nil {
		t.Fatalf("VerifyComplianceMatrix: %v", err)
	} else if res.Verified != res.Checked {
		for _, r := range res.Rows {
			if r.VerifyStatus != compliance.StatusVerified {
				t.Errorf("matrix row %s @ %s: %s %s", r.RequirementCode, r.SectionCode, r.VerifyStatus, r.FailureReason)
			}
		}
		t.Fatalf("matrix not fully verified: %d/%d", res.Verified, res.Checked)
	}
	cons, err := d.RunConsistencyCheck(ctx, db.DefaultTenant, task.ID, "reviewer")
	if err != nil {
		t.Fatalf("RunConsistencyCheck: %v", err)
	}
	if len(cons.Findings) != 0 {
		t.Fatalf("consistency findings: %+v", cons.Findings[0])
	}
	p.AssertReady(t)
	return p
}

// Save 以当前主版本为基线保存一版人工正文，返回新修订号。
func (p *Proposal) Save(t testing.TB, code, body string) int {
	t.Helper()
	ctx := context.Background()
	det, err := p.DB.GetSection(ctx, db.DefaultTenant, p.TaskID, p.Sections[code])
	if err != nil {
		t.Fatalf("GetSection %s: %v", code, err)
	}
	res, err := p.DB.SaveSectionRevision(ctx, db.DefaultTenant, "writer", db.SaveRevisionInput{TaskID: p.TaskID, SectionID: p.Sections[code],
		BaseRevNo: det.Section.CurrentRevNo, BasedOnOutlineVersion: p.OutlineVersion, Source: db.RevHumanEdit, ContentMD: body, SummaryNote: "摘要"})
	if err != nil || res.Outcome != db.SaveApplied {
		t.Fatalf("SaveSectionRevision %s: %v %+v", code, err, res)
	}
	return res.Revision.RevNo
}

// ApproveAll 批准全部章节的当前修订。
func (p *Proposal) ApproveAll(t testing.TB) {
	t.Helper()
	ctx := context.Background()
	for code, id := range p.Sections {
		det, err := p.DB.GetSection(ctx, db.DefaultTenant, p.TaskID, id)
		if err != nil {
			t.Fatal(err)
		}
		if det.Section.ApprovedRevNo != nil && *det.Section.ApprovedRevNo == det.Section.CurrentRevNo {
			continue
		}
		if _, err := p.DB.ApproveSectionRevision(ctx, db.DefaultTenant, p.TaskID, id, det.Section.CurrentRevNo, "approver"); err != nil {
			t.Fatalf("approve %s: %v", code, err)
		}
	}
}

// AssertReady 断言导出就绪。
func (p *Proposal) AssertReady(t testing.TB) {
	t.Helper()
	r, err := p.DB.ExportReadiness(context.Background(), db.DefaultTenant, p.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Ready {
		for _, b := range r.Blockers {
			t.Errorf("blocker %s: %s", b.Kind, b.Message)
		}
		t.Fatalf("proposal is not export-ready")
	}
}

// Headings 返回成稿中应出现的章节标题（编码 + 标题，按大纲顺序）。
func (p *Proposal) Headings() []string {
	out := make([]string, 0, len(p.Spec.Sections))
	for _, s := range p.Spec.Sections {
		out = append(out, s.Code+" "+s.Title)
	}
	return out
}
