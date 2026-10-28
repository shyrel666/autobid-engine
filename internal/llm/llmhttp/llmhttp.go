// Package llmhttp 为 Provider 适配器构造每 Profile 独立的 *http.Client：出站代理、会话亲和请求头与可插拔 Transport 包装。
package llmhttp

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/autobid/autobid-engine/internal/llm"
)

type Options struct {
	// Proxy 为 http/https/socks5 代理 URL；为空时沿用环境变量（HTTP_PROXY / HTTPS_PROXY / NO_PROXY）。
	Proxy string
	// SessionHeaderKey 非空时，为每个请求设置该请求头，值取自请求 Context 中的会话 ID（llm.WithSessionID）。
	SessionHeaderKey string
	// Wrap 在最外层之内包装 Transport（如 llmrec 原始报文录制）；可为 nil。
	Wrap func(http.RoundTripper) http.RoundTripper
}

// NewClient 不设置整体 Timeout：流式响应可能持续数分钟，超时由请求 Context 控制。
func NewClient(o Options) (*http.Client, error) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	if p := strings.TrimSpace(o.Proxy); p != "" {
		u, err := url.Parse(p)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("llmhttp: invalid proxy %q", p)
		}
		base.Proxy = http.ProxyURL(u)
	}
	var rt http.RoundTripper = base
	if o.Wrap != nil {
		rt = o.Wrap(rt)
	}
	if k := strings.TrimSpace(o.SessionHeaderKey); k != "" {
		rt = &sessionHeader{key: http.CanonicalHeaderKey(k), next: rt}
	}
	return &http.Client{Transport: rt}, nil
}

type sessionHeader struct {
	key  string
	next http.RoundTripper
}

func (s *sessionHeader) RoundTrip(req *http.Request) (*http.Response, error) {
	id := llm.SessionIDFrom(req.Context())
	if id == "" || req.Header.Get(s.key) != "" {
		return s.next.RoundTrip(req)
	}
	r := req.Clone(req.Context()) // RoundTripper 不得修改调用方请求
	r.Header.Set(s.key, id)
	return s.next.RoundTrip(r)
}
