package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/llmrec"
)

// openai 格式 Profile 经 DefaultProviderFactory + LLMService：base_url、session_header_key、
// 原始报文录制 Transport 与 token 账本同时生效。
func TestOpenAIProfileThroughService(t *testing.T) {
	pg := testDB(t)
	ctx := context.Background()
	var sessionHeader atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessionHeader.Store(r.Header.Get("X-Session-Id"))
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"好的\"},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	task, err := pg.CreateTask(ctx, db.CreateTaskInput{Name: fmt.Sprint("oa-", time.Now().UnixNano()), Goal: "g"})
	if err != nil {
		t.Fatal(err)
	}
	pid, err := pg.CreateLLMProfile(ctx, db.LLMProfile{
		Name: fmt.Sprint("compat-", time.Now().UnixNano()), Format: "openai", BaseURL: srv.URL + "/v1",
		APIKeyEnv: "AUTOBID_TEST_UNSET_KEY", Model: "deepseek-chat", Enabled: true, InGlobalPool: false,
		SessionHeaderKey: "X-Session-Id",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := pg.GetLLMProfile(ctx, pid); p.Effort != "" || p.APIKeyEnv != "AUTOBID_TEST_UNSET_KEY" {
		t.Fatalf("openai profile defaults: %+v", p)
	}

	svc := NewLLMService(pg, LLMServiceConfig{RecordBodies: func() bool { return true }})
	prov, err := svc.profileProvider(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	cctx := llmrec.WithAttribution(llm.WithSessionID(ctx, "exp1-worker-i9"), llmrec.Attribution{TaskID: task.ID, Worker: "worker"})
	acc := llm.NewAccumulator()
	for ev, err := range prov.Stream(cctx, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}}) {
		if err != nil {
			t.Fatal(err)
		}
		acc.Add(ev)
	}
	if msg, _, _ := acc.Result(); msg.Text() != "好的" {
		t.Fatalf("text = %q", msg.Text())
	}
	if sessionHeader.Load() != "exp1-worker-i9" {
		t.Fatalf("session header = %v", sessionHeader.Load())
	}

	var records, usageRows, inTok int
	var url, respBody string
	waitFor(t, 5*time.Second, "record row", func() bool {
		_ = pg.QueryRow(`SELECT COUNT(*), COALESCE(MAX(url),''), COALESCE(MAX(response_body),'') FROM llm_records WHERE task_id = $1`, task.ID).
			Scan(&records, &url, &respBody)
		return records == 1
	})
	_ = pg.QueryRow(`SELECT COUNT(*), COALESCE(SUM(input_tokens),0) FROM llm_usage WHERE task_id = $1 AND profile_id = $2`, task.ID, pid).Scan(&usageRows, &inTok)
	if url != srv.URL+"/v1/chat/completions" || respBody == "" || usageRows != 1 || inTok != 11 {
		t.Fatalf("records=%d url=%s usage=%d tokens=%d", records, url, usageRows, inTok)
	}
}
