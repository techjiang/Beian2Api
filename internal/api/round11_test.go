package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asoe/beian2api/internal/model"
)

// 只读接口必须拒绝不属于它的方法。
//
// 之前这里完全没有检查：`DELETE /api/v1/stats` 返回 200 和一份完整统计，
// `DELETE /api/v1/openapi.json` 也照常给文档。客户端与网关看到一个「删成功」
// 会以为自己真的删掉了什么——接口没变，变的是对外承诺，而承诺是错的。
func TestReadOnlyEndpointsRejectMutatingMethods(t *testing.T) {
	_, h := mkServer(t, nil)
	readOnly := []string{
		"/api/v1/stats",
		"/api/v1/providers",
		"/api/v1/changes",
		"/api/v1/changes.rss",
		"/api/v1/notify/status",
		"/api/v1/watch/status",
		"/api/v1/watch/list",
		"/api/v1/history?domain=example.com",
		"/api/v1/diff?domain=example.com",
		"/api/v1/verify?code=京ICP备12345678号-1",
		"/api/v1/openapi.json",
		"/healthz",
		"/readyz",
		"/badge/icp.svg?domain=example.com",
	}
	for _, path := range readOnly {
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			req := httptest.NewRequest(m, path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s 应 405，实际 %d: %s", m, path, w.Code, w.Body.String())
			}
			// Allow 头是给客户端和网关看的：光回 405 不说支持什么，
			// 调用方只能去翻文档（而文档可能没写）。
			if allow := w.Header().Get("Allow"); !strings.Contains(allow, "GET") {
				t.Errorf("%s %s 的 Allow 头应含 GET，实际 %q", m, path, allow)
			}
		}
	}
}

// HEAD 跟着 GET 放行。
//
// 不少监控探针和 CDN 预热只用 HEAD。之前 /api/v1/query 收到 HEAD 直接 405，
// `curl -I` 探活永远红叉——而服务本身完全正常。
// Go 的 net/http 对 HEAD 会自动丢掉响应体，handler 一个字都不用改。
func TestHeadIsAllowedWhereGetIs(t *testing.T) {
	_, h := mkServer(t, nil)
	for _, path := range []string{"/api/v1/query?domain=example.com", "/api/v1/stats", "/healthz"} {
		req := httptest.NewRequest(http.MethodHead, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("HEAD %s 应 200，实际 %d", path, w.Code)
		}
	}
}

// 空 feed 的 lastBuildDate 不能每次请求都变。
//
// 上一轮修过「lastBuildDate 用 time.Now()」，但只补了「有事件」那一支：
// 没有事件时仍然回落到 now，于是这个永远不会有条目的 feed 反而每次请求
// 都在告诉阅读器「内容刚变过」，比修之前更容易被反复拉。
// 空 feed 干脆不输出该字段——RFC 822 里它是可选项，缺失的正确含义就是
// 「没有内容更新过」。
func TestRSSEmptyFeedHasNoVolatileBuildDate(t *testing.T) {
	_, h := mkServer(t, nil)
	_, first := get(t, h, "/api/v1/changes.rss", nil)
	if strings.Contains(first, "<item>") {
		t.Fatalf("这个用例的前提是没有事件: %s", first)
	}
	if strings.Contains(first, "<lastBuildDate>") {
		t.Fatalf("空 feed 不该输出 lastBuildDate（它会每次请求都变）: %s", first)
	}
	// 连取两次必须逐字节相同：空 feed 就该是一个稳定的空 feed。
	_, second := get(t, h, "/api/v1/changes.rss", nil)
	if first != second {
		t.Fatal("空 feed 的两次响应必须完全一致，否则阅读器会反复拉")
	}
}

// 空 feed 也要是一个合法的 RSS 2.0 文档。
//
// 「不输出 lastBuildDate」不等于可以省掉别的必需元素：channel 的 title /
// link / description 仍然要齐，阅读器才不会把它当成坏 feed 丢弃。
func TestRSSEmptyFeedIsStillValid(t *testing.T) {
	_, h := mkServer(t, nil)
	_, body := get(t, h, "/api/v1/changes.rss", nil)
	for _, want := range []string{`<rss version="2.0">`, "<channel>", "<title>", "<link>", "<description>", "</channel></rss>"} {
		if !strings.Contains(body, want) {
			t.Errorf("空 feed 缺少 %q: %s", want, body)
		}
	}
}

// www 版本与主域名必须共用缓存、共用监控行。
//
// 备案按主域名登记，`www.example.com` 与 `example.com` 是同一份备案。
// 而缓存、历史、监控三处都以域名字符串做 key：不去重的话同一个备案会有
// 两份缓存、两行监控，改一次推两条重复通知，移出一条另一条还在报警。
// 文档里写着「两者共用同一份缓存、同一行监控」，实现必须对得上。
func TestWWWSharesCacheWithApex(t *testing.T) {
	s, h := mkServer(t, nil)
	_, apex := get(t, h, "/api/v1/query?domain=example.com", nil)
	var a struct {
		OK   bool `json:"ok"`
		Data struct {
			Domain string `json:"domain"`
			Cached bool   `json:"cached"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(apex), &a); err != nil || !a.OK {
		t.Fatalf("主域名查询失败: %s", apex)
	}
	if a.Data.Cached {
		t.Fatal("第一次查询不该命中缓存")
	}
	// 查 www 版本：应该命中同一份缓存，并且落成主域名的写法。
	_, www := get(t, h, "/api/v1/query?domain=www.example.com", nil)
	var b struct {
		OK   bool `json:"ok"`
		Data struct {
			Domain string `json:"domain"`
			Cached bool   `json:"cached"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(www), &b); err != nil || !b.OK {
		t.Fatalf("www 版本查询失败: %s", www)
	}
	if !b.Data.Cached {
		t.Fatal("www 版本应命中主域名那份缓存，而不是再回源一次")
	}
	if b.Data.Domain != "example.com" {
		t.Fatalf("结果里的域名应落成主域名写法，实际 %q", b.Data.Domain)
	}
	c, _ := s.svc.Store().Count()
	if c.Cache != 1 {
		t.Fatalf("同一份备案只该有一条缓存，实际 %d", c.Cache)
	}
}

// 用 www 加监控，和用主域名加监控，是同一行。
func TestWWWSharesWatchRowWithApex(t *testing.T) {
	s, h := mkServer(t, nil)
	post := func(domain string) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/watch",
			strings.NewReader(`{"domain":"`+domain+`"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("加入监控 %s 失败 %d: %s", domain, w.Code, w.Body.String())
		}
	}
	post("example.com")
	post("www.example.com")
	n, _ := s.svc.Store().CountWatch()
	if n != 1 {
		t.Fatalf("同一个备案只该占一行监控，实际 %d 行", n)
	}
}

// 只在 www 上加了监控时，删主域名那一行也要删得掉。
func TestWWWWatchDeleteFindsCanonicalRow(t *testing.T) {
	s, h := mkServer(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/watch",
		strings.NewReader(`{"domain":"www.example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("加入监控失败 %d: %s", w.Code, w.Body.String())
	}
	del := httptest.NewRequest(http.MethodDelete, "/api/v1/watch?domain=example.com", nil)
	dw := httptest.NewRecorder()
	h.ServeHTTP(dw, del)
	if dw.Code != http.StatusOK {
		t.Fatalf("移出失败 %d: %s", dw.Code, dw.Body.String())
	}
	if n, _ := s.svc.Store().CountWatch(); n != 0 {
		t.Fatalf("应已移出，实际还剩 %d 行", n)
	}
}

// CanonicalDomain 只剥 www.，不动真正的子域名。
//
// `api.example.com` / `m.example.com` 可能是独立备案的站点，
// 贸然合并会把两个不同网站的数据混在一起——这比不去重更糟。
func TestCanonicalDomainOnlyStripsWWW(t *testing.T) {
	cases := map[string]string{
		"example.com":               "example.com",
		"www.example.com":           "example.com",
		"WWW.Example.COM":           "example.com",
		"https://www.example.com/x": "example.com",
		"api.example.com":           "api.example.com",
		"m.example.com":             "m.example.com",
		"www.com":                   "www.com", // 自己就是注册域名
		"":                          "",
	}
	for in, want := range cases {
		if got := model.CanonicalDomain(in); got != want {
			t.Errorf("CanonicalDomain(%q) = %q，期望 %q", in, got, want)
		}
	}
}
