package llmrec

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/llm"
)

// DefaultMaxBody 是单个请求/响应正文的录制上限，超出部分截断并标记 truncated。
const DefaultMaxBody = 4 << 20

// BodySink 写入一条原始报文记录。
type BodySink func(ctx context.Context, r db.LLMRecord) error

// NewTransport 返回录制请求/响应正文的 RoundTripper（§5.7 第 1 条路径，可随时关闭）。
// 只录正文与去掉查询串的 URL，不录请求头，避免把 API Key 写进数据库。
// 流式响应边读边录，在 EOF、出错或 Close 时写入一次，不缓冲整个流也不改变下游读取节奏。
func NewTransport(next http.RoundTripper, sink BodySink, profile Profile, enabled func() bool, onError func(error)) http.RoundTripper {
	return &transport{next: next, sink: sink, profile: profile, enabled: enabled, onError: onError, maxBody: DefaultMaxBody}
}

type transport struct {
	next    http.RoundTripper
	sink    BodySink
	profile Profile
	enabled func() bool
	onError func(error)
	maxBody int
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.sink == nil || t.enabled == nil || !t.enabled() {
		return t.next.RoundTrip(req)
	}
	start := time.Now()
	reqBody, truncated, err := t.snapshotRequest(req)
	if err != nil {
		return nil, err
	}
	u := *req.URL
	u.RawQuery, u.User = "", nil
	a := AttributionFrom(req.Context())
	rec := db.LLMRecord{
		TaskID: a.TaskID, ExplorationID: a.ExplorationID, Worker: a.Worker,
		SessionID: llm.SessionIDFrom(req.Context()), ProfileID: t.profile.ID, ProfileName: t.profile.Name,
		Method: req.Method, URL: u.String(), RequestBody: reqBody, Truncated: truncated,
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		rec.Error = err.Error()
		rec.LatencyMs = int(time.Since(start).Milliseconds())
		t.write(req.Context(), rec)
		return nil, err
	}
	rec.StatusCode = resp.StatusCode
	resp.Body = &recordingBody{
		rc: resp.Body, max: t.maxBody,
		done: func(body []byte, cut bool, readErr error) {
			rec.ResponseBody = string(body)
			rec.Truncated = rec.Truncated || cut
			rec.LatencyMs = int(time.Since(start).Milliseconds())
			if readErr != nil && readErr != io.EOF {
				rec.Error = readErr.Error()
			}
			t.write(req.Context(), rec)
		},
	}
	return resp, nil
}

// snapshotRequest 读取请求正文用于录制，并把等价正文放回请求（RoundTripper 不得消耗调用方正文）。
func (t *transport) snapshotRequest(req *http.Request) (string, bool, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return "", false, nil
	}
	if req.GetBody != nil {
		rc, err := req.GetBody()
		if err != nil {
			return "", false, err
		}
		defer rc.Close()
		return readCapped(rc, t.maxBody)
	}
	raw, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		return "", false, err
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	if len(raw) > t.maxBody {
		return string(raw[:t.maxBody]), true, nil
	}
	return string(raw), false, nil
}

func readCapped(r io.Reader, max int) (string, bool, error) {
	raw, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return "", false, err
	}
	if len(raw) > max {
		return string(raw[:max]), true, nil
	}
	return string(raw), false, nil
}

func (t *transport) write(ctx context.Context, rec db.LLMRecord) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := t.sink(wctx, rec); err != nil && t.onError != nil {
		t.onError(err)
	}
}

type recordingBody struct {
	rc   io.ReadCloser
	max  int
	buf  bytes.Buffer
	cut  bool
	once sync.Once
	done func(body []byte, truncated bool, err error)
}

func (b *recordingBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		room := b.max - b.buf.Len()
		if room >= n {
			b.buf.Write(p[:n])
		} else {
			b.buf.Write(p[:max(room, 0)])
			b.cut = true
		}
	}
	if err != nil {
		b.finish(err)
	}
	return n, err
}

func (b *recordingBody) Close() error {
	b.finish(nil)
	return b.rc.Close()
}

func (b *recordingBody) finish(err error) {
	b.once.Do(func() { b.done(b.buf.Bytes(), b.cut, err) })
}
