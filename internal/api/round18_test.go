package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// POST 也要认 URL 上的 domain / refresh。
//
// 文档的参数表写的是「位置：query / body」，所以
// `POST /api/v1/query?refresh=1 -d '{"domain":"x"}'` 在承诺范围内。
// 之前 POST 分支完全不读 URL：refresh 静默失效（调用方以为在强制回源，
// 实际拿到缓存里那份旧数据），`?refresh=abc` 连 400 都不给——同一批接口里
// GET 会 400，只因为换了请求方法就变成「装作没看见」。
func TestQueryPostHonorsURLParams(t *testing.T) {
	_, h := mkServer(t, nil)

	// 先查一次进缓存，之后 cached 就是「有没有真的回源」的判据。
	if resp, _ := get(t, h, "/api/v1/query?domain=example.com", nil); resp.StatusCode != 200 {
		t.Fatalf("预热请求失败：%d", resp.StatusCode)
	}
	if _, body := get(t, h, "/api/v1/query?domain=example.com", nil); !strings.Contains(body, `"cached":true`) {
		t.Fatalf("第二次应命中缓存: %s", body)
	}

	// POST + ?refresh=1 必须真的回源。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query?refresh=1",
		strings.NewReader(`{"domain":"example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("状态 %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"cached":true`) {
		t.Fatalf("POST + ?refresh=1 应强制回源，实际仍读缓存: %s", w.Body.String())
	}

	// POST 只给 ?domain=，body 里没有 domain，也要能查。
	req = httptest.NewRequest(http.MethodPost, "/api/v1/query?domain=example.com", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"domain":"example.com"`) {
		t.Fatalf("POST 应回落到 URL 上的 domain，实际 %d: %s", w.Code, w.Body.String())
	}

	// 写错要 400，不能静默当 false。
	req = httptest.NewRequest(http.MethodPost, "/api/v1/query?refresh=abc",
		strings.NewReader(`{"domain":"example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("?refresh=abc 应 400，实际 %d: %s", w.Code, w.Body.String())
	}
}

// body 比 URL 更具体，两处都给时以 body 为准（老调用方行为不变）。
func TestQueryPostBodyWinsOverURL(t *testing.T) {
	_, h := mkServer(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query?domain=other.com",
		strings.NewReader(`{"domain":"example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"domain":"example.com"`) {
		t.Fatalf("body 应优先于 URL，实际 %d: %s", w.Code, w.Body.String())
	}
}

// 页面路由只认 GET / HEAD。
//
// 之前 `POST /`、`DELETE /admin`、`PUT /docs` 一律回 200 和整页 HTML。
// 对这种访问者来说「200 加一整页内容」就是「你做成了」，而它什么都没做成。
func TestPageRoutesRejectOtherMethods(t *testing.T) {
	_, h := mkServer(t, nil)
	for _, path := range []string{"/", "/admin", "/docs"} {
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			req := httptest.NewRequest(m, path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s 应 405，实际 %d", m, path, w.Code)
			}
			if got := w.Header().Get("Allow"); !strings.Contains(got, "GET") {
				t.Errorf("%s %s 的 Allow 头应包含 GET，实际 %q", m, path, got)
			}
		}
		// GET / HEAD 照常。
		for _, m := range []string{http.MethodGet, http.MethodHead} {
			req := httptest.NewRequest(m, path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("%s %s 应 200，实际 %d", m, path, w.Code)
			}
		}
	}
}

// /assets 下不能直接拿到页面模板。
//
// 三个页面是模板，__ASSET_VERSION__ 要在服务端替换。直接访问
// /assets/index.html 拿到的是原文，浏览器会去请求一个永远不存在的
// `style.css?v=__ASSET_VERSION__`；而 /assets/ 会列出目录索引，
// 把模板和内部注释一并摊出来。静态资源目录只该出 css / js / 图标。
func TestAssetsDoNotServeRawTemplates(t *testing.T) {
	_, h := mkServer(t, nil)
	for _, path := range []string{"/assets/index.html", "/assets/admin.html", "/assets/docs.html", "/assets/"} {
		resp, _ := get(t, h, path, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s 应 404，实际 %d", path, resp.StatusCode)
		}
	}
	// 真正的资源照常。
	for _, path := range []string{"/assets/style.css", "/assets/app.js", "/assets/admin.css", "/assets/logo.svg"} {
		resp, _ := get(t, h, path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s 应 200，实际 %d", path, resp.StatusCode)
		}
	}
}

// 首页与后台的模板必须带着替换后的指纹出去。
func TestServedPagesCarryAssetVersion(t *testing.T) {
	_, h := mkServer(t, nil)
	for _, path := range []string{"/", "/admin", "/docs"} {
		_, body := get(t, h, path, nil)
		if strings.Contains(body, "__ASSET_VERSION__") {
			t.Errorf("%s 里还有未替换的占位符", path)
		}
	}
}
