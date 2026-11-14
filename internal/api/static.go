package api

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:assets
var assets embed.FS

// assetVersion 是内嵌静态资源的指纹。
//
// 资源可以用很长的缓存期（immutable），但 URL 里带上指纹，
// 这样升级二进制后浏览器会自动拿新文件，不会出现「改了样式用户看不到」。
var assetVersion = func() string {
	h := sha256.New()
	for _, name := range []string{"style.css", "app.js"} {
		if b, err := assets.ReadFile("assets/" + name); err == nil {
			h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:10]
}()

// withVersion 把 HTML 里的 __ASSET_VERSION__ 换成真实指纹。
func withVersion(html []byte) []byte {
	return []byte(strings.ReplaceAll(string(html), "__ASSET_VERSION__", assetVersion))
}

// mountStatic 挂载内嵌的首页、文档与图片资源。
// 内嵌而非读取磁盘，保证单个二进制即可上线，也便于搬迁。
func (s *Server) mountStatic(mux *http.ServeMux) {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		return
	}
	fileServer := http.FileServer(http.FS(sub))
	mux.Handle("/assets/", cacheHeader(http.StripPrefix("/assets/", fileServer)))

	indexRaw, _ := fs.ReadFile(sub, "index.html")
	docsRaw, _ := fs.ReadFile(sub, "docs.html")
	logo, _ := fs.ReadFile(sub, "logo.svg")
	icon, _ := fs.ReadFile(sub, "apple-touch-icon.png")

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=60")
		_, _ = w.Write(withVersion(indexRaw))
	})
	mux.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(withVersion(docsRaw))
	})
	mux.HandleFunc("/logo.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(logo)
	})
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write(logo)
	})
	mux.HandleFunc("/apple-touch-icon.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(icon)
	})
}

func cacheHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".css") || strings.HasSuffix(r.URL.Path, ".js") || strings.HasSuffix(r.URL.Path, ".svg") || strings.HasSuffix(r.URL.Path, ".png") {
			w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
		}
		next.ServeHTTP(w, r)
	})
}
