package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asoe/beian2api/internal/config"
)

// watch/list 与 watch/status 要对「巡检节奏」说同一个数。
//
// 巡检关掉（watch.enabled=false）时，status 老实回 interval=0，
// 而 list 回的是配置里那个 86400——客户端从 list 读到的结论是
// 「每 24 小时巡检一轮」，实际上一次都不会跑。同一个问题在两个接口上
// 有两种答案，先被读到的那个就成了新的真相。
func TestWatchListAgreesWithStatusWhenDisabled(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) {
		c.Watch.Enabled = false
		c.Watch.Interval = 86400
	})

	_, list := get(t, h, "/api/v1/watch/list", nil)
	_, status := get(t, h, "/api/v1/watch/status", nil)

	var l struct {
		Data struct {
			Enabled  bool `json:"enabled"`
			Interval int  `json:"interval"`
		} `json:"data"`
	}
	var s struct {
		Data struct {
			Enabled  bool `json:"enabled"`
			Interval int  `json:"interval"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(list), &l); err != nil {
		t.Fatalf("list 响应解析失败: %v（%s）", err, list)
	}
	if err := json.Unmarshal([]byte(status), &s); err != nil {
		t.Fatalf("status 响应解析失败: %v（%s）", err, status)
	}
	if l.Data.Interval != s.Data.Interval {
		t.Fatalf("两个接口对 interval 的说法不一致：list=%d status=%d", l.Data.Interval, s.Data.Interval)
	}
	if l.Data.Interval != 0 {
		t.Fatalf("巡检关闭时 list 的 interval 应为 0，实际 %d", l.Data.Interval)
	}
	if l.Data.Enabled {
		t.Fatal("巡检关闭时 list 的 enabled 应为 false，否则读的人不知道 interval 为什么是 0")
	}
}

// 巡检打开时 list 要如实报出配置的间隔与 enabled。
func TestWatchListReportsIntervalWhenEnabled(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) {
		c.Watch.Enabled = true
		c.Watch.Interval = 21600
	})
	_, list := get(t, h, "/api/v1/watch/list", nil)
	var l struct {
		Data struct {
			Enabled  bool `json:"enabled"`
			Interval int  `json:"interval"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(list), &l); err != nil {
		t.Fatalf("list 响应解析失败: %v（%s）", err, list)
	}
	if !l.Data.Enabled || l.Data.Interval != 21600 {
		t.Fatalf("巡检打开时应回 enabled=true / interval=21600，实际 %+v", l.Data)
	}
	if !strings.Contains(list, `"items"`) {
		t.Fatalf("list 丢了 items 字段: %s", list)
	}
}
