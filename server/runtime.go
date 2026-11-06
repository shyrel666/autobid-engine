package server

// 本机运行能力状态（AGENT_TOOL_SKILLS_PLAN S1）。
//   GET /api/runtime/tools   ?capability=all|office|mermaid|ocr|enhanced_pdf|embedding|reranker
// 与 Agent 工具 tool_runtime_status 读取同一个 toolhealth.Registry；原因已脱敏，不含宿主路径与凭据。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/export"
	"github.com/autobid/autobid-engine/ingest"
	"github.com/autobid/autobid-engine/retrieval"
	"github.com/autobid/autobid-engine/toolhealth"
)

func (a *API) registerRuntimeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/runtime/tools", a.runtimeTools)
}

func (a *API) runtimeTools(w http.ResponseWriter, r *http.Request) {
	c := toolhealth.Capability(r.URL.Query().Get("capability"))
	var list []toolhealth.Status
	switch {
	case c == "" || c == "all":
		list = a.cfg.Runtime.List()
	case toolhealth.Valid(c):
		list = a.cfg.Runtime.List(c)
	default:
		writeError(w, http.StatusBadRequest, errors.New("unknown capability"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"capabilities": list, "now": time.Now()})
}

// fallbackRuntime 在未注入 Runtime（测试、嵌入使用）时按导出服务现状生成状态：文档引擎与 mmdc 按是否可用记录，
// 其余能力记为未配置（没有执行自检，不宣称可用）。
func fallbackRuntime(svc *export.Service) *toolhealth.Registry {
	reg := toolhealth.New()
	st := svc.Status()
	if st.Office {
		reg.Record(toolhealth.Office, toolhealth.Result{Provider: st.OfficeEngine, Engine: st.OfficeEngine, Version: st.OfficeVersion}, 0, nil)
	} else {
		reg.SetUnconfigured(toolhealth.Office, "", st.OfficeReason)
	}
	if st.Renderer {
		reg.Record(toolhealth.Mermaid, toolhealth.Result{Provider: st.RendererName}, 0, nil)
	} else {
		reg.SetUnconfigured(toolhealth.Mermaid, "", st.RendererReason)
	}
	for _, c := range []toolhealth.Capability{toolhealth.OCR, toolhealth.EnhancedPDF, toolhealth.Embedding, toolhealth.Reranker} {
		reg.SetUnconfigured(c, "", "服务未执行运行能力自检")
	}
	return reg
}

// ProbeRuntime 后台自检 OCR、embedding、reranker（文档引擎与 mmdc 由 export.NewServiceFromEnv 自检），
// 增强 PDF 解析后端尚未交付，记为未配置。自检期间状态为 starting；本函数立即返回。
func ProbeRuntime(ctx context.Context, reg *toolhealth.Registry, ocr ingest.OCRProvider, emb db.Embedder, rank retrieval.Reranker) {
	reg.SetUnconfigured(toolhealth.EnhancedPDF, "", "增强 PDF 解析后端（MinerU 等）尚未交付；当前使用内置 PDF 解析器")
	if ocr == nil {
		ocr = ingest.UnavailableOCR{}
	}
	if _, ok := ocr.(ingest.UnavailableOCR); ok {
		reg.SetUnconfigured(toolhealth.OCR, ocr.Name(), ingest.ErrOCRUnavailable.Error())
	} else {
		go reg.Probe(ctx, toolhealth.OCR, ocr.Name(), func(ctx context.Context) (toolhealth.Result, error) {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			// The local OCR service (tools/paddleocr) may still be loading its models when the server starts:
			// a refused connection or 503 is retried until the self-test deadline instead of failing at once.
			for {
				err := ingest.OCRSelfTest(ctx, ocr)
				if err == nil {
					v, verr := ingest.OCRVersion(ctx, ocr)
					if verr != nil {
						return toolhealth.Result{}, fmt.Errorf("self-test passed but the OCR version is unavailable: %w", verr)
					}
					return toolhealth.Result{Version: v}, nil
				}
				if !errors.Is(err, ingest.ErrOCRTransient) {
					return toolhealth.Result{}, err
				}
				select {
				case <-ctx.Done():
					return toolhealth.Result{}, err
				case <-time.After(2 * time.Second):
				}
			}
		})
	}
	if emb == nil {
		reg.SetUnconfigured(toolhealth.Embedding, db.DefaultEmbedder().Model(), "未配置语义向量模型（AUTOBID_EMBEDDING_URL）")
	} else {
		go reg.Probe(ctx, toolhealth.Embedding, emb.Model(), func(ctx context.Context) (toolhealth.Result, error) {
			err := retryModelSelfTest(ctx, func(ctx context.Context) error { return retrieval.SelfTestEmbedder(ctx, emb) })
			return toolhealth.Result{Version: modelServiceVersion(emb)}, err
		})
	}
	if rank == nil {
		reg.SetUnconfigured(toolhealth.Reranker, "", "未配置语义重排模型（AUTOBID_RERANKER_URL）")
	} else {
		go reg.Probe(ctx, toolhealth.Reranker, rank.Model(), func(ctx context.Context) (toolhealth.Result, error) {
			err := retryModelSelfTest(ctx, func(ctx context.Context) error { return retrieval.SelfTestReranker(ctx, rank) })
			return toolhealth.Result{Version: modelServiceVersion(rank)}, err
		})
	}
}

func modelServiceVersion(model interface{ Model() string }) string {
	v := model.Model()
	if m, ok := model.(*retrieval.HTTPModel); ok && m.Info().ServedVersion != "" {
		v += " (service " + m.Info().ServedVersion + ")"
	}
	return toolhealth.SanitizeModel(v)
}

func retryModelSelfTest(ctx context.Context, test func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		err := test(ctx)
		if !errors.Is(err, retrieval.ErrModelTransient) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(2 * time.Second):
		}
	}
}
