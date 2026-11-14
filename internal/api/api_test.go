package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asoe/beian2api/internal/config"
	"github.com/asoe/beian2api/internal/model"
	"github.com/asoe/beian2api/internal/provider"
	"github.com/asoe/beian2api/internal/service"
	"github.com/asoe/beian2api/internal/store"
)

type stub struct {
	recs []model.Record
	err  error
}

func (s *stub) Name() string                                                 { return "stub" }
func (s *stub) Lookup(ctx context.Context, d string) ([]model.Record, error) { return s.recs, s.err }

func mkServer(t *testing.T, mutate func(*config.Config)) (*Server, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.LogLevel = "error"
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := model.Record{Domain: "example.com", UnitName: "示例公司", ICPCode: "京ICP备1号-1", Source: "stub", FetchedAt: model.Now()}
	r.Normalize()
	svc := service.New(cfg, st, []provider.Task{{Provider: &stub{recs: []model.Record{r}}, Timeout: time.Second}}, log)
	s := New(Options{Config: cfg, Service: svc, Watch: nil, Log: log, Version: "test"})
	return s, s.Handler()
}

func get(t *testing.T, h http.Handler, path string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	resp := w.Result()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestQueryEndpoint(t *testing.T) {
	_, h := mkServer(t, nil)
	resp, body := get(t, h, "/api/v1/query?domain=example.com", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 %d: %s", resp.StatusCode, body)
	}
	var out struct {
		OK   bool `json:"ok"`
		Data struct {
			Found   bool           `json:"found"`
			Records []model.Record `json:"records"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || !out.Data.Found || out.Data.Records[0].ICPCode != "京ICP备1号-1" {
		t.Fatalf("响应内容不对: %s", body)
	}
}

func TestQueryMissingAndInvalid(t *testing.T) {
	_, h := mkServer(t, nil)
	if r, _ := get(t, h, "/api/v1/query", nil); r.StatusCode != 400 {
		t.Fatalf("缺参数应 400，实际 %d", r.StatusCode)
	}
	if r, _ := get(t, h, "/api/v1/query?domain=abc", nil); r.StatusCode != 400 {
		t.Fatalf("非法域名应 400，实际 %d", r.StatusCode)
	}
}

func TestJSONEscaping(t *testing.T) {
	_, h := mkServer(t, nil)
	_, body := get(t, h, "/api/v1/query?domain=example.com", nil)
	if strings.Contains(body, `\u4eac`) {
		t.Fatal("中文不应被转义成 \\uXXXX")
	}
	if !strings.Contains(body, "京ICP备1号-1") {
		t.Fatal("中文应原样输出")
	}
}

func TestBatchEndpoint(t *testing.T) {
	_, h := mkServer(t, nil)
	body := `{"domains":["example.com","Example.com","bad"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("状态码 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"total":2`) {
		t.Fatalf("应去重后剩 2 个（bad 被丢弃）: %s", w.Body.String())
	}
}

func TestAuthRequired(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) {
		c.Auth.Enabled = true
		c.Auth.Keys = []string{"secret-key"}
	})
	if r, _ := get(t, h, "/api/v1/query?domain=example.com", nil); r.StatusCode != 401 {
		t.Fatalf("无 key 应 401，实际 %d", r.StatusCode)
	}
	if r, _ := get(t, h, "/api/v1/query?domain=example.com", map[string]string{"X-API-Key": "wrong"}); r.StatusCode != 401 {
		t.Fatalf("错误 key 应 401，实际 %d", r.StatusCode)
	}
	if r, _ := get(t, h, "/api/v1/query?domain=example.com", map[string]string{"X-API-Key": "secret-key"}); r.StatusCode != 200 {
		t.Fatalf("正确 key 应放行，实际 %d", r.StatusCode)
	}
	if r, _ := get(t, h, "/api/v1/query?domain=example.com&key=secret-key", nil); r.StatusCode != 200 {
		t.Fatalf("query 传 key 也应放行，实际 %d", r.StatusCode)
	}
	if r, _ := get(t, h, "/healthz", nil); r.StatusCode != 200 {
		t.Fatalf("健康检查不应被鉴权拦住，实际 %d", r.StatusCode)
	}
}

func TestRateLimit(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) {
		c.RateLimit.Enabled = true
		c.RateLimit.RPS = 1
		c.RateLimit.Burst = 2
	})
	var limited bool
	for i := 0; i < 12; i++ {
		r, _ := get(t, h, "/api/v1/stats", nil)
		if r.StatusCode == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("限流未生效")
	}
}

func TestBadge(t *testing.T) {
	_, h := mkServer(t, nil)
	r, body := get(t, h, "/badge/icp.svg?domain=example.com", nil)
	if r.StatusCode != 200 || !strings.HasPrefix(body, "<svg") {
		t.Fatalf("徽章输出异常 %d: %s", r.StatusCode, body[:min(80, len(body))])
	}
	if !strings.Contains(body, "京ICP备1号-1") {
		t.Fatal("徽章应包含备案号")
	}
	if !strings.Contains(body, `xmlns="http://www.w3.org/2000/svg"`) {
		t.Fatal("SVG 缺少命名空间")
	}
}

func TestBadgeEscapesXSS(t *testing.T) {
	if out := esc(`<script>&"'`); strings.ContainsAny(out, `<>"`) {
		t.Fatalf("转义失败: %s", out)
	}
}

func TestWatchFlow(t *testing.T) {
	_, h := mkServer(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/watch", strings.NewReader(`{"domain":"example.com"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"watching":true`) {
		t.Fatalf("加入监控失败 %d: %s", w.Code, w.Body.String())
	}
	_, body := get(t, h, "/api/v1/watch/list", nil)
	if !strings.Contains(body, `"total":1`) {
		t.Fatalf("监控列表不对: %s", body)
	}
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/watch?domain=example.com", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("取消监控失败 %d", w.Code)
	}
	_, body = get(t, h, "/api/v1/watch/list", nil)
	if !strings.Contains(body, `"total":0`) {
		t.Fatalf("应已清空: %s", body)
	}
}

func TestCORS(t *testing.T) {
	_, h := mkServer(t, nil)
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/query", nil)
	req.Header.Set("Origin", "https://foo.dev")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("预检应 204，实际 %d", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("默认应允许任意来源")
	}
}

func TestPagesAndOpenAPI(t *testing.T) {
	_, h := mkServer(t, nil)
	for _, p := range []string{"/", "/docs", "/logo.svg", "/api/v1/openapi.json", "/assets/style.css", "/assets/app.js"} {
		r, body := get(t, h, p, nil)
		if r.StatusCode != 200 || len(body) == 0 {
			t.Fatalf("%s 返回异常 %d", p, r.StatusCode)
		}
	}
	r, _ := get(t, h, "/nope", nil)
	if r.StatusCode != 404 {
		t.Fatalf("未知路径应 404，实际 %d", r.StatusCode)
	}
}

func TestHealthAndReady(t *testing.T) {
	_, h := mkServer(t, nil)
	if r, _ := get(t, h, "/healthz", nil); r.StatusCode != 200 {
		t.Fatal("healthz 应 200")
	}
	if r, _ := get(t, h, "/readyz", nil); r.StatusCode != 200 {
		t.Fatal("readyz 应 200")
	}
}

func TestStatsEndpoint(t *testing.T) {
	_, h := mkServer(t, nil)
	_, body := get(t, h, "/api/v1/query?domain=example.com", nil)
	if !strings.Contains(body, "example.com") {
		t.Fatal("查询失败")
	}
	_, body = get(t, h, "/api/v1/stats", nil)
	for _, k := range []string{"cache_hits", "cache_misses", "hit_rate", "version"} {
		if !strings.Contains(body, k) {
			t.Fatalf("统计缺少字段 %s: %s", k, body)
		}
	}
}
