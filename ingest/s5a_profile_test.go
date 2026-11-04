package ingest

// S5a：解析路由（auto / native / ocr_only）、结构化缺口、OCR 坐标溯源与本机 OCR 服务协议。
// 真实 PaddleOCR 用例在设置 AUTOBID_OCR_URL（tools/paddleocr/service.py）时执行，否则跳过。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/ingest/ingesttest"
)

type profileOCR struct {
	calls int
	conf  float64
	empty bool
}

func (o *profileOCR) Name() string { return "profile-fake" }
func (o *profileOCR) Recognize(context.Context, []byte, string) (*OCRPage, error) {
	o.calls++
	if o.empty {
		return &OCRPage{Width: 800, Height: 1100}, nil
	}
	conf := o.conf
	if conf == 0 {
		conf = 0.97
	}
	return &OCRPage{Width: 800, Height: 1100, Version: "fake 1", Lines: []OCRLine{
		{Text: "第三章 技术要求", BBox: [4]float64{80, 60, 400, 100}, Confidence: 0.99},
		{Text: "3.1 服务器数量不少于12台。", BBox: [4]float64{80, 140, 700, 180}, Confidence: conf},
	}}, nil
}

// twoPagePDF：第 1 页有文本层，第 2 页只有扫描图像。
func twoPagePDF() []byte {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0, 1, 0xFF, 0xD9}
	return ingesttest.BuildPDF("混合件",
		ingesttest.Page{Texts: []ingesttest.Text{{X: 72, Y: 760, Size: 14, S: "第一章 总则"}, {X: 72, Y: 730, Size: 11, S: "1.1 本项目为数据中心改造工程。"}}},
		ingesttest.Page{JPEG: jpeg})
}

func gapKinds(p *Parsed) map[string][]int {
	out := map[string][]int{}
	for _, g := range p.Meta.Gaps {
		out[g.Kind] = append(out[g.Kind], g.Page)
	}
	return out
}

func TestParseProfilesRecordGapsAndOCRProvenance(t *testing.T) {
	ctx := context.Background()
	pdf := twoPagePDF()

	// auto + OCR：文本层页照常解析，扫描页经 OCR；OCR 版面框标明像素坐标与图像尺寸。
	ocr := &profileOCR{}
	p, err := Parse(ctx, File{Name: "mixed.pdf", Data: pdf}, ParseOptions{OCR: ocr, Profile: ProfileAuto})
	if err != nil {
		t.Fatal(err)
	}
	if ocr.calls != 1 || len(p.Meta.OCRPages) != 1 || p.Meta.OCRPages[0] != 2 || p.Meta.Profile != ProfileAuto || len(p.Meta.Gaps) != 0 {
		t.Fatalf("auto: calls=%d meta=%+v", ocr.calls, p.Meta)
	}
	if !strings.Contains(p.Text, "数据中心改造") || !strings.Contains(p.Text, "服务器数量不少于12台") {
		t.Fatalf("auto text: %q", p.Text)
	}
	var ocrBox *BBox
	for _, s := range p.Sections() {
		for i := range s.Boxes {
			if s.Boxes[i].Coord == CoordOCRPixels {
				ocrBox = &s.Boxes[i]
			}
		}
	}
	if ocrBox == nil || ocrBox.Page != 2 || ocrBox.ImgW != 800 || ocrBox.ImgH != 1100 || ocrBox.Conf == 0 {
		t.Fatalf("OCR provenance box: %+v", ocrBox)
	}

	// auto 无 OCR：文本层页可用，扫描页记为 ocr_unavailable 缺口，不当空白页。
	p, err = Parse(ctx, File{Name: "mixed.pdf", Data: pdf}, ParseOptions{Profile: ProfileAuto})
	if err != nil || gapKinds(p)[GapOCRUnavailable] == nil || gapKinds(p)[GapOCRUnavailable][0] != 2 {
		t.Fatalf("auto without OCR: %v %+v", err, p.Meta.Gaps)
	}

	// native：即使有 OCR 也不调用，扫描页记为 ocr_not_requested。
	ocr = &profileOCR{}
	p, err = Parse(ctx, File{Name: "mixed.pdf", Data: pdf}, ParseOptions{OCR: ocr, Profile: ProfileNative})
	if err != nil || ocr.calls != 0 || gapKinds(p)[GapOCRNotRequested] == nil {
		t.Fatalf("native: %v calls=%d gaps=%+v", err, ocr.calls, p.Meta.Gaps)
	}

	// ocr_only：忽略文本层；没有页面图像的第 1 页记为 no_page_image，第 2 页经 OCR。
	ocr = &profileOCR{}
	p, err = Parse(ctx, File{Name: "mixed.pdf", Data: pdf}, ParseOptions{OCR: ocr, Profile: ProfileOCROnly})
	if err != nil || ocr.calls != 1 || gapKinds(p)[GapNoPageImage] == nil || gapKinds(p)[GapNoPageImage][0] != 1 {
		t.Fatalf("ocr_only: %v calls=%d gaps=%+v", err, ocr.calls, p.Meta.Gaps)
	}
	if strings.Contains(p.Text, "数据中心改造") {
		t.Fatal("ocr_only must not use the text layer")
	}
	if _, err := Parse(ctx, File{Name: "mixed.pdf", Data: pdf}, ParseOptions{Profile: ProfileOCROnly}); !errors.Is(err, ErrOCRUnavailable) {
		t.Fatalf("ocr_only without OCR: %v", err)
	}

	// 低置信度行与空识别结果都记为缺口。
	p, err = Parse(ctx, File{Name: "mixed.pdf", Data: pdf}, ParseOptions{OCR: &profileOCR{conf: 0.4}})
	if err != nil || gapKinds(p)[GapOCRLowConfidence] == nil {
		t.Fatalf("low confidence: %v %+v", err, p.Meta.Gaps)
	}
	p, err = Parse(ctx, File{Name: "mixed.pdf", Data: pdf}, ParseOptions{OCR: &profileOCR{empty: true}})
	if err != nil || gapKinds(p)[GapOCREmpty] == nil {
		t.Fatalf("empty OCR: %v %+v", err, p.Meta.Gaps)
	}

	// 图片只能经 OCR：native 拒绝，不当空白文档。
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	if _, err := Parse(ctx, File{Name: "scan.png", Data: png}, ParseOptions{OCR: &profileOCR{}, Profile: ProfileNative}); !errors.Is(err, ErrNoText) {
		t.Fatalf("native image: %v", err)
	}

	// PrepareTender 把缺口与 OCR 页写进候选摘要（payload），供人工复核。
	prep, err := PrepareTender(ctx, 1, Input{TenderNo: "T-1", File: &File{Name: "mixed.pdf", Data: pdf}, Profile: ProfileNative})
	if err != nil || len(prep.Summary.Gaps) != 1 || prep.Summary.Gaps[0].Kind != GapOCRNotRequested {
		t.Fatalf("prepared gaps: %v %+v", err, prep)
	}
}

func TestHTTPOCRProtocolHashVersionAndTransientErrors(t *testing.T) {
	ctx := context.Background()
	image := []byte("fake image bytes")
	sum := sha256.Sum256(image)
	digest := hex.EncodeToString(sum[:])
	var lie bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]string{"engine": "paddleocr", "version": "paddleocr 9.9; test"})
			return
		}
		body, _ := io.ReadAll(r.Body)
		got := sha256.Sum256(body)
		if r.Header.Get("X-Autobid-Content-SHA256") != hex.EncodeToString(got[:]) || r.Header.Get("X-Autobid-Request-Id") == "" {
			http.Error(w, `{"error":"bad_request"}`, http.StatusBadRequest)
			return
		}
		echo := hex.EncodeToString(got[:])
		if lie {
			echo = strings.Repeat("0", 64)
		}
		_ = json.NewEncoder(w).Encode(OCRPage{Width: 10, Height: 10, Engine: "paddleocr", Version: "paddleocr 9.9; test", InputSHA256: echo,
			Lines: []OCRLine{{Text: "招标", BBox: [4]float64{1, 1, 5, 5}, Confidence: 0.9}}})
	}))
	defer srv.Close()
	o := &HTTPOCR{URL: srv.URL}
	page, err := o.Recognize(ctx, image, "image/png")
	if err != nil || page.InputSHA256 != digest || page.Version != "paddleocr 9.9; test" {
		t.Fatalf("recognize: %v %+v", err, page)
	}
	if v, err := OCRVersion(ctx, o); err != nil || v != "paddleocr 9.9; test" {
		t.Fatalf("version: %q %v", v, err)
	}
	lie = true
	if _, err := o.Recognize(ctx, image, "image/png"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("hash mismatch accepted: %v", err)
	}

	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not_ready"}`, http.StatusServiceUnavailable)
	}))
	defer busy.Close()
	if _, err := (&HTTPOCR{URL: busy.URL}).Recognize(ctx, image, "image/png"); !errors.Is(err, ErrOCRTransient) {
		t.Fatalf("503 must be transient: %v", err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unsupported_image"}`, http.StatusUnsupportedMediaType)
	}))
	defer bad.Close()
	if _, err := (&HTTPOCR{URL: bad.URL}).Recognize(ctx, image, "image/png"); err == nil || errors.Is(err, ErrOCRTransient) {
		t.Fatalf("415 must be a permanent failure: %v", err)
	}
	if _, err := OCRVersion(ctx, &HTTPOCR{URL: bad.URL}); err == nil {
		t.Fatal("a service without a version must not be pinned")
	}
	down := httptest.NewServer(http.NotFoundHandler())
	url := down.URL
	down.Close()
	if _, err := (&HTTPOCR{URL: url}).Recognize(ctx, image, "image/png"); !errors.Is(err, ErrOCRTransient) {
		t.Fatalf("refused connection must be transient: %v", err)
	}
	if _, err := OCRVersion(ctx, UnavailableOCR{}); !errors.Is(err, ErrOCRUnavailable) {
		t.Fatalf("unavailable OCR version: %v", err)
	}
}

// 真实 PaddleOCR（tools/paddleocr/service.py）：内置自检样本与合成扫描页（灰度 JPEG，嵌入 PDF 第 2 页）。
func TestRealPaddleOCRScannedPDF(t *testing.T) {
	url := strings.TrimSpace(os.Getenv("AUTOBID_OCR_URL"))
	if url == "" {
		t.Skip("AUTOBID_OCR_URL not set: start tools/paddleocr/service.py to run the real OCR test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	o := &HTTPOCR{URL: url, Token: os.Getenv("AUTOBID_OCR_TOKEN")}
	version, err := OCRVersion(ctx, o)
	if err != nil || !strings.Contains(version, "paddleocr") {
		t.Fatalf("version: %q %v", version, err)
	}
	if err := OCRSelfTest(ctx, o); err != nil {
		t.Fatalf("self-test: %v", err)
	}
	scan, err := os.ReadFile("testdata/scan_tender_page.jpg")
	if err != nil {
		t.Fatal(err)
	}
	pdf := ingesttest.BuildPDF("扫描件",
		ingesttest.Page{Texts: []ingesttest.Text{{X: 72, Y: 760, Size: 14, S: "第一章 总则"}, {X: 72, Y: 730, Size: 11, S: "1.1 本项目为数据中心改造工程。"}}},
		ingesttest.Page{JPEG: scan, JPEGWidth: 900, JPEGHeight: 1270})
	start := time.Now()
	p, err := Parse(ctx, File{Name: "scan.pdf", Data: pdf}, ParseOptions{OCR: o, Profile: ProfileAuto})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("real OCR: %s, page 2 in %v, gaps=%+v", version, time.Since(start).Round(time.Millisecond), p.Meta.Gaps)
	if len(p.Meta.OCRPages) != 1 || p.Meta.OCRPages[0] != 2 {
		t.Fatalf("ocr pages: %+v", p.Meta)
	}
	for _, want := range []string{"数据中心改造", "服务器数量不少于12台", "ISO9001", "99.9%", "机架式服务器", "核心交换机"} {
		if !strings.Contains(strings.ReplaceAll(p.Text, " ", ""), want) {
			t.Errorf("recognized text misses %q", want)
		}
	}
	boxes := 0
	for _, s := range p.Sections() {
		for _, b := range s.OCRLines {
			if b.Coord != CoordOCRPixels {
				continue
			}
			boxes++
			if b.Page != 2 || b.ImgW != 900 || b.ImgH != 1270 || b.X0 < 0 || b.Y0 < 0 || b.X1 > 900 || b.Y1 > 1270 || b.X1 <= b.X0 || b.Conf <= 0 || b.Conf > 1 {
				t.Fatalf("OCR box out of the page image: %+v", b)
			}
		}
	}
	if boxes < 15 {
		t.Fatalf("only %d OCR line boxes kept", boxes)
	}
}
