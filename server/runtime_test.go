package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/bidtools"
	"github.com/autobid/autobid-engine/export"
	"github.com/autobid/autobid-engine/ingest"
	"github.com/autobid/autobid-engine/toolhealth"
)

type runtimeList struct {
	Capabilities []toolhealth.Status `json:"capabilities"`
}

func newRuntimeAPI(t *testing.T, cfg APIConfig) *apiClient {
	t.Helper()
	pg := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(pg)
	e := NewEngine(m, func(*Task) (*agent.Planner, *agent.Worker) { return nil, nil }, Config{Workers: 1})
	api := testAPI(ctx, m, e, NewLLMService(pg, LLMServiceConfig{}), nil, NewHub(), cfg)
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		srv.Close()
		e.StopAll()
		cancel()
	})
	return &apiClient{t: t, url: srv.URL}
}

// GET /api/runtime/tools 与 tool_runtime_status 读取同一个注册表：状态、影响与下一步可见，原因不含宿主路径。
func TestRuntimeToolsAPIAndTool(t *testing.T) {
	reg := toolhealth.New()
	reg.Record(toolhealth.Office, toolhealth.Result{Provider: "wps", Engine: "wps", Version: "WPS 12.1.0.29161"}, 3*time.Second, nil)
	reg.Record(toolhealth.Mermaid, toolhealth.Result{Provider: "mmdc", Version: "11.17.0"}, time.Second,
		errors.New(`self-test architecture: mmdc: Failed to launch the browser D:\Pycharm Project\autobid\tools\chrome.exe`))
	c := newRuntimeAPI(t, APIConfig{Runtime: reg})

	var all runtimeList
	if code := c.raw("GET", "/api/runtime/tools", nil, &all); code != 200 || len(all.Capabilities) != len(toolhealth.All) {
		t.Fatalf("runtime tools: %d %+v", code, all)
	}
	byCap := map[toolhealth.Capability]toolhealth.Status{}
	for _, s := range all.Capabilities {
		byCap[s.Capability] = s
	}
	if o := byCap[toolhealth.Office]; o.State != toolhealth.Healthy || !o.Available || o.Engine != "wps" || o.CheckedAt == nil {
		t.Fatalf("office = %+v", o)
	}
	mm := byCap[toolhealth.Mermaid]
	if mm.State != toolhealth.SelfTestFailed || mm.Available || mm.Impact == "" || mm.NextAction == "" || strings.Contains(mm.Reason, "Pycharm") {
		t.Fatalf("mermaid = %+v", mm)
	}
	if byCap[toolhealth.OCR].State != toolhealth.Starting {
		t.Fatalf("ocr before probe = %+v", byCap[toolhealth.OCR])
	}
	var one runtimeList
	if code := c.raw("GET", "/api/runtime/tools?capability=office", nil, &one); code != 200 || len(one.Capabilities) != 1 || one.Capabilities[0].Capability != toolhealth.Office {
		t.Fatalf("office only: %d %+v", code, one)
	}
	if code := c.raw("GET", "/api/runtime/tools?capability=bogus", nil, nil); code != 400 {
		t.Fatalf("bogus capability: %d", code)
	}

	// Agent 工具与 HTTP 同源。
	tl := bidtools.RuntimeStatusTool(reg)
	res, err := tl.Call(context.Background(), json.RawMessage(`{"capability":"mermaid"}`), nil)
	if err != nil || res.IsError {
		t.Fatalf("tool: %v %+v", err, res)
	}
	var fromTool runtimeList
	if err := json.Unmarshal([]byte(res.Content), &fromTool); err != nil || len(fromTool.Capabilities) != 1 || fromTool.Capabilities[0].Reason != mm.Reason {
		t.Fatalf("tool result = %s", res.Content)
	}
	if res, _ := tl.Call(context.Background(), json.RawMessage(`{"capability":"gpu"}`), nil); !res.IsError {
		t.Fatalf("unknown capability accepted: %+v", res)
	}
	if got := bidtools.New(nil, 0, nil, nil).RuntimeTools(); len(got) != 0 {
		t.Fatal("kit without runtime registry must not expose tool_runtime_status")
	}
}

// 未注入 Runtime 时按导出服务现状生成：没有执行自检的能力不宣称可用。
func TestRuntimeToolsFallback(t *testing.T) {
	svc := export.NewService(nil, t.TempDir(), "测试：未探测")
	svc.Renderer = pngRenderer{}
	c := newRuntimeAPI(t, APIConfig{Export: svc})
	var all runtimeList
	if code := c.raw("GET", "/api/runtime/tools", nil, &all); code != 200 {
		t.Fatalf("runtime tools: %d", code)
	}
	for _, s := range all.Capabilities {
		want := toolhealth.Unconfigured
		if s.Capability == toolhealth.Mermaid {
			want = toolhealth.Healthy
		}
		if s.State != want {
			t.Errorf("%s = %s, want %s", s.Capability, s.State, want)
		}
	}
}

// ProbeRuntime：已配置的 OCR / 重排模型在后台实际自检，未配置的立即标为 unconfigured。
func TestProbeRuntime(t *testing.T) {
	ocrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet { // S5a protocol: GET reports the execution version pinned by parse jobs
			_ = json.NewEncoder(w).Encode(map[string]string{"engine": "fake", "version": "fake-ocr 1"})
			return
		}
		_ = json.NewEncoder(w).Encode(ingest.OCRPage{Width: 720, Height: 200, Lines: []ingest.OCRLine{{Text: "招标文件自检 2026", BBox: [4]float64{40, 30, 380, 90}}}})
	}))
	defer ocrSrv.Close()
	reg := toolhealth.New()
	ProbeRuntime(context.Background(), reg, &ingest.HTTPOCR{URL: ocrSrv.URL}, nil, invertedRanker{})
	if st := reg.Get(toolhealth.Embedding); st.State != toolhealth.Unconfigured || st.Provider == "" {
		t.Fatalf("embedding = %+v", st)
	}
	if st := reg.Get(toolhealth.EnhancedPDF); st.State != toolhealth.Unconfigured {
		t.Fatalf("enhanced_pdf = %+v", st)
	}
	deadline := time.Now().Add(5 * time.Second)
	for (reg.Get(toolhealth.OCR).State == toolhealth.Starting || reg.Get(toolhealth.Reranker).State == toolhealth.Starting) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st := reg.Get(toolhealth.OCR); st.State != toolhealth.Healthy || st.Provider != "http" || st.Version != "fake-ocr 1" {
		t.Fatalf("ocr = %+v", st)
	}
	if st := reg.Get(toolhealth.Reranker); st.State != toolhealth.SelfTestFailed || st.Version != "inverted@1" {
		t.Fatalf("reranker = %+v", st)
	}
}

type invertedRanker struct{}

func (invertedRanker) Model() string { return "inverted@1" }
func (invertedRanker) Rank(context.Context, string, []string) ([]float64, error) {
	return []float64{0.1, 0.9}, nil
}
