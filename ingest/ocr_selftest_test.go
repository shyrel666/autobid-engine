package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// OCR 自检把内置样本发给真实接口实现（HTTPOCR → 本机服务替身），只有识别出中文与数字、坐标合理才通过。
func TestOCRSelfTest(t *testing.T) {
	cfg, err := png.DecodeConfig(bytes.NewReader(ocrSelfTestPNG))
	if err != nil || cfg.Width < 300 {
		t.Fatalf("embedded sample: %v %+v", err, cfg)
	}
	serve := func(page OCRPage) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if r.Header.Get("Content-Type") != "image/png" || !bytes.Equal(body, ocrSelfTestPNG) {
				http.Error(w, "unexpected request", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(page)
		}))
	}
	good := serve(OCRPage{Width: 720, Height: 200, Lines: []OCRLine{
		{Text: "招标文件自检", BBox: [4]float64{40, 30, 380, 90}, Confidence: 0.98},
		{Text: "编号 2026-1103", BBox: [4]float64{40, 110, 380, 160}, Confidence: 0.97},
	}})
	defer good.Close()
	if err := OCRSelfTest(context.Background(), &HTTPOCR{URL: good.URL}); err != nil {
		t.Fatalf("good OCR failed the self-test: %v", err)
	}

	wrong := serve(OCRPage{Width: 720, Height: 200, Lines: []OCRLine{{Text: "lorem ipsum", BBox: [4]float64{1, 1, 100, 20}}}})
	defer wrong.Close()
	if err := OCRSelfTest(context.Background(), &HTTPOCR{URL: wrong.URL}); err == nil || !strings.Contains(err.Error(), "招标") {
		t.Fatalf("wrong text accepted: %v", err)
	}
	badBox := serve(OCRPage{Width: 720, Height: 200, Lines: []OCRLine{{Text: "招标文件 2026", BBox: [4]float64{40, 30, 9000, 90}}}})
	defer badBox.Close()
	if err := OCRSelfTest(context.Background(), &HTTPOCR{URL: badBox.URL}); err == nil || !strings.Contains(err.Error(), "bbox") {
		t.Fatalf("out-of-page bbox accepted: %v", err)
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "busy", 503) }))
	defer down.Close()
	if err := OCRSelfTest(context.Background(), &HTTPOCR{URL: down.URL}); err == nil {
		t.Fatal("failing service passed the self-test")
	}
	if err := OCRSelfTest(context.Background(), UnavailableOCR{}); !errors.Is(err, ErrOCRUnavailable) {
		t.Fatalf("unavailable OCR: %v", err)
	}
}
