package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asoe/beian2api/internal/config"
	"github.com/asoe/beian2api/internal/service"
)

// 跨站防护的覆盖面。
//
// 上一版只护了 /api/v1/admin/*，理由是「破坏性操作都在后台」。实际不是：
//
//   - POST /api/v1/watch 能把任意域名塞进监控列表（不需要 Key）；
//   - DELETE /api/v1/watch 能把别人的监控悄悄移出——巡检就此停掉，
//     而没有任何人会收到通知，这比删缓存更隐蔽；
//   - POST /api/v1/watch/run 每调一次就是一次全量回源，等于拿别人的机器打上游。
//
// 这些接口和后台一样会改变实例状态，判据就该一样。

// crossSite 发一个「来自别的站点」的请求。
func crossSite(t *testing.T, h http.Handler, method, path, body, site string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:8080"
	if body != "" {
		// text/plain 是故意的：浏览器不会为它发预检，普通 <form> 就够。
		req.Header.Set("Content-Type", "text/plain")
	}
	req.Header.Set("Origin", "https://evil.example")
	if site != "" {
		req.Header.Set("Sec-Fetch-Site", site)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// 跨站的「加入监控」必须被拒，而且要真的没写进去。
func TestCrossSiteCannotAddWatch(t *testing.T) {
	s, h := mkServer(t, nil)
	code, body := crossSite(t, h, http.MethodPost, "/api/v1/watch",
		`{"domain":"evil-injected.com"}`, "cross-site")
	if code != http.StatusForbidden {
		t.Fatalf("跨站加监控应 403，实际 %d %s", code, body)
	}
	if !strings.Contains(body, "cross_site_blocked") {
		t.Fatalf("错误码应是 cross_site_blocked：%s", body)
	}
	if n, _ := s.svc.Store().CountWatch(); n != 0 {
		t.Fatalf("跨站请求不该写进监控，实际 %d 条", n)
	}
}

// 跨站的「移出监控」更隐蔽：巡检会就此停掉，而没人收到通知。
func TestCrossSiteCannotRemoveWatch(t *testing.T) {
	s, h := mkServer(t, nil)
	if _, body := adminReq(t, h, http.MethodPost, "/api/v1/watch", `{"domain":"keep.com"}`); !strings.Contains(body, `"ok":true`) {
		t.Fatalf("准备阶段应加得进去：%s", body)
	}
	if n, _ := s.svc.Store().CountWatch(); n != 1 {
		t.Fatalf("准备阶段应有 1 条监控，实际 %d", n)
	}

	code, body := crossSite(t, h, http.MethodDelete, "/api/v1/watch?domain=keep.com", "", "cross-site")
	if code != http.StatusForbidden {
		t.Fatalf("跨站移出监控应 403，实际 %d %s", code, body)
	}
	if n, _ := s.svc.Store().CountWatch(); n != 1 {
		t.Fatal("跨站请求不该把监控移出")
	}
}

// 跨站触发全量巡检＝拿别人的机器打上游。
func TestCrossSiteCannotTriggerWatchRun(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) { c.Watch.Enabled = true })
	code, body := crossSite(t, h, http.MethodPost, "/api/v1/watch/run", "", "cross-site")
	if code != http.StatusForbidden && code != http.StatusServiceUnavailable {
		// 巡检未装配时是 503，装配了就该被守卫拦成 403。
		t.Fatalf("跨站触发巡检不该放行，实际 %d %s", code, body)
	}
	if code == http.StatusOK {
		t.Fatal("跨站请求不该真的跑一轮巡检")
	}
}

// 同源页面自己的请求必须照常，否则查询台上的「加入监控」按钮就废了。
func TestSameOriginCanStillManageWatch(t *testing.T) {
	s, h := mkServer(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/watch", strings.NewReader(`{"domain":"mine.com"}`))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("同源请求应放行，实际 %d %s", w.Code, w.Body.String())
	}
	if n, _ := s.svc.Store().CountWatch(); n != 1 {
		t.Fatalf("同源请求应真的加进去，实际 %d 条", n)
	}
}

// 不带 Origin 的脚本调用不受影响：它没有「来自哪个站点」可言。
func TestScriptWithoutOriginStillWorks(t *testing.T) {
	s, h := mkServer(t, nil)
	resp, body := adminReq(t, h, http.MethodPost, "/api/v1/watch", `{"domain":"cli.com"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("无 Origin 的脚本调用应放行，实际 %d %s", resp.StatusCode, body)
	}
	if n, _ := s.svc.Store().CountWatch(); n != 1 {
		t.Fatalf("应真的加进去，实际 %d 条", n)
	}
}

// `cors: ["*"]` 的默认值只该覆盖公开查询，不该顺带公开「这台机器在盯什么」。
func TestWildcardCORSDoesNotExposeInstanceState(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) { c.CORS = []string{"*"} })

	// 公开查询：别人家的前端来调本来就是这个配置的用途。
	for _, p := range []string{"/api/v1/query?domain=example.com", "/api/v1/verify?code="} {
		resp, _ := get(t, h, p, map[string]string{"Origin": "https://friend.example"})
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("%s 是公开信息，应允许通配跨域，实际 %q", p, got)
		}
	}
	// 实例状态：监控清单、变更流水、运行统计、推送状态、后台。
	for _, p := range []string{
		"/api/v1/stats", "/api/v1/providers", "/api/v1/watch/list",
		"/api/v1/watch/status", "/api/v1/changes", "/api/v1/changes.rss",
		"/api/v1/notify/status", "/api/v1/admin/overview",
	} {
		resp, _ := get(t, h, p, map[string]string{"Origin": "https://evil.example"})
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s 是实例状态，不该被通配 CORS 公开，实际 ACAO=%q", p, got)
		}
	}
}

// cors 里明确写的域名，读实例状态仍然可以——通配符不等于「一律拒绝跨域」。
func TestExplicitOriginCanReadInstanceState(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) { c.CORS = []string{"https://ops.example"} })
	resp, _ := get(t, h, "/api/v1/watch/list", map[string]string{"Origin": "https://ops.example"})
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://ops.example" {
		t.Fatalf("明确列出的域名应被回显，实际 %q", got)
	}
	if v := resp.Header.Get("Vary"); v != "Origin" {
		t.Fatalf("回显具体 Origin 时必须带 Vary，否则缓存会串，实际 %q", v)
	}
}

// 删一个从没缓存过的域名，不能回「删成功」。
//
// bbolt 的 Delete 对不存在的 key 返回 nil，照旧写 deleted:true 的话，
// 后台会弹「已删除 nope.com」——而那个域名从来没被缓存过，用户只会以为
// 自己记错了，或者怀疑删除按钮根本没生效。
func TestDeleteMissingCacheReportsNotFound(t *testing.T) {
	s, h := mkServer(t, nil)
	if _, err := s.svc.Query(httptest.NewRequest(http.MethodGet, "/", nil).Context(), "cached.com", service.Options{}); err != nil {
		t.Fatal(err)
	}

	_, body := adminReq(t, h, http.MethodDelete, "/api/v1/admin/cache?domain=cached.com", "")
	if !strings.Contains(body, `"deleted":true`) {
		t.Fatalf("真删掉时应报 deleted:true：%s", body)
	}

	_, body = adminReq(t, h, http.MethodDelete, "/api/v1/admin/cache?domain=nope.com", "")
	if strings.Contains(body, `"deleted":true`) {
		t.Fatalf("没删到任何东西时不该报 deleted:true：%s", body)
	}
	if !strings.Contains(body, `"deleted":false`) {
		t.Fatalf("应明确回 deleted:false：%s", body)
	}
}

// 趋势序列要出现在概览里，且形状对得上：小时对齐、连续、带命中率可算的量。
func TestOverviewIncludesSeries(t *testing.T) {
	_, h := mkServer(t, nil)
	_, body := adminReq(t, h, http.MethodGet, "/api/v1/admin/overview", "")
	if !strings.Contains(body, `"series"`) {
		t.Fatalf("概览应带 series：%s", body)
	}
	// 24 个点：前端的时间轴就按这个长度画，少一个整条线都会错位。
	if n := strings.Count(body, `"hour":`); n < 24 {
		t.Fatalf("series 应有 24 个点，实际 %d：%s", n, body)
	}
}

// 变更流水翻页：cursor 一路翻下去不重不漏，且 next_cursor 指向本页最后一条。
func TestChangesCursorPaging(t *testing.T) {
	s, h := mkServer(t, nil)
	const base = int64(1700000000)
	const n = 23
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("%020d-example.com", base+int64(i))
		_ = s.svc.Store().AppendChange(key, map[string]any{"domain": "example.com", "at": base + int64(i)})
	}

	type page struct {
		Data struct {
			Items []struct {
				At int64 `json:"at"`
			} `json:"items"`
			HasMore    bool   `json:"has_more"`
			NextCursor string `json:"next_cursor"`
		} `json:"data"`
	}
	seen := 0
	cursor := ""
	for i := 0; i < 10; i++ {
		path := "/api/v1/changes?limit=5"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		_, body := get(t, h, path, nil)
		var out page
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		for _, it := range out.Data.Items {
			if want := base + int64(n-1-seen); it.At != want {
				t.Fatalf("第 %d 条应是 %d，实际 %d（%s）", seen, want, it.At, body)
			}
			seen++
		}
		if !out.Data.HasMore {
			// 最后一页：不该再给游标，否则调用方会再请求一次空页。
			if out.Data.NextCursor != "" {
				t.Fatalf("没有更多时不该给 next_cursor：%s", body)
			}
			break
		}
		if out.Data.NextCursor == "" {
			t.Fatalf("还有更多时必须给 next_cursor：%s", body)
		}
		cursor = out.Data.NextCursor
	}
	if seen != n {
		t.Fatalf("应翻出全部 %d 条，实际 %d", n, seen)
	}
}

// 游标里的控制字符会让 key 的字典序前提失效。明确拒绝，别静默翻出乱序结果。
func TestChangesRejectsBadCursor(t *testing.T) {
	_, h := mkServer(t, nil)
	resp, body := get(t, h, "/api/v1/changes?cursor=abc%00def", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("含控制字符的游标应 400，实际 %d: %s", resp.StatusCode, body)
	}
	resp, body = get(t, h, "/api/v1/changes?cursor="+strings.Repeat("a", 400), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("过长的游标应 400，实际 %d: %s", resp.StatusCode, body)
	}
}
