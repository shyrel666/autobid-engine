package server

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
)

func TestModelSettingsPersistenceAndSecrets(t *testing.T) {
	t.Setenv("AUTOBID_LLM_KEY_FILE", filepath.Join(t.TempDir(), "master.key"))
	t.Setenv("AUTOBID_LLM_MASTER_KEY", "")
	c, _ := newIdleAPI(t)
	pg := testDB(t)
	ctx := context.Background()
	oldSettings, err := pg.LLMSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var oldDefault int64
	_ = pg.QueryRow(`SELECT id FROM llm_profiles WHERE is_default`).Scan(&oldDefault)
	var ids []int64
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, id)
			_ = pg.DeleteLLMProfile(ctx, id)
		}
		if oldDefault > 0 {
			_ = pg.ActivateLLMProfile(ctx, oldDefault, "")
		}
		_ = pg.SaveLLMSettings(ctx, oldSettings, "")
	})
	payload := map[string]any{"name": fmt.Sprint("settings-", time.Now().UnixNano()), "model": "fixture", "format": "openai-responses", "enabled": true, "in_global_pool": true, "api_key_env": "AUTOBID_MODEL_TEST_UNSET_KEY", "api_key": "fixture-only-secret-1234", "effort": "xhigh", "thinking_type": "enabled", "streaming": false, "rate_per_second": 2, "window_tokens": 64000, "retry": map[string]any{"connect": map[string]any{"attempts": 1, "interval_ms": 7}}}
	var p db.LLMProfile
	if code := c.do("POST", "/api/llm/profiles", payload, &p); code != 201 {
		t.Fatalf("create: %d", code)
	}
	ids = append(ids, p.ID)
	if p.APIKeyHint != "…1234" || !p.KeyConfigured || p.Streams() || p.RatePerSecond != 2 || p.Effort != "xhigh" || p.Retry.Connect.Attempts != 1 {
		t.Fatalf("profile roundtrip: %+v", p)
	}
	var raw map[string]any
	c.do("GET", fmt.Sprint("/api/llm/profiles/", p.ID), nil, &raw)
	if _, ok := raw["api_key"]; ok {
		t.Fatal("plaintext key exposed")
	}
	if _, ok := raw["api_key_ciphertext"]; ok {
		t.Fatal("ciphertext exposed")
	}
	stored, err := pg.GetLLMProfile(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored.APIKeyCiphertext, "v1:") || strings.Contains(stored.APIKeyCiphertext, "fixture-only") {
		t.Fatal("key not encrypted")
	}
	payload["api_key"] = ""
	payload["name"] = payload["name"].(string) + "-edited"
	if code := c.do("PUT", fmt.Sprint("/api/llm/profiles/", p.ID), payload, &p); code != 200 {
		t.Fatal(code)
	}
	stored2, _ := pg.GetLLMProfile(ctx, p.ID)
	if stored2.APIKeyCiphertext != stored.APIKeyCiphertext {
		t.Fatal("blank key overwrote stored credential")
	}
	if code := c.do("POST", "/api/llm/profiles/active", map[string]any{"id": p.ID}, nil); code != 200 {
		t.Fatal(code)
	}
	if code := c.do("DELETE", fmt.Sprint("/api/llm/profiles/", p.ID), nil, nil); code != 400 {
		t.Fatalf("active delete: %d", code)
	}
	payload["enabled"] = false
	if code := c.do("PUT", fmt.Sprint("/api/llm/profiles/", p.ID), payload, nil); code != 400 {
		t.Fatalf("active disable: %d", code)
	}
	payload["enabled"] = true
	bindings, _ := pg.AgentLLMBindings(ctx)
	t.Cleanup(func() { _ = pg.SetAgentLLMBinding(ctx, "judge", bindings["judge"]) })
	if code := c.do("PUT", "/api/llm/bindings/judge", map[string]any{"profile_id": p.ID}, nil); code != 200 {
		t.Fatal(code)
	}
	var got map[string]int64
	c.do("GET", "/api/llm/bindings", nil, &got)
	if got["judge"] != p.ID {
		t.Fatal("binding did not echo")
	}
	settings := db.LLMSettings{PoolEnabled: true, BindFallback: true, Retry: db.LLMRetryPolicy{RetryOverride: db.RetryOverride{Connect: db.RetryRule{Attempts: 2, IntervalMS: 10}}, Breaker: db.RetryRule{Attempts: 5}}}
	if code := c.do("PUT", "/api/llm/settings", settings, nil); code != 200 {
		t.Fatal(code)
	}
	var settings2 db.LLMSettings
	c.do("GET", "/api/llm/settings", nil, &settings2)
	if settings2 != settings {
		t.Fatalf("settings: %+v", settings2)
	}
	payload["clear_api_key"] = true
	p = db.LLMProfile{ID: p.ID}
	if code := c.do("PUT", fmt.Sprint("/api/llm/profiles/", p.ID), payload, &p); code != 200 {
		t.Fatal(code)
	}
	if p.KeyConfigured || p.APIKeyHint != "" {
		t.Fatal("explicit clear failed")
	}
	// Migration is idempotent and preserves persisted policy/default/credential state.
	if err := pg.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentModelActivation(t *testing.T) {
	pg := testDB(t)
	ctx := context.Background()
	var old int64
	_ = pg.QueryRow(`SELECT id FROM llm_profiles WHERE is_default`).Scan(&old)
	ids := []int64{}
	for i := 0; i < 2; i++ {
		id, e := pg.CreateLLMProfile(ctx, db.LLMProfile{Name: fmt.Sprintf("activate-%d-%d", time.Now().UnixNano(), i), Model: "fixture", Enabled: true})
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, id)
			_ = pg.DeleteLLMProfile(ctx, id)
		}
		if old > 0 {
			_ = pg.ActivateLLMProfile(ctx, old, "")
		}
	})
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			if e := pg.ActivateLLMProfile(ctx, id, "test"); e != nil {
				t.Error(e)
			}
		}(id)
	}
	wg.Wait()
	var count int
	_ = pg.QueryRow(`SELECT count(*) FROM llm_profiles WHERE is_default`).Scan(&count)
	if count != 1 {
		t.Fatalf("default count=%d", count)
	}
}

func TestLegacyModelSettingsMigration(t *testing.T) {
	pg := isolatedModelDB(t)
	ctx := context.Background()
	ids := []int64{}
	for i, rank := range []int{1, 0, 0} {
		id, err := pg.CreateLLMProfile(ctx, db.LLMProfile{Name: fmt.Sprint("legacy", i), Format: "openai", Model: "fixture", APIKeyEnv: "LEGACY_KEY", Enabled: true, InGlobalPool: true, Rank: rank, Priority: 10 - i})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := pg.SetAgentLLMBinding(ctx, "worker", ids[2]); err != nil {
		t.Fatal(err)
	}
	// Re-run the embedded migration as an installation with old profiles but no
	// model-settings row, including the pre-existing format/name constraints.
	if _, err := pg.Exec(`DROP TABLE llm_settings; UPDATE schema_version SET checksum=''`); err != nil {
		t.Fatal(err)
	}
	if err := pg.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := pg.LLMSettings(ctx)
	if err != nil || !s.PoolEnabled {
		t.Fatalf("legacy failover not preserved: %+v %v", s, err)
	}
	ps, err := pg.GlobalPoolProfiles(ctx)
	if err != nil || ps[0].ID != ids[1] || !ps[0].IsDefault || ps[0].APIKeyEnv != "LEGACY_KEY" {
		t.Fatalf("legacy selection changed: %+v %v", ps, err)
	}
	b, err := pg.AgentLLMBinding(ctx, "worker")
	if err != nil || b != ids[2] {
		t.Fatal("legacy binding lost")
	}
	if _, err := pg.CreateLLMProfile(ctx, db.LLMProfile{Name: "responses-after-upgrade", Format: "openai-responses", Model: "fixture", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`UPDATE schema_version SET checksum=''`); err != nil {
		t.Fatal(err)
	}
	if err := pg.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
}
