package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/asoe/beian2api/internal/config"
	"github.com/asoe/beian2api/internal/model"
	"github.com/asoe/beian2api/internal/notify"
	"github.com/asoe/beian2api/internal/provider"
	"github.com/asoe/beian2api/internal/service"
	"github.com/asoe/beian2api/internal/store"
	"github.com/asoe/beian2api/internal/watch"
)

func TestChangesAndRSS(t *testing.T) {
	s, h := mkServer(t, nil)
	// 手工塞两条变更事件
	_ = s.svc.Store().AppendChange("0000000001-a.com", map[string]any{
		"domain": "a.com", "at": 100, "icp_code": "京ICP备1号-1",
		"diff": []map[string]string{{"field": "备案号", "from": "x", "to": "y"}},
	})
	_ = s.svc.Store().AppendChange("0000000002-b.com", map[string]any{
		"domain": "b.com", "at": 200, "icp_code": "沪ICP备2号-1",
	})

	_, body := get(t, h, "/api/v1/changes", nil)
	if !strings.Contains(body, "a.com") || !strings.Contains(body, "b.com") {
		t.Fatalf("变更列表不对: %s", body)
	}
	var out struct {
		Data struct {
			Total int              `json:"total"`
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Total != 2 {
		t.Fatalf("应有 2 条，实际 %d", out.Data.Total)
	}
	if out.Data.Items[0]["domain"] != "b.com" {
		t.Fatal("应按时间倒序")
	}

	resp, rss := get(t, h, "/api/v1/changes.rss", nil)
	if resp.Header.Get("Content-Type") != "application/rss+xml; charset=utf-8" {
		t.Fatalf("RSS 类型不对: %s", resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(rss, "<rss version=\"2.0\">") || !strings.Contains(rss, "b.com") {
		t.Fatalf("RSS 内容不对: %s", rss)
	}
	if strings.Count(rss, "<item>") != 2 {
		t.Fatalf("应有 2 个 item")
	}
}

func TestChangesSinceFilter(t *testing.T) {
	s, h := mkServer(t, nil)
	_ = s.svc.Store().AppendChange("0001", map[string]any{"domain": "old.com", "at": 50})
	_ = s.svc.Store().AppendChange("0002", map[string]any{"domain": "new.com", "at": 500})
	_, body := get(t, h, "/api/v1/changes?since=100", nil)
	if !strings.Contains(body, "new.com") || strings.Contains(body, "old.com") {
		t.Fatalf("since 过滤失效: %s", body)
	}
}

func TestRSSEscaping(t *testing.T) {
	s, h := mkServer(t, nil)
	_ = s.svc.Store().AppendChange("0001", map[string]any{
		"domain": "a.com", "at": 1,
		"diff": []map[string]string{{"field": "网站名称", "from": "<b>", "to": "&x"}},
	})
	_, rss := get(t, h, "/api/v1/changes.rss", nil)
	if strings.Contains(rss, "<b>") {
		t.Fatal("RSS 内容必须转义")
	}
}

func TestProvidersEndpoint(t *testing.T) {
	_, h := mkServer(t, nil)
	_, body := get(t, h, "/api/v1/providers", nil)
	if !strings.Contains(body, "miit") || !strings.Contains(body, "weight") {
		t.Fatalf("数据源列表不对: %s", body)
	}
}

func TestQueryPOST(t *testing.T) {
	_, h := mkServer(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader(`{"domain":"example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("POST 查询失败 %d: %s", w.Code, w.Body.String())
	}
}

func TestBadRequestBodies(t *testing.T) {
	_, h := mkServer(t, nil)
	for _, p := range []string{"/api/v1/query", "/api/v1/batch", "/api/v1/watch"} {
		req := httptest.NewRequest(http.MethodPost, p, strings.NewReader("{not json"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatalf("%s 非法 JSON 应 400，实际 %d", p, w.Code)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	_, h := mkServer(t, nil)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/query", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT 应 405，实际 %d", w.Code)
	}
}

func TestVerifyEndpoint(t *testing.T) {
	_, h := mkServer(t, nil)
	resp, body := get(t, h, "/api/v1/verify?code="+url.QueryEscape("京ICP备12345678号-1"), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Data struct {
			Valid    bool   `json:"valid"`
			Province string `json:"province"`
			Kind     string `json:"kind"`
			Serial   string `json:"serial"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Data.Valid || out.Data.Province != "北京市" || out.Data.Serial != "12345678" {
		t.Fatalf("校验结果不对: %s", body)
	}
}

func TestVerifyInvalidAndCrossCheck(t *testing.T) {
	_, h := mkServer(t, nil)
	_, body := get(t, h, "/api/v1/verify?code=hello", nil)
	if !strings.Contains(body, `"valid":false`) {
		t.Fatalf("非法备案号应 valid=false: %s", body)
	}
	// 与域名交叉核对：假造的号在这条域名下必然不匹配
	_, body = get(t, h, "/api/v1/verify?code="+url.QueryEscape("沪ICP备99999999号-9")+"&domain=example.com", nil)
	if !strings.Contains(body, `"match":false`) {
		t.Fatalf("交叉核对应不匹配: %s", body)
	}
	if r, _ := get(t, h, "/api/v1/verify", nil); r.StatusCode != 400 {
		t.Fatalf("缺参数应 400，实际 %d", r.StatusCode)
	}
}

func TestAdminBackup(t *testing.T) {
	s, h := mkServer(t, nil)
	// httptest 的默认 RemoteAddr 是 192.0.2.1，不是回环，未配 key 时应拒绝。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/backup", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("未配 Key 且非本机应 403，实际 %d: %s", w.Code, w.Body.String())
	}

	// 模拟本机请求
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/backup", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("本机应允许备份，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"size"`) {
		t.Fatalf("响应应包含文件大小: %s", w.Body.String())
	}
	_ = s

	// 路径穿越必须被拦
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/backup?name=../../etc/passwd", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("路径穿越应 400，实际 %d", w.Code)
	}
}

func TestAdminBackupWithKey(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) {
		c.Auth.Enabled = true
		c.Auth.Keys = []string{"k1"}
	})
	// 带 Key 且来自外部 IP 也应允许
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/backup", nil)
	req.Header.Set("X-API-Key", "k1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("带正确 Key 应允许，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestWatchReAddKeepsProgress(t *testing.T) {
	s, h := mkServer(t, nil)
	post := func() {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/watch", strings.NewReader(`{"domain":"example.com"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("加入监控失败 %d: %s", w.Code, w.Body.String())
		}
	}
	post()
	// 模拟巡检已经记录过一次变更
	item, err := s.svc.Store().GetWatch("example.com")
	if err != nil {
		t.Fatal(err)
	}
	addedAt := item.AddedAt
	item.Changes = 3
	if err := s.svc.Store().PutWatch(item); err != nil {
		t.Fatal(err)
	}
	post() // 重复添加
	again, err := s.svc.Store().GetWatch("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if again.Changes != 3 {
		t.Fatalf("重复添加不应清零变更计数，实际 %d", again.Changes)
	}
	if again.AddedAt != addedAt {
		t.Fatal("重复添加不应改写首次加入时间")
	}
	if again.LastFP == "" {
		t.Fatal("基线指纹应被保留")
	}
}

// since 增量拉取不能被 limit 截断：中间早于 limit 窗口的事件也必须能取到。
func TestChangesSinceNotTruncatedByLimit(t *testing.T) {
	s, h := mkServer(t, nil)
	for i, d := range []string{"a.com", "b.com", "c.com", "d.com"} {
		_ = s.svc.Store().AppendChange(fmt.Sprintf("%020d-%s", 100+i, d), map[string]any{
			"domain": d, "at": 100 + i,
		})
	}
	// limit 只数「符合 since」的那些，最旧的 a.com（at=100）必须被过滤掉。
	_, body := get(t, h, "/api/v1/changes?since=101&limit=1", nil)
	if !strings.Contains(body, "d.com") || strings.Contains(body, "a.com") {
		t.Fatalf("since 与 limit 应各自生效: %s", body)
	}
	_, body = get(t, h, "/api/v1/changes?since=101&limit=10", nil)
	if strings.Contains(body, "a.com") {
		t.Fatalf("早于 since 的事件必须过滤: %s", body)
	}
}

func TestWatchStatusEndpoint(t *testing.T) {
	_, h := mkServer(t, nil)
	_, body := get(t, h, "/api/v1/watch/status", nil)
	if !strings.Contains(body, `"enabled":false`) || !strings.Contains(body, `"total":0`) {
		t.Fatalf("未启用巡检时应如实返回: %s", body)
	}
}

// 带巡检的 server：用于验证 watch/run 与 watch/status。
func mkWatchServer(t *testing.T) (*Server, http.Handler, *stub) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.LogLevel = "error"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sb := &stub{recs: []model.Record{}}
	svc := service.New(cfg, st, []provider.Task{{Provider: sb, Timeout: time.Second}}, log)
	w := watch.New(svc, st, time.Hour, 2, nil, log)
	s := New(Options{Config: cfg, Service: svc, Watch: w, Log: log, Version: "test"})
	return s, s.Handler(), sb
}

func TestWatchStatusWithWatcher(t *testing.T) {
	_, h, _ := mkWatchServer(t)
	_, body := get(t, h, "/api/v1/watch/status", nil)
	if !strings.Contains(body, `"enabled":true`) || !strings.Contains(body, `"interval":3600`) {
		t.Fatalf("巡检状态不对: %s", body)
	}
}

// 客户端断开（请求 ctx 已取消）不能中断巡检：这是「点了立即巡检又关掉页面」的真实场景。
func TestWatchRunSurvivesCanceledClient(t *testing.T) {
	s, h, sb := mkWatchServer(t)
	rec := model.Record{Domain: "example.com", UnitName: "示例公司", ICPCode: "京ICP备1号-1", Source: "stub", FetchedAt: model.Now()}
	rec.Normalize()
	sb.recs = []model.Record{rec}
	if err := s.svc.Store().PutWatch(&store.WatchItem{Domain: "example.com"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模拟客户端已经断开
	req := httptest.NewRequest(http.MethodPost, "/api/v1/watch/run", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("巡检应照常完成，实际 %d: %s", w.Code, w.Body.String())
	}
	item, err := s.svc.Store().GetWatch("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if item.LastFP == "" {
		t.Fatal("巡检应写入基线，说明请求 ctx 取消不该中断它")
	}
}

func mkNotifyServer(t *testing.T, hookURL string) (http.Handler, *notify.Dispatcher) {
	t.Helper()
	s, _ := mkServer(t, nil)
	var chs []notify.Channel
	if hookURL != "" {
		chs = []notify.Channel{
			{Name: "dd", Kind: notify.KindDingTalk, URL: hookURL},
			{Name: "fs", Kind: notify.KindFeishu, URL: hookURL},
		}
	}
	d := notify.New(chs, slog.New(slog.NewTextHandler(io.Discard, nil))).WithRetries(1, time.Millisecond)
	s.notifier = d
	return s.Handler(), d
}

func TestNotifyStatusWithoutChannels(t *testing.T) {
	h, _ := mkNotifyServer(t, "")
	_, body := get(t, h, "/api/v1/notify/status", nil)
	if !strings.Contains(body, `"enabled":false`) {
		t.Fatalf("无通道时应如实报告: %s", body)
	}
	// 没配通道时发测试消息应给明确错误，而不是静默成功
	req := httptest.NewRequest(http.MethodPost, "/api/v1/notify/test", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未配置通道应 503，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestNotifyTestDelivers(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		_, _ = w.Write([]byte(`{"errcode":0,"code":0}`))
	}))
	defer srv.Close()
	h, _ := mkNotifyServer(t, srv.URL)

	_, body := get(t, h, "/api/v1/notify/status", nil)
	if !strings.Contains(body, `"enabled":true`) || !strings.Contains(body, `"kind":"dingtalk"`) {
		t.Fatalf("通道列表不对: %s", body)
	}
	if strings.Contains(body, srv.URL) {
		t.Fatal("状态接口不得回显 webhook 地址（地址本身就是凭据）")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/notify/test", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("测试推送失败 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":2`) {
		t.Fatalf("两个通道都应成功: %s", w.Body.String())
	}
	if n != 2 {
		t.Fatalf("应发出 2 条请求，实际 %d", n)
	}

	// 发完之后状态接口要能反映成功
	_, body = get(t, h, "/api/v1/notify/status", nil)
	if !strings.Contains(body, `"last_ok":true`) {
		t.Fatalf("状态应记为成功: %s", body)
	}
}

func TestNotifyTestReportsSemanticFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errcode":310000,"errmsg":"sign not match"}`))
	}))
	defer srv.Close()
	h, _ := mkNotifyServer(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/notify/test", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `"ok":0`) || !strings.Contains(w.Body.String(), "310000") {
		t.Fatalf("上游 errcode 必须透出给用户: %s", w.Body.String())
	}
}

func TestNotifyTestMethodNotAllowed(t *testing.T) {
	h, _ := mkNotifyServer(t, "http://127.0.0.1:1/x")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/notify/test", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("应 405，实际 %d", w.Code)
	}
}

// 未声明 trust_proxy 时，客户端伪造的 X-Forwarded-For 不能换到新的限流桶。
func TestRateLimitIgnoresForgedForwardedFor(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) {
		c.RateLimit.Enabled = true
		c.RateLimit.RPS = 1
		c.RateLimit.Burst = 2
	})
	var codes []int
	for i := 0; i < 12; i++ {
		// 每个请求都换一个伪造的代理头：如果实现采信了它，就永远不会 429。
		resp, _ := get(t, h, "/api/v1/query?domain=example.com", map[string]string{
			"X-Forwarded-For": fmt.Sprintf("10.0.0.%d", i),
			"X-Real-IP":       fmt.Sprintf("10.0.1.%d", i),
		})
		codes = append(codes, resp.StatusCode)
	}
	got429 := false
	for _, c := range codes {
		if c == http.StatusTooManyRequests {
			got429 = true
		}
	}
	if !got429 {
		t.Fatalf("伪造 X-Forwarded-For 不应绕过限流: %v", codes)
	}
}

// 声明了 trust_proxy 后，反代传来的真实 IP 才参与限流分桶。
func TestRateLimitHonorsTrustProxy(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) {
		c.RateLimit.Enabled = true
		c.RateLimit.RPS = 1
		c.RateLimit.Burst = 1
		c.TrustProxy = true
	})
	// 两个不同的真实 IP 各自有一桶，互不影响。
	for _, ip := range []string{"1.1.1.1", "2.2.2.2"} {
		resp, _ := get(t, h, "/api/v1/query?domain=example.com", map[string]string{"X-Real-IP": ip})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s 首个请求应放行，实际 %d", ip, resp.StatusCode)
		}
	}
	// 同一个 IP 第二次就该被限。
	resp, _ := get(t, h, "/api/v1/query?domain=example.com", map[string]string{"X-Real-IP": "1.1.1.1"})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("同一 IP 超额应 429，实际 %d", resp.StatusCode)
	}
}

// trust_proxy 关闭时，带转发头来的备份请求按「非直连」拒绝。
func TestAdminBackupRejectsForwardedWithoutTrustProxy(t *testing.T) {
	s, h := mkServer(t, nil)
	_ = s
	resp, body := get(t, h, "/api/v1/admin/backup", map[string]string{"X-Forwarded-For": "1.2.3.4"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("未信任代理时应拒绝，实际 %d %s", resp.StatusCode, body)
	}
}

// limit 超上限必须报错，不能静默改成默认值：
// 调用方写 limit=500 收到 50 条，分页逻辑会以为「后面没有了」。
func TestChangesLimitOverMaxRejected(t *testing.T) {
	_, h := mkServer(t, nil)
	resp, body := get(t, h, "/api/v1/changes?limit=500", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("应返回 400，实际 %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "invalid_limit") || !strings.Contains(body, "200") {
		t.Fatalf("应说明上限是 200: %s", body)
	}
	// 合法的边界值要能用
	if resp, _ := get(t, h, "/api/v1/changes?limit=200", nil); resp.StatusCode != 200 {
		t.Fatalf("limit=200 应可用，实际 %d", resp.StatusCode)
	}
	// 非数字也拒绝
	if resp, _ := get(t, h, "/api/v1/changes?limit=abc", nil); resp.StatusCode != 400 {
		t.Fatalf("非法 limit 应返回 400，实际 %d", resp.StatusCode)
	}
}

// RSS 的 lastBuildDate 用最后一条事件时间，不是请求时刻。
// 每次请求都刷新成「现在」，等于告诉阅读器「内容刚变过」，feed 会被反复拉取。
func TestRSSLastBuildDateIsLastEvent(t *testing.T) {
	s, h := mkServer(t, nil)
	at := int64(1700000000)
	_ = s.svc.Store().AppendChange("0001", map[string]any{"domain": "a.com", "at": at})
	_, rss := get(t, h, "/api/v1/changes.rss", nil)
	want := time.Unix(at, 0).Format(time.RFC1123Z)
	if !strings.Contains(rss, "<lastBuildDate>"+want+"</lastBuildDate>") {
		t.Fatalf("lastBuildDate 应为 %s，实际:\n%s", want, rss)
	}
}

func TestStatsExposeStorage(t *testing.T) {
	s, h := mkServer(t, nil)
	s.svc.Store().WithRetention(1234, 56)
	_, body := get(t, h, "/api/v1/stats", nil)
	if !strings.Contains(body, `"retain_changes":1234`) || !strings.Contains(body, `"retain_history":56`) {
		t.Fatalf("stats 应暴露保留策略: %s", body)
	}
	if !strings.Contains(body, `"size_bytes"`) {
		t.Fatalf("stats 应暴露数据文件大小: %s", body)
	}
}

// 变更流水是「你在监控哪些域名」的清单，开了 API Key 之后它不能还公开。
func TestRSSRequiresKeyWhenAuthEnabled(t *testing.T) {
	s, h := mkServer(t, func(c *config.Config) {
		c.Auth.Enabled = true
		c.Auth.Keys = []string{"bk_test"}
	})
	_ = s.svc.Store().AppendChange("0000000001-a.com", map[string]any{"domain": "a.com", "at": 100})

	resp, body := get(t, h, "/api/v1/changes.rss", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未带 Key 时应 401，实际 %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "a.com") {
		t.Fatal("401 时不该泄露任何域名")
	}

	// RSS 阅读器塞不了请求头，但 ?key= 是认的——订阅地址里带上即可。
	resp, body = get(t, h, "/api/v1/changes.rss?key=bk_test", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "a.com") {
		t.Fatalf("带 Key 时应正常输出，实际 %d: %s", resp.StatusCode, body)
	}
}

// 开了鉴权之后首页仍然是公开的（要用它填 Key），只是 /api 全部要 Key。
func TestIndexStaysPublicWithAuth(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) {
		c.Auth.Enabled = true
		c.Auth.Keys = []string{"bk_test"}
	})
	resp, body := get(t, h, "/", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "keybox") {
		t.Fatalf("首页应可访问且带 API Key 输入框，实际 %d", resp.StatusCode)
	}
}

// 徽章是公开接口（README 的 <img> 带不了请求头），但它每个请求都可能触发一次
// 回源。之前它既不过鉴权也不限流，等于给全网开了一个「随便填域名就能让服务器
// 去查上游」的放大器：实测连刷 120 次全 200，同时 /api/v1/query 已经 429。
func TestBadgeIsRateLimited(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) {
		c.RateLimit.Enabled = true
		c.RateLimit.RPS = 1
		c.RateLimit.Burst = 2
	})
	limited := false
	for i := 0; i < 12; i++ {
		resp, _ := get(t, h, "/badge/icp.svg?domain=example.com", nil)
		if resp.StatusCode == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("徽章接口应受限流约束")
	}
}

// 徽章不需要 API Key：README 里的 Markdown 图片请求带不了自定义请求头。
// 这个「不用 Key」是有意为之，所以更需要限流兜底——两条一起测，防止后人
// 为了「安全」把 badge 也塞进鉴权，把所有人的 README 图打红。
func TestBadgeStaysPublicWithAuthEnabled(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) {
		c.Auth.Enabled = true
		c.Auth.Keys = []string{"bk_test"}
	})
	resp, body := get(t, h, "/badge/icp.svg?domain=example.com", nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(body, "<svg") {
		t.Fatalf("开了鉴权后徽章仍应公开可读: %d %s", resp.StatusCode, body)
	}
}

// 巡检窗口不能写死 2 分钟：域名多、worker 少时一轮本来就要跑几十分钟，
// 到点被取消等于大半域名状态停在中途。
func TestWatchRunWindowScalesWithDomainCount(t *testing.T) {
	s, _, _ := mkWatchServer(t)
	s.cfg.Watch.Workers = 4
	for i := 0; i < 200; i++ {
		if err := s.svc.Store().PutWatch(&store.WatchItem{Domain: fmt.Sprintf("d%d.com", i)}); err != nil {
			t.Fatal(err)
		}
	}
	// 200 / 4 = 50 轮 × 30 秒 = 25 分钟，加上余量
	if w := s.watchRunWindow(); w < 20*time.Minute {
		t.Fatalf("200 个域名的巡检窗口应长于 20 分钟，实际 %s", w)
	}
	// 但不能无限长：反代和浏览器早就超时了
	s.cfg.Watch.Workers = 1
	for i := 0; i < 5000; i++ {
		if err := s.svc.Store().PutWatch(&store.WatchItem{Domain: fmt.Sprintf("e%d.com", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if w := s.watchRunWindow(); w > 30*time.Minute {
		t.Fatalf("巡检窗口应有 30 分钟上限，实际 %s", w)
	}
}

// 只有少量域名时窗口仍保持 2 分钟的宽松默认值，不要因为算出来 30 秒就收紧。
func TestWatchRunWindowHasTwoMinuteFloor(t *testing.T) {
	s, _, _ := mkWatchServer(t)
	if w := s.watchRunWindow(); w < 2*time.Minute {
		t.Fatalf("窗口不该小于 2 分钟，实际 %s", w)
	}
}

// stats 要把新的保留策略一并暴露出来：看不到就等于没有，
// 用户没法判断「数据文件该不该担心」。
func TestStatsExposesRetentionPolicy(t *testing.T) {
	_, h := mkServer(t, nil)
	_, body := get(t, h, "/api/v1/stats", nil)
	for _, k := range []string{"retain_history_total", "stale_window", "retain_changes", "size_bytes"} {
		if !strings.Contains(body, k) {
			t.Fatalf("stats 缺少 %s: %s", k, body)
		}
	}
}

// since 写错必须报错：静默当成 0 会让增量同步的调用方把历史事件重复处理一遍。
func TestChangesInvalidSinceRejected(t *testing.T) {
	_, h := mkServer(t, nil)
	for _, bad := range []string{"abc", "-5", "1.5"} {
		resp, body := get(t, h, "/api/v1/changes?since="+bad, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("since=%s 应返回 400，实际 %d: %s", bad, resp.StatusCode, body)
		}
		if !strings.Contains(body, "invalid_since") {
			t.Fatalf("应给出 invalid_since: %s", body)
		}
	}
	// 空值仍然表示「不限制」，合法时间戳照常工作。
	if resp, body := get(t, h, "/api/v1/changes?since=", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("空 since 应放行，实际 %d: %s", resp.StatusCode, body)
	}
	if resp, body := get(t, h, "/api/v1/changes?since=100", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("合法 since 应放行，实际 %d: %s", resp.StatusCode, body)
	}
}

// watch/status 默认分页：域名上千时不该每次轮询都全量返回。
func TestWatchStatusPaginates(t *testing.T) {
	s, h, _ := mkWatchServer(t)
	for _, d := range []string{"a.com", "b.com", "c.com", "d.com", "e.com"} {
		if err := s.svc.Store().PutWatch(&store.WatchItem{Domain: d}); err != nil {
			t.Fatal(err)
		}
	}
	var out struct {
		Data struct {
			Total   int  `json:"total"`
			Offset  int  `json:"offset"`
			Limit   int  `json:"limit"`
			HasMore bool `json:"has_more"`
			Items   []struct {
				Domain string `json:"domain"`
			} `json:"items"`
		} `json:"data"`
	}
	_, body := get(t, h, "/api/v1/watch/status?limit=2", nil)
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Total != 5 || len(out.Data.Items) != 2 || !out.Data.HasMore || out.Data.Limit != 2 {
		t.Fatalf("首页应 2 条、总数 5、还有更多: %s", body)
	}
	_, body = get(t, h, "/api/v1/watch/status?limit=2&offset=4", nil)
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Total != 5 || len(out.Data.Items) != 1 || out.Data.HasMore {
		t.Fatalf("第二页末尾应只剩 1 条且没有更多: %s", body)
	}
}

// offset 写错要明确拒绝，不能当成 0 悄悄返回第一页——
// 翻页客户端会因此把第一页当成第 N 页，然后无限循环。
func TestWatchStatusRejectsBadOffset(t *testing.T) {
	_, h := mkServer(t, nil)
	for _, bad := range []string{"abc", "-1"} {
		resp, body := get(t, h, "/api/v1/watch/status?offset="+bad, nil)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "invalid_offset") {
			t.Fatalf("offset=%s 应 400，实际 %d: %s", bad, resp.StatusCode, body)
		}
	}
}

// 请求体超过 1MB 时不能报「不是合法 JSON」。
//
// 两者都是 400，但一个超长的合法 JSON 被这句话挡下来，用户会反复检查
// 自己的 JSON 有没有写错——它一点错都没有，就是太长了。
func TestOversizeBodyReportsSizeNotSyntax(t *testing.T) {
	_, h := mkServer(t, nil)
	big := `{"domains":["` + strings.Repeat("a", 2<<20) + `.com"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/batch", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("应返回 400，实际 %d: %s", w.Code, body)
	}
	if !strings.Contains(body, "上限") {
		t.Fatalf("应说明是超过上限而不是语法错误，实际 %s", body)
	}
}

// since 增量拉取要能早停：变更流水的 key 是「时间戳-域名」，
// 读到第一条更旧的就说明后面全是更旧的，继续扫只是白读。
func TestChangesSinceStopsAtFirstOlderEvent(t *testing.T) {
	s, h := mkServer(t, nil)
	const base = int64(1700000000)
	for i := 0; i < 30; i++ {
		key := fmt.Sprintf("%020d-example.com", base+int64(i))
		_ = s.svc.Store().AppendChange(key, map[string]any{"domain": "example.com", "at": base + int64(i)})
	}
	_, body := get(t, h, "/api/v1/changes?since="+strconv.FormatInt(base+25, 10), nil)
	var out struct {
		Data struct {
			Total int `json:"total"`
			Items []struct {
				At int64 `json:"at"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Total != 5 {
		t.Fatalf("since 之后应有 5 条，实际 %d: %s", out.Data.Total, body)
	}
	for _, it := range out.Data.Items {
		if it.At < base+25 {
			t.Fatalf("返回了早于 since 的事件: %+v", it)
		}
	}
}
