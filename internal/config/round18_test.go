package config

import (
	"strings"
	"testing"
	"time"
)

// 时长型环境变量必须和 yaml 认同一套写法。
//
// `BEIAN2API_TTL=6h` 之前走的是 strconv.Atoi，解析失败后被**静默忽略**：
// 服务照常起来，缓存 TTL 停在默认的 6 小时。配的人以为改过了，
// 而改的和没改一样。容器的 compose 里最容易踩这一条——改一行、重启、
// 一切"正常"，只是没生效。
func TestDurationEnvAcceptsUnits(t *testing.T) {
	cases := []struct {
		key  string
		val  string
		want Seconds
	}{
		{"BEIAN2API_TTL", "6h", 21600},
		{"BEIAN2API_TTL", "90", 90},
		{"BEIAN2API_TTL", "10m", 600},
		{"BEIAN2API_NEGATIVE_TTL", "2m", 120},
		{"BEIAN2API_STALE_MAX", "7d", 604800},
	}
	for _, tc := range cases {
		t.Setenv(tc.key, tc.val)
		c := Default()
		c.applyEnv()
		if err := c.Validate(); err != nil {
			t.Fatalf("%s=%s 应被接受: %v", tc.key, tc.val, err)
		}
		var got Seconds
		switch tc.key {
		case "BEIAN2API_TTL":
			got = c.Cache.TTL
		case "BEIAN2API_NEGATIVE_TTL":
			got = c.Cache.Negative
		case "BEIAN2API_STALE_MAX":
			got = c.Cache.StaleMax
		}
		if got != tc.want {
			t.Errorf("%s=%s 应为 %d 秒，实际 %d", tc.key, tc.val, tc.want, got)
		}
	}
}

// 写错必须报错，不能静默退回默认值。
func TestBadEnvValuesAreRejected(t *testing.T) {
	cases := []string{
		"BEIAN2API_TTL",
		"BEIAN2API_WATCH_INTERVAL",
		"BEIAN2API_RETAIN_HISTORY",
		"BEIAN2API_RETAIN_CHANGES",
		"BEIAN2API_RETAIN_HISTORY_TOTAL",
		"BEIAN2API_WATCH_FAIL_THRESHOLD",
	}
	for _, key := range cases {
		t.Setenv(key, "zzz")
		c := Default()
		c.applyEnv()
		err := c.Validate()
		if err == nil {
			t.Fatalf("%s=zzz 应被拒绝", key)
		}
		// 报错必须点出是哪个变量、错在哪。
		if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "无法识别") {
			t.Errorf("%s 的报错应点出变量名，实际 %v", key, err)
		}
	}
}

// 负数与非正数巡检间隔也要拒。
func TestNegativeEnvValuesAreRejected(t *testing.T) {
	t.Setenv("BEIAN2API_RETAIN_HISTORY", "-5")
	c := Default()
	c.applyEnv()
	if err := c.Validate(); err == nil {
		t.Fatal("负数保留上限应被拒绝")
	}

	t.Setenv("BEIAN2API_RETAIN_HISTORY", "0")
	t.Setenv("BEIAN2API_WATCH_INTERVAL", "0")
	c = Default()
	c.applyEnv()
	// 0 是合法取值（不裁剪），巡检间隔 0 不是。
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "BEIAN2API_WATCH_INTERVAL") {
		t.Fatalf("巡检间隔 0 应被拒绝，实际 %v", err)
	}
}

// 一次改错多个环境变量时，一次报全——改一个、重启、再看下一个太费事。
func TestMultipleEnvErrorsReportedTogether(t *testing.T) {
	t.Setenv("BEIAN2API_TTL", "zz")
	t.Setenv("BEIAN2API_RETAIN_HISTORY", "abc")
	t.Setenv("BEIAN2API_WATCH_INTERVAL", "yy")
	c := Default()
	c.applyEnv()
	err := c.Validate()
	if err == nil {
		t.Fatal("三个坏值应报错")
	}
	for _, key := range []string{"BEIAN2API_TTL", "BEIAN2API_RETAIN_HISTORY", "BEIAN2API_WATCH_INTERVAL"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("报错应包含 %s，实际 %v", key, err)
		}
	}
}

// 巡检间隔仍支持纯秒数与带单位两种写法（老行为不能坏）。
func TestWatchIntervalEnvKeepsBothForms(t *testing.T) {
	t.Setenv("BEIAN2API_WATCH_INTERVAL", "6h")
	c := Default()
	c.applyEnv()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.WatchInterval() != 6*time.Hour {
		t.Fatalf("6h 应为 6 小时，实际 %v", c.WatchInterval())
	}

	t.Setenv("BEIAN2API_WATCH_INTERVAL", "1800")
	c = Default()
	c.applyEnv()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.WatchInterval() != 30*time.Minute {
		t.Fatalf("1800 应为 30 分钟，实际 %v", c.WatchInterval())
	}
}
