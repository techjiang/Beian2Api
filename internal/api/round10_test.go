package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asoe/beian2api/internal/config"
	"github.com/asoe/beian2api/internal/model"
	"github.com/asoe/beian2api/internal/store"
	"github.com/asoe/beian2api/internal/watch"
)

// 转发头是不可信输入，不能直接成为限流器的 key。
//
// 取末段解决了「首段可伪造」，但末段本身仍是字符串：随便填一个乱码就是一桶
// 新令牌（限流等于没有），而且会原样进 limiter 的 map——每个请求一个 key，
// 内存随请求量无界增长。现在只接受真的能解析成 IP 的值。
func TestClientIPRejectsNonIPHops(t *testing.T) {
	cfg := config.Default()
	cfg.TrustProxy = true
	s := &Server{cfg: cfg}

	cases := []string{
		strings.Repeat("a", 40000) + "1", // 超长垃圾
		"not-an-ip",
		"127.0.0.1 evil",
		"",
	}
	for _, bad := range cases {
		r := httptest.NewRequest("GET", "/api/v1/query", nil)
		r.RemoteAddr = "1.2.3.4:5678"
		r.Header.Set("X-Real-IP", bad)
		if got := s.clientIP(r); got != "1.2.3.4" {
			t.Fatalf("X-Real-IP=%q 应被忽略，回落到 TCP 对端，实际 %q", bad, got)
		}
		r.Header.Set("X-Forwarded-For", "9.9.9.9, "+bad)
		// 末段坏了就继续往前找一个合法的（lastHop 的语义），
		// 但绝不能把乱码当成 IP 交出去。
		if got := s.clientIP(r); strings.Contains(got, "a") && bad != "" {
			t.Fatalf("XFF 末段=%q 被当成了 IP：%q", bad, got)
		}
	}

	// 正常值要照常采信，包括带端口的写法。
	r := httptest.NewRequest("GET", "/api/v1/query", nil)
	r.RemoteAddr = "1.2.3.4:5678"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 5.5.5.5")
	if got := s.clientIP(r); got != "5.5.5.5" {
		t.Fatalf("应取末段 5.5.5.5，实际 %q", got)
	}
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 5.5.5.5:1234")
	if got := s.clientIP(r); got != "5.5.5.5" {
		t.Fatalf("带端口的末段应被归一化，实际 %q", got)
	}
	r.Header.Set("X-Forwarded-For", "6.6.6.6, [2001:db8::1]:443")
	if got := s.clientIP(r); got != "2001:db8::1" {
		t.Fatalf("IPv6 带端口应被归一化，实际 %q", got)
	}
}

// 未知的 /api 路径要回统一外壳，而不是一行纯文本 404。
func TestUnknownAPIPathUsesEnvelope(t *testing.T) {
	_, h := mkServer(t, nil)
	resp, body := get(t, h, "/api/v1/nope", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("应 404，实际 %d", resp.StatusCode)
	}
	var out struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("不是 JSON：%q", body)
	}
	if out.OK || out.Error == nil || out.Error.Code != "not_found" {
		t.Fatalf("外壳不对: %s", body)
	}
	if strings.Contains(body, "page not found") {
		t.Fatal("不该是把 http.NotFound 的文本漏出来")
	}
	// 非 /api 的未知路径仍然保持原样（静态站点的 404）。
	if resp, _ := get(t, h, "/nope", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("静态路径应 404，实际 %d", resp.StatusCode)
	}
}

// 没有变更时 changes 必须是 []，不能是 null。
func TestWatchRunReturnsEmptyArrayNotNull(t *testing.T) {
	s, h := mkServer(t, nil)
	w := watch.New(s.svc, s.svc.Store(), 3600, 1, nil, s.log)
	s.watch = w
	if err := s.svc.Store().PutWatch(&store.WatchItem{Domain: "example.com"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/watch/run", nil)
	wr := httptest.NewRecorder()
	h.ServeHTTP(wr, req)
	if wr.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d: %s", wr.Code, wr.Body.String())
	}
	if strings.Contains(wr.Body.String(), `"changes":null`) {
		t.Fatalf("changes 不能是 null：%s", wr.Body.String())
	}
}

// 变更流水要能筛选类型、能翻页，并且 has_more 结论正确。
func TestChangesKindFilterAndPaging(t *testing.T) {
	s, h := mkServer(t, nil)
	for _, c := range []struct {
		at   int64
		kind string
		dom  string
	}{
		{100, "changed", "a.com"},
		{200, "revoked", "b.com"},
		{300, "changed", "c.com"},
		{400, "upstream_error", "d.com"},
		{500, "changed", "e.com"},
	} {
		id := strings.Repeat("0", 20) + "-" + c.dom
		_ = s.svc.Store().AppendChange(id, watch.Change{Domain: c.dom, At: c.at, Kind: c.kind})
	}

	// 只看 changed：3 条，新的在前
	_, body := get(t, h, "/api/v1/changes?kind=changed", nil)
	var out struct {
		Data struct {
			Total   int            `json:"total"`
			HasMore bool           `json:"has_more"`
			Items   []watch.Change `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Total != 3 {
		t.Fatalf("应有 3 条 changed，实际 %d: %s", out.Data.Total, body)
	}
	if out.Data.Items[0].Domain != "e.com" || out.Data.Items[2].Domain != "a.com" {
		t.Fatalf("排序不对: %+v", out.Data.Items)
	}

	// 翻页：kind=changed & limit=2 → 第一页 e,c 且 has_more=true
	_, body = get(t, h, "/api/v1/changes?kind=changed&limit=2", nil)
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Total != 2 || !out.Data.HasMore {
		t.Fatalf("第一页应 2 条且 has_more=true: %s", body)
	}
	// 第二页只剩 a，且 has_more=false
	_, body = get(t, h, "/api/v1/changes?kind=changed&limit=2&offset=2", nil)
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Total != 1 || out.Data.HasMore || out.Data.Items[0].Domain != "a.com" {
		t.Fatalf("第二页应只剩 a.com 且 has_more=false: %s", body)
	}
}

// kind 写错要 400 并列出合法值，不能静默当成「不过滤」。
func TestChangesRejectsUnknownKind(t *testing.T) {
	_, h := mkServer(t, nil)
	resp, body := get(t, h, "/api/v1/changes?kind=upstream-error", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("应 400，实际 %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "invalid_kind") || !strings.Contains(body, "upstream_error") {
		t.Fatalf("应指出合法的 kind 值: %s", body)
	}
}

// OpenAPI 必须覆盖全部 /api 路由。
//
// 这份描述存在的唯一理由是「照着它就能生成能用的客户端」。少一条，
// 生成的客户端就少一个接口，而且不会报错——直到有人用到它。
func TestOpenAPICoversEveryRoute(t *testing.T) {
	s, h := mkServer(t, nil)
	_, body := get(t, h, "/api/v1/openapi.json", nil)
	var doc struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	// 从真实路由表里抠出全部 /api 路径，不手写一份「期望列表」——
	// 手写的清单只会在加路由时被忘掉，那正是这个测试要防的。
	routes := apiRoutes(t, s.Handler())
	for _, p := range routes {
		if _, ok := doc.Paths[p]; !ok {
			t.Fatalf("OpenAPI 缺少路由 %s（已有：%v）", p, keysOf(doc.Paths))
		}
	}
	// 反向也要查：描述里写了、实际没有的路径同样是错，客户端会去打 404。
	for p := range doc.Paths {
		found := false
		for _, r := range routes {
			if r == p {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("OpenAPI 里的 %s 没有对应路由", p)
		}
	}
}

// apiRoutes 探测出实际注册的 /api 路径（含徽章）。
func apiRoutes(t *testing.T, h http.Handler) []string {
	t.Helper()
	candidates := []string{
		"/api/v1/query", "/api/v1/batch", "/api/v1/history", "/api/v1/diff",
		"/api/v1/verify", "/api/v1/watch", "/api/v1/watch/list", "/api/v1/watch/status",
		"/api/v1/watch/run", "/api/v1/changes", "/api/v1/changes.rss",
		"/api/v1/notify/status", "/api/v1/notify/test", "/api/v1/stats",
		"/api/v1/providers", "/api/v1/admin/backup",
		"/badge/icp.svg", "/badge/icp", "/healthz", "/readyz",
	}
	var out []string
	for _, p := range candidates {
		req := httptest.NewRequest(http.MethodPost, p, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		// 未注册的路径会落到静态处理器的 404（且带 not_found 外壳）。
		if w.Code == http.StatusNotFound && strings.Contains(w.Body.String(), `"not_found"`) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// www. 前缀不该影响身份：同一个备案，同一份缓存，同一行监控。
func TestSameDomainIgnoresWWW(t *testing.T) {
	if !model.SameDomain("www.qq.com", "qq.com") {
		t.Fatal("www.qq.com 与 qq.com 应视为同一域名")
	}
	if !model.SameDomain("WWW.QQ.COM", "qq.com") {
		t.Fatal("大小写不该影响")
	}
	// www.com 自己就是注册域名，不能被剥成 com
	if model.SameDomain("www.com", "com") {
		t.Fatal("www.com 不该等于 com")
	}
	if model.SameDomain("api.qq.com", "qq.com") {
		t.Fatal("子域名不是主域名")
	}
}
