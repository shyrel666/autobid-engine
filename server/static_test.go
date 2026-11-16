package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSPAHandler(t *testing.T) {
	if newSPAHandler("") != nil || newSPAHandler(t.TempDir()) != nil {
		t.Fatal("handler must be nil without index.html")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>spa</html>"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("console.log(1)"), 0o644)
	api := &API{cfg: APIConfig{WebDir: dir}, authn: stubAuth{}}
	h := api.Handler()

	get := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}
	if rec := get("GET", "/"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "spa") {
		t.Fatalf("root: %d %s", rec.Code, rec.Body)
	}
	if rec := get("GET", "/tasks/12/report"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "spa") {
		t.Fatalf("history fallback: %d %s", rec.Code, rec.Body)
	}
	if rec := get("GET", "/assets/app.js"); rec.Code != 200 || rec.Body.String() != "console.log(1)" ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d %s", rec.Code, rec.Body)
	}
	// ServeMux 先把含 .. 的路径重定向到清洗后的路径，清洗后的路径仍只在 dir 内解析。
	if rec := get("GET", "/assets/../../etc/passwd"); rec.Code != http.StatusTemporaryRedirect && rec.Code != http.StatusMovedPermanently {
		t.Fatalf("traversal must be redirected: %d %s", rec.Code, rec.Body)
	}
	if rec := get("GET", "/etc/passwd"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "spa") {
		t.Fatalf("unknown file must fall back to index: %d %s", rec.Code, rec.Body)
	}
	// /api 未注册路径不回退到 index.html；鉴权仍然生效。
	if rec := get("GET", "/api/nope"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated api: %d", rec.Code)
	}
	req := httptest.NewRequest("GET", "/api/nope", nil)
	req.Header.Set(testActorHeader, "tester")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "spa") {
		t.Fatalf("unknown api path: %d %s", rec.Code, rec.Body)
	}
	if rec := get("POST", "/somewhere"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("non-GET static: %d", rec.Code)
	}
}

// stubAuth 只在带测试请求头时给出身份，不访问数据库。
type stubAuth struct{}

func (stubAuth) authenticate(_ http.ResponseWriter, r *http.Request) (*Identity, error) {
	if r.Header.Get(testActorHeader) == "" {
		return nil, nil
	}
	return &Identity{Login: r.Header.Get(testActorHeader), Tenant: "default"}, nil
}
