package export

// Service 组合导出目录、mmdc 渲染器与文档引擎（WPS / Word）；图表生成与渲染在本文件，导出执行见 exec.go / runner.go。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/toolhealth"
)

// Service 是图表与导出的服务端依赖。Renderer / Office 为 nil 表示对应外部依赖不可用（原因见 *Reason）。
// Health 非 nil 时记录运行中的失败 / 恢复（运行能力状态 degraded / healthy）。
type Service struct {
	DB             *db.DB
	Store          *Store
	Renderer       Renderer
	RendererReason string
	Office         Office
	OfficeReason   string
	Health         *toolhealth.Registry
}

// Status 是外部依赖可用性（门禁明细与前端展示）。
type Status struct {
	Office         bool   `json:"office"`
	OfficeEngine   string `json:"office_engine,omitempty"`
	OfficeVersion  string `json:"office_version,omitempty"`
	OfficeReason   string `json:"office_reason,omitempty"`
	Renderer       bool   `json:"renderer"`
	RendererName   string `json:"renderer_name,omitempty"`
	RendererReason string `json:"renderer_reason,omitempty"`
}

func (s *Service) Status() Status {
	st := Status{OfficeReason: s.OfficeReason, RendererReason: s.RendererReason}
	if s.Office != nil {
		st.Office, st.OfficeEngine, st.OfficeVersion, st.OfficeReason = true, s.Office.Name(), s.Office.Version(), ""
	}
	if s.Renderer != nil {
		st.Renderer, st.RendererName, st.RendererReason = true, s.Renderer.Name(), ""
	}
	return st
}

// NewService 返回不探测任何外部程序的服务（没有文档引擎与 mmdc）。
func NewService(pg *db.DB, dir, reason string) *Service {
	return &Service{DB: pg, Store: &Store{Root: dir}, OfficeReason: reason, RendererReason: reason}
}

// OfficeTimeout 是单次文档引擎刷新的上限（AUTOBID_OFFICE_TIMEOUT_SECONDS，默认 180 秒）。
func OfficeTimeout() time.Duration {
	return time.Duration(envInt("AUTOBID_OFFICE_TIMEOUT_SECONDS", 180)) * time.Second
}

// NewServiceFromEnv 并行探测文档引擎（AUTOBID_OFFICE_ENGINE，含实际刷新自检）与 mmdc（含中文架构图 / 甘特图渲染自检），
// 结果写入 health（可为 nil）；不可用时记录原因，不报错。
func NewServiceFromEnv(pg *db.DB, dir string, logger *slog.Logger, health *toolhealth.Registry) *Service {
	s := &Service{DB: pg, Store: &Store{Root: dir}, Health: health}
	ctx, cancel := context.WithTimeout(context.Background(), 2*OfficeTimeout()+30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		start := time.Now()
		o, err := DetectOffice(ctx, OfficeOptions{Engine: os.Getenv("AUTOBID_OFFICE_ENGINE"), Timeout: OfficeTimeout()})
		res := toolhealth.Result{}
		if err != nil {
			s.OfficeReason = err.Error()
		} else {
			s.Office = o
			res = toolhealth.Result{Provider: o.Name(), Engine: o.Name(), Version: o.Version()}
		}
		recordProbe(health, toolhealth.Office, res, time.Since(start), err)
	}()
	go func() {
		defer wg.Done()
		start := time.Now()
		m, err := DetectMMDC()
		res := toolhealth.Result{Provider: "mmdc"}
		if err == nil {
			res.Version = m.Version()
			err = RendererSelfTest(ctx, m)
		}
		if err != nil {
			s.RendererReason = err.Error()
		} else {
			s.Renderer = m
		}
		recordProbe(health, toolhealth.Mermaid, res, time.Since(start), err)
	}()
	wg.Wait()
	if logger != nil {
		if s.Office != nil {
			logger.Info("document engine available for export", "engine", s.Office.Name(), "version", s.Office.Version())
		} else {
			logger.Warn("no document engine (WPS or Word): PDF export jobs will fail and DOCX TOC page numbers will not be refreshed", "reason", s.OfficeReason)
		}
		if s.Renderer != nil {
			logger.Info("mermaid-cli available for charts", "renderer", s.Renderer.Name(), "version", rendererVersion(s.Renderer))
		} else {
			logger.Warn("mermaid-cli unavailable: Mermaid charts stay unrendered and export as source placeholders", "reason", s.RendererReason)
		}
	}
	return s
}

// recordProbe 把探测结果写入运行能力状态：未安装 / 未配置记为 unconfigured，其余失败记为 self_test_failed。
func recordProbe(h *toolhealth.Registry, c toolhealth.Capability, res toolhealth.Result, took time.Duration, err error) {
	if h == nil {
		return
	}
	if err != nil && errors.Is(err, ErrNotInstalled) {
		err = toolhealth.AsUnconfigured(err)
	}
	h.Record(c, res, took, err)
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && v > 0 {
		return v
	}
	return def
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
			if ctx.Err() == nil {
				s.Health.Degrade(toolhealth.Mermaid, err)
			}
			r = db.ChartRender{Status: db.ChartFailed, Error: truncateRunes(err.Error(), 500)}
			break
		}
		s.Health.Recover(toolhealth.Mermaid)
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
