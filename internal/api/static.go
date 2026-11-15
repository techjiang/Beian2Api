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
	// 指纹必须覆盖**所有**前端资源。只算 style.css / app.js 的话，
	// 改后台的 admin.css / admin.js 不会让 ?v= 变，浏览器那头吃 7 天的
	// immutable 缓存，升级后看不到任何变化——而人只会说「你没改吧」。
	for _, name := range []string{"style.css", "app.js", "admin.css", "admin.js"} {
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
	mux.Handle("/assets/", cacheHeader(noRawTemplates(http.StripPrefix("/assets/", fileServer))))

	indexRaw, _ := fs.ReadFile(sub, "index.html")
	adminRaw, _ := fs.ReadFile(sub, "admin.html")
	docsRaw, _ := fs.ReadFile(sub, "docs.html")
	logo, _ := fs.ReadFile(sub, "logo.svg")
	icon, _ := fs.ReadFile(sub, "apple-touch-icon.png")

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			// /api 下的未知路径必须回统一外壳。
			//
			// 文档第一句就写着「所有响应统一为 {ok,data} / {ok,error}」，
			// 而 /api/v1/nope 落到这里只会拿到一行纯文本 `404 page not found`。
			// 调用方按 ok==false 判错，收到的却是解析不了的 HTML 文本，
			// 报错信息变成"Unexpected token <"。接口的路径写错是最常见的一类
			// 调试事故，它该得到一句能看懂的话。
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeErr(w, http.StatusNotFound, "not_found", "接口不存在，请查看 /api/v1/openapi.json 或 /docs")
				return
			}
			http.NotFound(w, r)
			return
		}
		servePage(w, r, indexRaw)
	})
	// 后台页面本身是静态的，数据全靠 /api/v1/admin/* 鉴权。
	//
	// 为什么页面不做成「配了 Key 才返回」：页面里没有任何数据，
	// 拦它只会让人打不开自己的后台、也看不到那句「需要 API Key」的提示。
	// 真正的门在后面每个接口上——那才是数据所在的地方。
	// 页面类路由只认 GET / HEAD。
	//
	// 之前这三个页面用裸 HandleFunc —— `POST /`、`DELETE /admin`、`PUT /docs`
	// 一律回 200 和整页 HTML。接口层早就把方法检查做全了（DELETE /api/v1/stats
	// 回 405），页面层却是例外。对这种访问者来说，"200 加一整页内容"就是
	// 一句"你做成了"，而它什么都没做成——一个把 `POST /` 当成"提交查询"的
	// 脚本会永远看到成功。
	mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) { servePage(w, r, adminRaw) })
	mux.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) { servePage(w, r, docsRaw) })
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

// servePage 输出一个页面模板，并把方法限制在 GET / HEAD。
//
// 之前这三个页面用裸 HandleFunc —— `POST /`、`DELETE /admin`、`PUT /docs`
// 一律回 200 和整页 HTML。接口层早就把方法检查做全了（DELETE /api/v1/stats
// 回 405），页面层却是例外。对这种访问者来说，「200 加一整页内容」就是一句
// 「你做成了」，而它什么都没做成。
func servePage(w http.ResponseWriter, r *http.Request, body []byte) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 GET / HEAD")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60")
	_, _ = w.Write(withVersion(body))
}

// noRawTemplates 挡住 /assets 下直接访问 .html。
//
// 三个页面（index / admin / docs）是**模板**：里面的 __ASSET_VERSION__
// 要在服务端替换成资源指纹才成立。它们只该从 `/`、`/admin`、`/docs` 出去。
// 直接访问 /assets/index.html 拿到的是未替换的原文，浏览器会去请求
// /assets/style.css?v=__ASSET_VERSION__——一个永远不存在的版本号。
// 更要紧的是 /assets/ 会列出目录索引，把这三份模板（以及它们的内部注释）
// 摊给任何路过的人看。静态资源目录只该出 css / js / 图标。
func noRawTemplates(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 目录请求（以 / 结尾）也挡：http.FileServer 对目录会自己去找
		// index.html，于是 `/assets/` 就把首页模板原样吐出来了——
		// 同样带着没替换的占位符，同样把内部注释摊给路过的人。
		if strings.HasSuffix(r.URL.Path, ".html") || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
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
