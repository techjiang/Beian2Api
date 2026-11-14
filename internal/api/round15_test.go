package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/asoe/beian2api/internal/config"
	"github.com/asoe/beian2api/internal/model"
	"github.com/asoe/beian2api/internal/provider"
	"github.com/asoe/beian2api/internal/service"
	"github.com/asoe/beian2api/internal/store"
)

// 这一组用例来自「把服务真跑起来、照着文档打一遍」时撞到的东西，
// 不是在编辑器里想出来的。它们的共同点是：**报错是沉默的**——
// 配置上看着对、接口回 200、界面显示正常，只是做的事和说的事不一样。

// failingProvider 永远返回上游错误，用来观察「上游挂掉」时的对外行为。
type failingProvider struct{}

func (failingProvider) Name() string { return "failing" }

func (failingProvider) Lookup(context.Context, string) ([]model.Record, error) {
	return nil, errors.New("upstream error: 认证失败 服务器异常")
}

// mkServerWith 用指定上游构造服务，其余与 mkServer 一致。
func mkServerWith(t *testing.T, p provider.Provider, mutate func(*config.Config)) (*Server, http.Handler) {
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
	svc := service.New(cfg, st, []provider.Task{{Provider: p, Timeout: time.Second}}, log)
	s := New(Options{Config: cfg, Service: svc, Log: log, Version: "test"})
	return s, s.Handler()
}

// refresh 写错不能被当成 false。
//
// `?refresh=abc` 之前静默按「不刷新」处理：调用方以为自己在强制回源，
// 实际拿到的是缓存里那份旧数据——而且没有任何迹象说明它没生效。
// 同一批接口里 limit=abc / since=abc 都会回 400，这个没道理是例外。
func TestRefreshParamRejectsGarbage(t *testing.T) {
	_, h := mkServer(t, nil)
	for _, bad := range []string{"abc", "2", "yes%20please", "x%00y"} {
		resp, body := get(t, h, "/api/v1/query?domain=example.com&refresh="+bad, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("refresh=%s 应 400，实际 %d: %s", bad, resp.StatusCode, body)
		}
		if !json.Valid([]byte(body)) {
			t.Fatalf("错误响应也该是 JSON 外壳: %s", body)
		}
	}
	// 合法的真假写法照常工作。
	for _, ok := range []string{"1", "true", "TRUE", "yes", "on"} {
		if resp, body := get(t, h, "/api/v1/query?domain=example.com&refresh="+ok, nil); resp.StatusCode != 200 {
			t.Fatalf("refresh=%s 应 200，实际 %d: %s", ok, resp.StatusCode, body)
		}
	}
	for _, ok := range []string{"0", "false", "no", "off"} {
		if resp, body := get(t, h, "/api/v1/batch?domains=example.com&refresh="+ok, nil); resp.StatusCode != 200 {
			t.Fatalf("batch refresh=%s 应 200，实际 %d: %s", ok, resp.StatusCode, body)
		}
	}
}

// 垃圾游标不能静默翻回第一页。
//
// 乱码在字典序上排在所有 key 之后，Seek 到末尾再 Prev 一步，落点正是
// 最新那一条——于是 `?cursor=abc` 与「不带 cursor」返回一模一样的内容。
// 调用方拿着一个已经损坏的游标继续翻，会以为自己在往后走，其实原地绕圈。
func TestChangesRejectsMalformedCursor(t *testing.T) {
	s, h := mkServer(t, nil)
	_ = s.svc.Store().AppendChange("00000000001700000001-example.com",
		map[string]any{"domain": "example.com", "at": 1700000001})

	for _, bad := range []string{"abc", "nope", "1794672000-", "123", "-example.com", "abcdefghijklmnopqrst-example.com"} {
		resp, body := get(t, h, "/api/v1/changes?cursor="+bad, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("游标 %q 应 400，实际 %d: %s", bad, resp.StatusCode, body)
		}
	}
	// 真正发出去的游标必须照常可用。
	resp, body := get(t, h, "/api/v1/changes?cursor=00000000001700000001-example.com", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("合法游标应 200，实际 %d: %s", resp.StatusCode, body)
	}
}

// 徽章在上游失败时不能被缓存 10 分钟。
//
// 上游抖一下，徽章变成「ICP: error」，然后这张红图被浏览器和 CDN 钉死
// 10 分钟——上游早就恢复了，所有嵌着它的 README 还在红色报错，
// 而那不是本地缓存，是别人 CDN 上的，清都清不掉。
func TestBadgeErrorIsNotCached(t *testing.T) {
	_, h := mkServerWith(t, failingProvider{}, nil)
	resp, body := get(t, h, "/badge/icp.svg?domain=example.com", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("徽章始终回 200（不能打印成红图之外的东西），实际 %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("失败态必须不可缓存，实际 Cache-Control=%q", cc)
	}
	if !strings.Contains(body, "error") {
		t.Fatalf("失败态应在徽章上体现: %s", body)
	}

	// 正常状态仍然要能缓存：徽章每个页面都在拉，不缓存等于每次回源。
	_, h2 := mkServer(t, nil)
	resp2, _ := get(t, h2, "/badge/icp.svg?domain=example.com", nil)
	if cc := resp2.Header.Get("Cache-Control"); cc == "no-store" {
		t.Fatalf("成功态应可缓存，实际 %q", cc)
	}
}

// 「一个通道都没配」和「配了但全被关掉」必须分开说。
//
// 之前两种情况都回 enabled:false + 空列表，页面上写着「还没有配置推送通道」。
// 而 config.yaml 里明明写着三个通道，只是都设了 disabled: true——
// 用户会照着那句提示反复去加通道，加完还是不生效。
func TestNotifyStatusDistinguishesDisabledChannels(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) {
		c.Notify.Channels = []config.NotifyChannel{
			{Name: "运维群", Kind: "feishu", URL: "https://open.feishu.cn/open-apis/bot/v2/hook/x", Disabled: true},
			{Name: "值班群", Kind: "dingtalk", URL: "https://oapi.dingtalk.com/robot/send?access_token=y", Disabled: true},
		}
	})
	_, body := get(t, h, "/api/v1/notify/status", nil)
	var out struct {
		Data struct {
			Enabled    bool     `json:"enabled"`
			Total      int      `json:"total"`
			Configured int      `json:"configured"`
			Disabled   []string `json:"disabled"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Enabled || out.Data.Total != 0 {
		t.Fatalf("全部停用时 enabled/total 应为假/0: %s", body)
	}
	if out.Data.Configured != 2 {
		t.Fatalf("应如实报出配置里写了 2 个通道，实际 %d: %s", out.Data.Configured, body)
	}
	if len(out.Data.Disabled) != 2 {
		t.Fatalf("应列出两个停用的通道: %s", body)
	}

	// 测试消息接口也一样：说明白是「全停了」，别让人以为没配。
	resp, tbody := adminReq(t, h, http.MethodPost, "/api/v1/notify/test", "{}")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("无可用通道时应 503，实际 %d: %s", resp.StatusCode, tbody)
	}
	if !strings.Contains(tbody, "全部处于停用状态") {
		t.Fatalf("错误信息应点明「全停了」: %s", tbody)
	}
}
