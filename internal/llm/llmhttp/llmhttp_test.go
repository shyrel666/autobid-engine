package llmhttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/autobid/autobid-engine/internal/llm"
)

func TestSessionHeaderFromContext(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Session-Affinity")
	}))
	defer srv.Close()

	c, err := NewClient(Options{SessionHeaderKey: "x-session-affinity"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := llm.WithSessionID(context.Background(), "exp7-worker-i42")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != "exp7-worker-i42" || req.Header.Get("X-Session-Affinity") != "" {
		t.Fatalf("header=%q, caller request mutated=%v", got, req.Header.Get("X-Session-Affinity") != "")
	}
}

func TestProxyIsUsed(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 经 HTTP 代理时请求行为绝对 URL。
		if r.URL.Host == "llm.invalid" {
			proxied.Add(1)
		}
		w.WriteHeader(204)
	}))
	defer proxy.Close()

	c, err := NewClient(Options{Proxy: proxy.URL})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("http://llm.invalid/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if proxied.Load() != 1 {
		t.Fatal("request did not go through the configured proxy")
	}
	if _, err := NewClient(Options{Proxy: "not a url"}); err == nil {
		t.Fatal("invalid proxy must be rejected")
	}
}

func TestWrapIsApplied(t *testing.T) {
	var wrapped atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c, _ := NewClient(Options{Wrap: func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			wrapped.Add(1)
			return next.RoundTrip(r)
		})
	}})
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if wrapped.Load() != 1 {
		t.Fatal("wrap transport not used")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
