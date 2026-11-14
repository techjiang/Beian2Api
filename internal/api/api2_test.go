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
	"strings"
	"testing"
	"time"

	"github.com/asoe/beian2api/internal/config"
	"github.com/asoe/beian2api/internal/model"
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
	w := watch.New(svc, st, time.Hour, 2, "", log)
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
