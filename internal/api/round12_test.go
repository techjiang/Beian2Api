package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asoe/beian2api/internal/config"
	"github.com/asoe/beian2api/internal/service"
)

// 后台的跨站防护。
//
// 这组用例守的是一个不需要任何凭据就能打穿后台的口子：
//
//	1. `text/plain` 的 POST 不触发 CORS 预检，于是任意网页都能往
//	   127.0.0.1 发一句表单提交。而 decodeJSON 不看 Content-Type，
//	   照样把它当 JSON 解出来——清空缓存、清空监控于是全部可被跨站触发。
//	2. DNS 重绑定：RemoteAddr 真的是 127.0.0.1，只有 Host 露馅。
//
// 两条都不需要攻击者拿到 API Key，所以在 withAdmin 之上要再加一层判据。

// crossSitePost 发一个「来自别的站点」的后台请求。
func crossSitePost(t *testing.T, h http.Handler, path, body, origin string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:8080"
	// 关键：故意用 text/plain —— 这样浏览器不会为它发预检。
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", origin)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// 一个陌生站点的 text/plain POST 不能清空缓存，也不能改任何数据。
func TestAdminRejectsCrossSiteSimpleRequest(t *testing.T) {
	s, h := mkServer(t, nil)
	// 先塞一条缓存，好判断「到底删没删」。
	if _, err := s.svc.Query(httptest.NewRequest(http.MethodGet, "/", nil).Context(), "example.com", service.Options{}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.svc.Store().Count()
	if before.Cache == 0 {
		t.Fatal("准备阶段应有缓存")
	}

	cases := []struct{ path, body, what string }{
		{"/api/v1/admin/cache/clear", `{"only_expired":false}`, "清空全部缓存"},
		{"/api/v1/admin/changes/clear", `{}`, "清空变更流水"},
		{"/api/v1/admin/watch/purge", `{}`, "清空监控"},
		{"/api/v1/admin/watch/bulk", `{"domains":["evil.com"]}`, "偷偷加监控"},
		{"/api/v1/admin/backup", ``, "往磁盘写备份文件"},
	}
	for _, c := range cases {
		code, body := crossSitePost(t, h, c.path, c.body, "https://evil.example")
		if code != http.StatusForbidden {
			t.Errorf("%s：跨站请求应 403，实际 %d %s", c.what, code, body)
		}
		if !strings.Contains(body, "cross_site_blocked") {
			t.Errorf("%s：错误码应是 cross_site_blocked，实际 %s", c.what, body)
		}
	}

	after, _ := s.svc.Store().Count()
	if after.Cache == 0 {
		t.Fatal("跨站请求不该删掉任何缓存")
	}
	if after.Watch != 0 {
		t.Fatal("跨站请求不该加进任何监控")
	}
}

// 跨站 GET 也不行：备份接口 GET 和 POST 等价，读接口也不该被任意站点读走。
func TestAdminRejectsCrossSiteGET(t *testing.T) {
	_, h := mkServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/overview", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("跨站读后台应 403，实际 %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "storage") {
		t.Fatal("被拒绝的响应里不该漏出存储信息")
	}
}

// same-site 也要拒：evil.example.com 与 example.com 是 same-site，
// 但它显然不是同一个管理台。
func TestAdminRejectsSameSiteSubdomain(t *testing.T) {
	_, h := mkServer(t, nil)
	code, body := crossSitePost(t, h, "/api/v1/admin/watch/purge", `{}`, "https://evil.example.com")
	if code != http.StatusForbidden {
		t.Fatalf("同站子域也应拒绝，实际 %d %s", code, body)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/overview", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Sec-Fetch-Site", "same-site")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("Sec-Fetch-Site: same-site 应拒绝，实际 %d", w.Code)
	}
}

// 后台自己的页面发出来的请求必须放行，否则后台整个不能用。
func TestAdminAllowsSameOrigin(t *testing.T) {
	s, h := mkServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/overview", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("同源后台请求应放行，实际 %d: %s", w.Code, w.Body.String())
	}
	_ = s

	// 地址栏直接访问（Sec-Fetch-Site: none）没有 Origin，也要放行，
	// 否则用 curl / 直接打开页面全被挡。
	req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/overview", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:8080"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("无 Origin 的本机请求应放行，实际 %d", w.Code)
	}
}

// DNS 重绑定：对端是回环，但 Host 是攻击者的域名。
//
// 这一条和 RemoteAddr 完全不同——那种攻击下网络层看起来完全合法。
func TestAdminRejectsDNSRebinding(t *testing.T) {
	_, h := mkServer(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/watch/purge", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "evil.example:8080" // 浏览器地址栏写的还是 evil.example
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("Host 不是本机时应拒绝，实际 %d: %s", w.Code, w.Body.String())
	}
}

// `cors: ["*"]` 只该放行只读接口，绝不能顺带把后台也开放出去。
func TestAdminCORSWildcardDoesNotExposeAdmin(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) { c.CORS = []string{"*"} })
	resp, body := get(t, h, "/api/v1/stats", map[string]string{"Origin": "https://evil.example"})
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("只读接口仍应允许通配跨域")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/overview", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("后台不该返回通配 CORS，实际 %q", got)
	}
	_ = resp
	_ = body
}

// 显式写进 cors 的域名可以跨域调后台。
func TestAdminCORSExplicitOriginAllowed(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) { c.CORS = []string{"https://ops.example"} })
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/overview", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Origin", "https://ops.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("显式白名单域名应放行，实际 %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://ops.example" {
		t.Fatalf("应回显白名单 origin，实际 %q", got)
	}
}

// chunked 的请求体不能被静默丢掉。
//
// 旧代码只判 `r.ContentLength > 0`，而 chunked 的 ContentLength 是 -1，
// 于是 `\{"only_expired":false\}` 的意图被整个忽略，按「只清过期」执行了。
// 对一个不可逆操作来说，「静默换个语义执行」比直接报错危险得多。
func TestAdminCacheClearReadsChunkedBody(t *testing.T) {
	s, h := mkServer(t, nil)
	if _, err := s.svc.Query(httptest.NewRequest(http.MethodGet, "/", nil).Context(), "example.com", service.Options{}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/cache/clear", strings.NewReader(`{"only_expired":false}`))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:8080"
	req.ContentLength = -1 // 模拟 chunked：长度未知
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("chunked 请求应正常处理，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"only_expired":false`) {
		t.Fatalf("必须按请求体里的意图执行，实际 %s", w.Body.String())
	}
	if n, _ := s.svc.Store().Count(); n.Cache != 0 {
		t.Fatalf("全清之后缓存应为 0，实际 %d", n.Cache)
	}
}
