package server

// 前端工作台静态托管：存在 web/dist 构建产物时由控制面直接提供，未命中文件的页面路由回退到 index.html
// （SPA History 路由）。/api 与 /healthz 从不回退，未注册的 API 路径返回 JSON 404。

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// newSPAHandler 在 dir 含 index.html 时返回静态处理器，否则返回 nil（不托管）。
func newSPAHandler(dir string) http.Handler {
	if dir == "" {
		return nil
	}
	fsys := os.DirFS(dir)
	if st, err := fs.Stat(fsys, "index.html"); err != nil || st.IsDir() {
		return nil
	}
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/healthz" || p == "/api" || strings.HasPrefix(p, "/api/") {
			writeError(w, http.StatusNotFound, errors.New("not found"))
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
			return
		}
		if name := strings.TrimPrefix(path.Clean(p), "/"); name != "" && name != "index.html" {
			if st, err := fs.Stat(fsys, name); err == nil && !st.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, fsys, "index.html")
	})
}
