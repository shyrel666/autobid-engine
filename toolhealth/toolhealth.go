// Package toolhealth 汇总本机运行组件（文档引擎、Mermaid-CLI、OCR、增强 PDF 解析、embedding / reranker）的可用性。
//
// 状态来自实际功能自检（固定小样本真实执行），而不是“程序已安装”；运行中的失败把健康组件降为 degraded，
// 下一次成功后恢复。HTTP（GET /api/runtime/tools）与 Agent 工具（tool_runtime_status）读取同一个 Registry。
// 原因文本经 Sanitize 脱敏：不含宿主路径、URL 查询串、令牌或密钥。
package toolhealth

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Capability 是一类运行能力。
type Capability string

const (
	Office      Capability = "office"
	Mermaid     Capability = "mermaid"
	OCR         Capability = "ocr"
	EnhancedPDF Capability = "enhanced_pdf"
	Embedding   Capability = "embedding"
	Reranker    Capability = "reranker"
)

// All 是全部能力（展示顺序）。
var All = []Capability{Office, Mermaid, OCR, EnhancedPDF, Embedding, Reranker}

// State 是能力状态。
type State string

const (
	Unconfigured   State = "unconfigured"     // 未安装 / 未配置
	Starting       State = "starting"         // 自检进行中
	Healthy        State = "healthy"          // 自检通过
	Degraded       State = "degraded"         // 自检通过，但最近一次实际调用失败
	SelfTestFailed State = "self_test_failed" // 已配置，自检失败
)

// Valid 报告 c 是否为已知能力。
func Valid(c Capability) bool {
	for _, x := range All {
		if x == c {
			return true
		}
	}
	return false
}

// Status 是一项能力的当前状态（对外 JSON）。
type Status struct {
	Capability Capability `json:"capability"`
	Label      string     `json:"label"`
	State      State      `json:"state"`
	Available  bool       `json:"available"` // healthy / degraded：可以提交，结果仍可能失败
	Provider   string     `json:"provider,omitempty"`
	Engine     string     `json:"engine,omitempty"` // office：wps / word
	Version    string     `json:"version,omitempty"`
	CheckedAt  *time.Time `json:"checked_at,omitempty"` // 最近一次自检完成时间
	DurationMS int64      `json:"duration_ms,omitempty"`
	FailedAt   *time.Time `json:"failed_at,omitempty"` // degraded：最近一次运行失败时间
	Reason     string     `json:"reason,omitempty"`
	Impact     string     `json:"impact,omitempty"`
	NextAction string     `json:"next_action,omitempty"`
}

// Result 是一次自检的结果。
type Result struct {
	Provider string
	Engine   string
	Version  string
}

// ErrUnconfigured 标记“未安装 / 未配置”，与“已配置但自检失败”区分。
var ErrUnconfigured = errors.New("toolhealth: capability is not configured")

type unconfigured struct{ err error }

func (u unconfigured) Error() string   { return u.err.Error() }
func (u unconfigured) Unwrap() []error { return []error{ErrUnconfigured, u.err} }

// AsUnconfigured 把 err 标记为“未配置”（errors.Is(err, ErrUnconfigured) 为真），错误文本不变。
func AsUnconfigured(err error) error {
	if err == nil {
		return nil
	}
	return unconfigured{err}
}

// Registry 保存各能力状态，并发安全。
type Registry struct {
	mu     sync.RWMutex
	states map[Capability]Status
	now    func() time.Time
}

// New 返回全部能力为 starting（检查中）的注册表。
func New() *Registry {
	r := &Registry{states: map[Capability]Status{}, now: time.Now}
	for _, c := range All {
		r.states[c] = Status{Capability: c, State: Starting}
	}
	return r
}

// SetUnconfigured 记录能力未配置（不需要自检）。
func (r *Registry) SetUnconfigured(c Capability, provider, reason string) {
	now := r.now()
	r.set(Status{Capability: c, State: Unconfigured, Provider: provider, CheckedAt: &now, Reason: Sanitize(reason)})
}

// Record 记录一次已完成的自检：err 为 nil 时 healthy；errors.Is(err, ErrUnconfigured) 时 unconfigured；否则 self_test_failed。
func (r *Registry) Record(c Capability, res Result, took time.Duration, err error) {
	now := r.now()
	st := Status{Capability: c, Provider: res.Provider, Engine: res.Engine, Version: res.Version, CheckedAt: &now, DurationMS: took.Milliseconds()}
	switch {
	case err == nil:
		st.State = Healthy
	case errors.Is(err, ErrUnconfigured):
		st.State, st.Reason = Unconfigured, Sanitize(err.Error())
	default:
		st.State, st.Reason = SelfTestFailed, Sanitize(err.Error())
	}
	r.set(st)
}

// Probe 把能力置为 starting，执行自检 fn 并记录结果。
func (r *Registry) Probe(ctx context.Context, c Capability, provider string, fn func(context.Context) (Result, error)) {
	r.mu.Lock()
	r.states[c] = Status{Capability: c, State: Starting, Provider: provider}
	r.mu.Unlock()
	start := r.now()
	res, err := fn(ctx)
	if res.Provider == "" {
		res.Provider = provider
	}
	r.Record(c, res, r.now().Sub(start), err)
}

// Degrade 记录运行中的一次失败：只把 healthy / degraded 能力降为 degraded，不改变自检结论。
func (r *Registry) Degrade(c Capability, err error) {
	if r == nil || err == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.states[c]
	if !ok || (st.State != Healthy && st.State != Degraded) {
		return
	}
	now := r.now()
	st.State, st.FailedAt, st.Reason = Degraded, &now, Sanitize(err.Error())
	r.states[c] = st
}

// Recover 记录运行中的一次成功：degraded 恢复为 healthy。
func (r *Registry) Recover(c Capability) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.states[c]; ok && st.State == Degraded {
		st.State, st.FailedAt, st.Reason = Healthy, nil, ""
		r.states[c] = st
	}
}

func (r *Registry) set(st Status) {
	r.mu.Lock()
	r.states[st.Capability] = st
	r.mu.Unlock()
}

// Get 返回一项能力的状态（含说明文字）。
func (r *Registry) Get(c Capability) Status {
	r.mu.RLock()
	st, ok := r.states[c]
	r.mu.RUnlock()
	if !ok {
		st = Status{Capability: c, State: Unconfigured}
	}
	return describe(st)
}

// List 返回指定能力（为空时全部）的状态，按 All 的顺序。
func (r *Registry) List(caps ...Capability) []Status {
	want := map[Capability]bool{}
	for _, c := range caps {
		want[c] = true
	}
	out := make([]Status, 0, len(All))
	for _, c := range All {
		if len(want) == 0 || want[c] {
			out = append(out, r.Get(c))
		}
	}
	return out
}

var labels = map[Capability]string{
	Office: "文档引擎（WPS / Word）", Mermaid: "图表渲染（Mermaid-CLI）", OCR: "OCR 识别",
	EnhancedPDF: "增强 PDF 解析", Embedding: "语义向量模型", Reranker: "语义重排模型",
}

// impacts 是能力不可用时对业务的影响。
var impacts = map[Capability]string{
	Office:      "PDF 导出与最终稿不可用；DOCX 只能作为预览导出（目录页码未刷新，打开时更新域）。",
	Mermaid:     "新生成的架构图、甘特图、组织图保持未渲染，导出时插入源码占位框；已渲染的图表与设备表不受影响。",
	OCR:         "扫描件与无文本层的 PDF 页面无法识别，这类文件上传会被拒绝，不会当作空白文档导入。",
	EnhancedPDF: "复杂 PDF 使用内置解析器（版面与框线表格还原）；增强解析后端尚未交付。",
	Embedding:   "语义通道不可用，知识库检索只使用全文与关键词召回。",
	Reranker:    "知识库检索结果不经语义重排，按融合得分排序。",
}

var nextActions = map[Capability]map[State]string{
	Office: {
		Unconfigured:   "在本机安装 WPS（优先）或 Word 后重启服务。",
		SelfTestFailed: "在本机打开 WPS / Word 确认可以正常使用（关闭激活、更新等弹窗）后重启服务。",
		Degraded:       "查看导出任务的失败原因；关闭卡住的 WPS / Word 窗口后重试导出。",
	},
	Mermaid: {
		Unconfigured:   "按 docs/LOCAL_RUNTIME.md 安装 Mermaid-CLI，并设置 AUTOBID_MMDC_PATH 与 AUTOBID_MMDC_BROWSER 后重启服务。",
		SelfTestFailed: "检查 AUTOBID_MMDC_PATH / AUTOBID_MMDC_BROWSER 指向的本机组件是否可用后重启服务。",
		Degraded:       "查看图表的失败原因后重新渲染；若持续失败，重启服务以重新自检。",
	},
	OCR: {
		Unconfigured:   "配置本机 OCR 服务（AUTOBID_OCR_URL）或安装 tesseract（含 chi_sim）后重启服务。",
		SelfTestFailed: "检查本机 OCR 服务是否运行、语言包是否完整后重启服务。",
		Degraded:       "查看文件处理任务的失败原因；OCR 服务恢复后重新提交。",
	},
	EnhancedPDF: {
		Unconfigured: "无需操作；增强解析后端按计划在后续阶段交付。",
	},
	Embedding: {
		Unconfigured:   "如需语义检索，按 eval/retrieval/README.md 配置 AUTOBID_EMBEDDING_* 并重建索引。",
		SelfTestFailed: "检查语义向量服务地址、模型名、维度与密钥配置后重启服务。",
		Degraded:       "检索会自动降级为全文与关键词召回；服务恢复后自动恢复。",
	},
	Reranker: {
		Unconfigured:   "如需语义重排，按 eval/retrieval/README.md 配置 AUTOBID_RERANKER_*。",
		SelfTestFailed: "检查语义重排服务地址、模型名与密钥配置后重启服务。",
		Degraded:       "检索会跳过重排；服务恢复后自动恢复。",
	},
}

func describe(st Status) Status {
	st.Label = labels[st.Capability]
	st.Available = st.State == Healthy || st.State == Degraded
	switch st.State {
	case Healthy:
		st.Impact, st.NextAction = "", ""
	case Starting:
		st.Impact, st.NextAction = "正在自检，结果出来前不要据此判断可用性。", "稍后刷新查看自检结果。"
	case Degraded:
		st.Impact = "最近一次调用失败，后续调用可能继续失败：" + impacts[st.Capability]
		st.NextAction = nextActions[st.Capability][Degraded]
	default:
		st.Impact = impacts[st.Capability]
		st.NextAction = nextActions[st.Capability][st.State]
	}
	return st
}

var (
	reURLQuery = regexp.MustCompile(`(https?://[^\s?#"']+)\?[^\s"']*`)
	reURLUser  = regexp.MustCompile(`(https?://)[^\s/@"']+@`)
	reWinPath  = regexp.MustCompile(`(?i)(^|[^a-z0-9])((?:[a-z]:[\\/]|\\\\)[^\s"'<>|*?]*(?:\s[^\s"'<>|*?\\/:]+[\\/][^\s"'<>|*?]*)*)`)
	reUnixPath = regexp.MustCompile(`(^|[\s"'=(])/(?:[\w.\-]+/)+[\w.\-]*`)
	reBearer   = regexp.MustCompile(`(?i)\bbearer\s+[\w.\-~+/=]+`)
	reKeyValue = regexp.MustCompile(`(?i)\b(api[_-]?key|token|secret|password|passwd|authorization)\b\s*[=:]\s*\S+`)
	reSKKey    = regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{8,}`)
)

// Sanitize 去掉原因文本中的宿主路径、URL 查询串与凭据，并截断到 300 字。
func Sanitize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = reURLUser.ReplaceAllString(s, "$1")
	s = reURLQuery.ReplaceAllString(s, "$1")
	s = reBearer.ReplaceAllString(s, "Bearer <redacted>")
	s = reKeyValue.ReplaceAllString(s, "$1=<redacted>")
	s = reSKKey.ReplaceAllString(s, "<redacted>")
	s = reWinPath.ReplaceAllString(s, "$1<path>")
	s = reUnixPath.ReplaceAllString(s, "$1<path>")
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "…"
	}
	return s
}

var reModelEndpoint = regexp.MustCompile(`https?://[^\s"'<>]+`)

// SanitizeModel also removes endpoint addresses from model diagnostics exposed to HTTP and agents.
func SanitizeModel(s string) string {
	return Sanitize(reModelEndpoint.ReplaceAllString(s, "<endpoint>"))
}
