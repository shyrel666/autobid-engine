package export

// S4（AGENT_TOOL_SKILLS_PLAN 4.1 / 4.3 / 5.2）：导出请求组的服务层（HTTP 与 Agent 工具共用）。
//   - 规范化客户端输入（格式去重排序、模板、投标人、期望大纲版本、导出策略）并求请求哈希，策略版本计入哈希；
//   - 同键重放返回原请求组，不受当前运行能力影响；只有新请求检查运行条件：最终稿要求文档引擎（WPS / Word）可用，并在首次
//     提交时固定其执行版本（executor），之后执行时版本不符即失败，不随配置漂移；
//   - 提交后只唤醒导出执行者（作业已持久化），执行在 Runner 中、数据库事务外完成。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/toolhealth"
)

// 导出模式：最终稿强制严格运行策略（strict_runtime=true），预览稿保持 Phase 6B 行为。
const (
	ExportModeFinal   = "final"
	ExportModePreview = "preview"
)

// ExportJobDeadline 是导出作业从首次提交起的截止时长（AUTOBID_EXPORT_JOB_DEADLINE_SECONDS，默认 1800 秒）。
func ExportJobDeadline() time.Duration {
	return time.Duration(envInt("AUTOBID_EXPORT_JOB_DEADLINE_SECONDS", int(db.DefaultExportJobDeadline/time.Second))) * time.Second
}

// ExportRequest 是一次导出提交（HTTP 与 Agent 工具共用）。request_key 由调用方生成，同一次提交的重发复用，改变输入或明确
// 重新执行时换新键。Agent 工具固定 Mode=final，模型无权关闭严格策略。
type ExportRequest struct {
	Formats                []string `json:"formats"`
	TemplateID             int64    `json:"template_id"`
	Bidder                 string   `json:"bidder"`
	ExpectedOutlineVersion int      `json:"expected_outline_version"`
	Mode                   string   `json:"mode"`
	RequestKey             string   `json:"request_key"`
}

// Strict 报告请求是否为严格最终稿。
func (r ExportRequest) Strict() bool { return r.Mode == ExportModeFinal }

// ExportPolicy 返回请求对应的显式策略版本。
func (r ExportRequest) ExportPolicy() string {
	if r.Strict() {
		return db.ExportPolicyFinal
	}
	return db.ExportPolicyPreviewV1
}

// normalize 校验并规范化请求；未给出 mode 时按预览稿处理（旧兼容结果不默认为最终稿）。
func (r *ExportRequest) normalize() error {
	r.Mode = strings.ToLower(strings.TrimSpace(r.Mode))
	if r.Mode == "" {
		r.Mode = ExportModePreview
	}
	if r.Mode != ExportModeFinal && r.Mode != ExportModePreview {
		return fmt.Errorf("%w: mode must be %q or %q", db.ErrInvalidExport, ExportModeFinal, ExportModePreview)
	}
	formats, err := db.NormalizeExportFormats(r.Formats)
	if err != nil {
		return err
	}
	r.Formats, r.Bidder, r.RequestKey = formats, strings.TrimSpace(r.Bidder), strings.TrimSpace(r.RequestKey)
	switch {
	case r.TemplateID < 0 || r.ExpectedOutlineVersion < 0:
		return fmt.Errorf("%w: template_id and expected_outline_version must not be negative", db.ErrInvalidExport)
	case len([]rune(r.Bidder)) > 200:
		return fmt.Errorf("%w: bidder must be at most 200 characters", db.ErrInvalidExport)
	}
	return db.ValidExportRequestKey(r.RequestKey)
}

// hash 是规范化客户端输入在给定策略版本下的请求哈希。
func (r ExportRequest) hash(policy string) string {
	return requestHash("export", r.Formats, r.TemplateID, r.Bidder, r.ExpectedOutlineVersion, policy)
}

// officeExecutor 返回新的最终稿请求固定的执行版本；文档引擎不可用时拒绝提交（RUNTIME_UNAVAILABLE），不降级为预览稿。
func (s *Service) officeExecutor() (string, error) {
	if s != nil && s.Office != nil {
		return OfficeLabel(s.Office), nil
	}
	reason := ""
	if s != nil {
		reason = strings.TrimPrefix(s.OfficeReason, ErrOfficeUnavailable.Error()+": ")
	}
	if reason == "" {
		return "", ErrOfficeUnavailable
	}
	return "", fmt.Errorf("%w: %s", ErrOfficeUnavailable, toolhealth.Sanitize(reason))
}

// SubmitExport 校验并提交一次导出请求组。宿主绑定 Intent 的调用（Agent 工具）须由宿主授予任务导出能力，并同事务订阅全部作业。
func (s *Service) SubmitExport(ctx context.Context, tenant string, taskID int64, actor string, req ExportRequest) (*db.ExportSubmission, error) {
	if err := req.normalize(); err != nil {
		return nil, err
	}
	in := db.SubmitExportInput{RequestKey: req.RequestKey, Formats: req.Formats, TemplateID: req.TemplateID, DefaultTemplateHash: DefaultTemplateHash(),
		Bidder: req.Bidder, ExpectedOutlineVersion: req.ExpectedOutlineVersion, StrictRuntime: req.Strict(), Deadline: ExportJobDeadline(), Actor: actor,
		RequestHash: req.hash(req.ExportPolicy())}
	// 同键重放返回原请求组，不受当前运行能力与当前策略版本影响；只有新的最终稿请求检查投标人名称与文档引擎并固定执行版本。
	switch prev, err := s.DB.FindExportRequest(ctx, tenant, taskID, req.RequestKey); {
	case errors.Is(err, db.ErrNotFound):
		if in.StrictRuntime {
			// D4：最终稿的封面与文档属性都用投标人名称；没有它的成稿过不了提交检查，只能重导，所以在提交时就拒绝。
			if req.Bidder == "" {
				return nil, fmt.Errorf("%w: a final draft needs the bidder name (it goes on the cover and into the document properties)", db.ErrInvalidExport)
			}
			exec, err := s.officeExecutor()
			if err != nil {
				return nil, err
			}
			in.Executor = exec
		}
	case err != nil:
		return nil, err
	default:
		// 请求哈希含策略版本。重放按原请求提交时的版本求哈希：策略升级（如 export-final/v1 → v2）不应把同一请求的重发
		// 判成“同键异参”。严格 / 预览不同仍是不同的输入。
		if prev.StrictRuntime == in.StrictRuntime {
			in.RequestHash = req.hash(prev.PolicyVersion)
		}
	}
	sub, err := s.DB.SubmitExport(ctx, tenant, taskID, in)
	if err == nil && !sub.Replayed {
		s.WakeExportRunner()
	}
	return sub, err
}

// WakeExportRunner 非阻塞地唤醒导出执行者。
func (s *Service) WakeExportRunner() {
	if s != nil && s.ExportWake != nil {
		s.ExportWake()
	}
}
