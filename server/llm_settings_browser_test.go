package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func isolatedModelDB(t *testing.T) *db.DB {
	t.Helper()
	root := testDB(t)
	schema := fmt.Sprintf("model_browser_%d", time.Now().UnixNano())
	if _, err := root.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = root.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	cfg, err := pgx.ParseConfig(os.Getenv("AUTOBID_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pg := &db.DB{DB: stdlib.OpenDB(*cfg)}
	if err := pg.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	var actualSchema string
	if err := pg.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&actualSchema); err != nil || actualSchema != schema {
		t.Fatalf("isolated schema not selected: %s %v", actualSchema, err)
	}
	if err := pg.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return pg
}

// Opt-in browser acceptance test: real PostgreSQL in a private, disposable schema,
// real HTTP routes and built UI, local model fixture (no external credentials/cost).
func TestModelSettingsBrowser(t *testing.T) {
	if os.Getenv("PLAYWRIGHT_MODULE") == "" {
		t.Skip("set PLAYWRIGHT_MODULE and build web to run browser acceptance")
	}
	pg := isolatedModelDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	t.Setenv("AUTOBID_LLM_MASTER_KEY", "")
	t.Setenv("AUTOBID_LLM_KEY_FILE", filepath.Join(t.TempDir(), "master.key"))

	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer browser-fixture-key" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"missing fixture credential"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"fixture-chat"},{"id":"fixture-responses"},{"id":"fixture-reject"}]}`)
			return
		}
		var in map[string]any
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			return
		}
		if in["model"] == "fixture-reject" {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"unsupported fixture model"}}`)
			return
		}
		switch r.URL.Path {
		case "/v1/chat/completions":
			if in["max_completion_tokens"] != float64(2048) || in["reasoning_effort"] != "high" || in["thinking"].(map[string]any)["type"] != "enabled" {
				t.Error("connection test ignored draft parameters")
			}
			if in["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fixture OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			} else {
				fmt.Fprint(w, `{"choices":[{"message":{"content":"fixture OK"},"finish_reason":"stop"}]}`)
			}
		case "/v1/responses":
			fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"responses OK"}]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer model.Close()
	m := NewManager(pg)
	svc := NewLLMService(pg, LLMServiceConfig{})
	e := NewEngine(m, svc.Resolve, Config{})
	defer e.StopAll()
	web, _ := filepath.Abs("../web/dist")
	api := httptest.NewServer(testAPI(ctx, m, e, svc, nil, NewHub(), APIConfig{WebDir: web}).Handler())
	defer api.Close()
	cmd := exec.CommandContext(ctx, "node", "../web/e2e/model-settings.mjs")
	cmd.Env = append(os.Environ(), "E2E_OUT="+browserOutDir(t), "E2E_BASE="+api.URL, "E2E_MODEL_BASE="+model.URL+"/v1")
	output, err := cmd.CombinedOutput()
	t.Log(strings.TrimSpace(string(output)))
	if err != nil {
		t.Fatal(err)
	}
}
