package toolhealth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSanitize(t *testing.T) {
	cases := []struct{ in, mustNot, must string }{
		{`mmdc: exit status 1: Error: Failed to launch D:\Pycharm Project\Bid_plan\autobid-engine\tools\mermaid-cli\cli.js`, `Pycharm`, `<path>`},
		{`open C:/Users/LENOVO/AppData/Local/Temp/autobid-mmdc-1/chart.png: no such file`, `LENOVO`, `<path>`},
		{`stat /home/svc/autobid/bin/mmdc: no such file`, `/home/svc`, `<path>`},
		{`POST https://dashscope.aliyuncs.com/api/v1/services/embeddings?key=abc123 failed`, `abc123`, `https://dashscope.aliyuncs.com/api/v1/services/embeddings`},
		{`dial https://user:pass@10.0.0.1:9000/ocr: refused`, `pass@`, `https://10.0.0.1:9000/ocr`},
		{`401: Authorization: Bearer sk-abcdefghijklmnop`, `sk-abcdef`, `<redacted>`},
		{`api_key=supersecret rejected`, `supersecret`, `api_key=<redacted>`},
		{`\\fileserver\share\ocr.exe missing`, `fileserver`, `<path>`},
	}
	for _, c := range cases {
		got := Sanitize(c.in)
		if strings.Contains(got, c.mustNot) || !strings.Contains(got, c.must) {
			t.Errorf("Sanitize(%q) = %q; must not contain %q, must contain %q", c.in, got, c.mustNot, c.must)
		}
	}
	if got := Sanitize(strings.Repeat("长", 400)); len([]rune(got)) != 301 {
		t.Errorf("Sanitize did not truncate: %d runes", len([]rune(got)))
	}
}

func TestRegistryStates(t *testing.T) {
	r := New()
	for _, st := range r.List() {
		if st.State != Starting || st.Available || st.Label == "" {
			t.Fatalf("initial %s = %+v", st.Capability, st)
		}
	}
	r.Record(Office, Result{Provider: "wps", Engine: "wps", Version: "WPS 12.1"}, 3*time.Second, nil)
	st := r.Get(Office)
	if st.State != Healthy || !st.Available || st.Engine != "wps" || st.CheckedAt == nil || st.DurationMS != 3000 || st.Impact != "" {
		t.Fatalf("healthy office = %+v", st)
	}
	r.Degrade(Office, errors.New(`refresh C:\Temp\x.docx timed out`))
	st = r.Get(Office)
	if st.State != Degraded || !st.Available || st.FailedAt == nil || strings.Contains(st.Reason, `C:\Temp`) || st.NextAction == "" {
		t.Fatalf("degraded office = %+v", st)
	}
	r.Recover(Office)
	if st = r.Get(Office); st.State != Healthy || st.Reason != "" || st.FailedAt != nil {
		t.Fatalf("recovered office = %+v", st)
	}

	// 未配置与自检失败区分；运行失败不会把它们改成 degraded。
	r.Record(Mermaid, Result{Provider: "mmdc"}, 0, AsUnconfigured(errors.New("mmdc is not on PATH")))
	if st = r.Get(Mermaid); st.State != Unconfigured || st.Reason != "mmdc is not on PATH" || st.Available || st.Impact == "" || st.NextAction == "" {
		t.Fatalf("unconfigured mermaid = %+v", st)
	}
	r.Degrade(Mermaid, errors.New("boom"))
	if r.Get(Mermaid).State != Unconfigured {
		t.Fatal("degrade must not change an unconfigured capability")
	}
	r.Record(OCR, Result{}, 0, errors.New("self-test: expected 招标"))
	if st = r.Get(OCR); st.State != SelfTestFailed || st.Available {
		t.Fatalf("failed ocr = %+v", st)
	}
	r.Recover(OCR)
	if r.Get(OCR).State != SelfTestFailed {
		t.Fatal("recover must not turn a failed self-test healthy")
	}

	if got := r.List(OCR, Office); len(got) != 2 || got[0].Capability != Office || got[1].Capability != OCR {
		t.Fatalf("filtered list = %+v", got)
	}
}

func TestProbeShowsStarting(t *testing.T) {
	r := New()
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Probe(context.Background(), Embedding, "model@rev", func(context.Context) (Result, error) {
			<-release
			return Result{Version: "model@rev"}, nil
		})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for r.Get(Embedding).Provider != "model@rev" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if st := r.Get(Embedding); st.State != Starting || st.Available {
		t.Fatalf("during probe = %+v", st)
	}
	close(release)
	<-done
	if st := r.Get(Embedding); st.State != Healthy || st.Version != "model@rev" || st.Provider != "model@rev" {
		t.Fatalf("after probe = %+v", st)
	}
	var nilReg *Registry
	nilReg.Degrade(Office, errors.New("x")) // 未配置注册表时调用方不需要判空
	nilReg.Recover(Office)
}
