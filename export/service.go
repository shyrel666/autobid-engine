package export

// Service 组合导出目录与 mmdc 渲染器：图表生成、渲染与读取。

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"log/slog"
	"strings"

	"github.com/autobid/autobid-engine/db"
)

// Service 是图表的服务端依赖。Renderer 为 nil 表示 mmdc 不可用（原因见 RendererReason）。
type Service struct {
	DB             *db.DB
	Store          *Store
	Renderer       Renderer
	RendererReason string
}

// Status 是外部依赖可用性（门禁明细与前端展示）。
type Status struct {
	Renderer       bool   `json:"renderer"`
	RendererName   string `json:"renderer_name,omitempty"`
	RendererReason string `json:"renderer_reason,omitempty"`
}

func (s *Service) Status() Status {
	st := Status{RendererReason: s.RendererReason}
	if s.Renderer != nil {
		st.Renderer, st.RendererName, st.RendererReason = true, s.Renderer.Name(), ""
	}
	return st
}

// NewServiceFromEnv 探测 mmdc（不可用时记录原因，不报错）。
func NewServiceFromEnv(pg *db.DB, dir string, logger *slog.Logger) *Service {
	s := &Service{DB: pg, Store: &Store{Root: dir}}
	if m, err := DetectMMDC(); err != nil {
		s.RendererReason = err.Error()
	} else {
		s.Renderer = m
	}
	if logger != nil {
		if s.Renderer != nil {
			logger.Info("mermaid-cli available for charts", "renderer", s.Renderer.Name())
		} else {
			logger.Warn("mermaid-cli unavailable: Mermaid charts stay unrendered and export as source placeholders", "reason", s.RendererReason)
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// 图表
// ---------------------------------------------------------------------------

// ChartRequest 是一次图表生成请求。
type ChartRequest struct {
	SectionID int64           `json:"section_id"`
	ChartType string          `json:"chart_type"`
	Title     string          `json:"title"`
	Spec      json.RawMessage `json:"spec"`
}

// GenerateChart 校验规格、生成源码、绑定章节当前修订并尝试渲染。
func (s *Service) GenerateChart(ctx context.Context, tenant string, taskID int64, actor string, req ChartRequest) (*db.ProposalChart, error) {
	src, err := BuildChartSource(req.ChartType, req.Title, req.Spec)
	if err != nil {
		return nil, err
	}
	c, err := s.DB.CreateChart(ctx, tenant, taskID, actor, db.ChartInput{SectionID: req.SectionID, ChartType: req.ChartType,
		Title: strings.TrimSpace(req.Title), Spec: src.Spec, SourceFormat: src.Format, Source: src.Source, SourceHash: src.Hash})
	if err != nil {
		return nil, err
	}
	return s.RenderChart(ctx, tenant, taskID, c, actor)
}

// RenderChart 渲染（或重新渲染）一张图表并写回状态。
func (s *Service) RenderChart(ctx context.Context, tenant string, taskID int64, c *db.ProposalChart, actor string) (*db.ProposalChart, error) {
	var r db.ChartRender
	switch {
	case c.SourceFormat == db.ChartSourceJSON:
		r = db.ChartRender{Status: db.ChartRendered, Format: db.ChartFormatNative}
	case s.Renderer == nil:
		reason := s.RendererReason
		if reason == "" {
			reason = ErrRendererMissing.Error()
		}
		r = db.ChartRender{Status: db.ChartUnrendered, Error: "mmdc 不可用：" + reason}
	default:
		png, err := s.Renderer.RenderPNG(ctx, c.Source)
		if err != nil {
			r = db.ChartRender{Status: db.ChartFailed, Error: truncateRunes(err.Error(), 500)}
			break
		}
		cfg, _, _ := image.DecodeConfig(bytes.NewReader(png))
		sum := sha256Hex(png)
		rel := s.Store.chartRel(tenant, taskID, sum)
		if _, err := s.Store.WriteFile(rel, png); err != nil {
			return nil, err
		}
		r = db.ChartRender{Status: db.ChartRendered, Format: db.ChartFormatPNG, ArtifactPath: rel, ArtifactHash: sum,
			ArtifactSize: int64(len(png)), WidthPx: cfg.Width, HeightPx: cfg.Height}
	}
	return s.DB.SetChartRender(ctx, tenant, taskID, c.ID, c.SourceHash, r, actor)
}

// ChartImage 读取已渲染图表的 PNG。
func (s *Service) ChartImage(c *db.ProposalChart) ([]byte, error) {
	if c.RenderStatus != db.ChartRendered || c.RenderFormat != db.ChartFormatPNG || c.ArtifactPath == "" {
		return nil, db.ErrNotFound
	}
	return s.Store.ReadFile(c.ArtifactPath, c.ArtifactHash)
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
