package llmrec

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/llm"
)

type sinkLog struct {
	mu   sync.Mutex
	recs []db.LLMRecord
}

func (s *sinkLog) sink(_ context.Context, r db.LLMRecord) error {
	s.mu.Lock()
	s.recs = append(s.recs, r)
	s.mu.Unlock()
	return nil
}

func TestTransportRecordsBodiesNotHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(200)
		io.WriteString(w, "data: echo "+string(body)+"\n\n")
	}))
	defer srv.Close()

	log := &sinkLog{}
	on := true
	client := &http.Client{Transport: NewTransport(http.DefaultTransport, log.sink,
		Profile{ID: 3, Name: "claude-main"}, func() bool { return on }, nil)}

	ctx := WithAttribution(llm.WithSessionID(context.Background(), "exp1-worker-i2"),
		Attribution{TaskID: 9, ExplorationID: 1, Worker: "worker"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/messages?key=secret", strings.NewReader(`{"q":1}`))
	req.Header.Set("x-api-key", "sk-secret")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "data: echo {\"q\":1}\n\n" {
		t.Fatalf("downstream body altered: %q", got)
	}

	if len(log.recs) != 1 {
		t.Fatalf("records = %d", len(log.recs))
	}
	r := log.recs[0]
	if r.RequestBody != `{"q":1}` || r.ResponseBody != string(got) || r.StatusCode != 200 {
		t.Fatalf("record bodies: %+v", r)
	}
	if strings.Contains(r.URL, "secret") || strings.Contains(r.RequestBody+r.ResponseBody, "sk-secret") {
		t.Fatalf("credentials leaked into record: %+v", r)
	}
	if r.TaskID != 9 || r.SessionID != "exp1-worker-i2" || r.ProfileID != 3 || r.Worker != "worker" {
		t.Fatalf("attribution: %+v", r)
	}

	on = false
	resp, _ = client.Post(srv.URL, "application/json", strings.NewReader("{}"))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if len(log.recs) != 1 {
		t.Fatal("disabled recorder must not write")
	}
}

func TestTransportTruncatesLargeBodies(t *testing.T) {
	big := strings.Repeat("x", 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, big) }))
	defer srv.Close()
	log := &sinkLog{}
	tr := NewTransport(http.DefaultTransport, log.sink, Profile{}, func() bool { return true }, nil).(*transport)
	tr.maxBody = 10
	resp, err := (&http.Client{Transport: tr}).Post(srv.URL, "text/plain", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	r := log.recs[0]
	if len(got) != 100 || len(r.ResponseBody) != 10 || len(r.RequestBody) != 10 || !r.Truncated {
		t.Fatalf("got=%d record=%+v", len(got), r)
	}
}
