package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YAML 里的时长只能写整数秒，写 `6h` 直接启动失败。
//
// README 教人用 BEIAN2API_WATCH_INTERVAL=6h，环境变量那条路认时间写法；
// 而 config.yaml 里的 interval / cache.ttl 只认 int。于是把同一行
// `interval: 6h` 搬进文件，服务起不来，报一句
// `cannot unmarshal !!str `6h` into int`——同一套配置两种写法，一种能用。
// 配时长写 6h 是最自然的手感（config.example.yaml 里那几个大数也没人读得出来）。
func TestYAMLDurationsAcceptUnits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
data_dir: "` + dir + `"
cache:
  ttl: 6h
  negative: 10m
  stale_max: 7d
watch:
  interval: 90m
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("带单位的时长应被接受: %v", err)
	}
	if c.Cache.TTL != 21600 {
		t.Fatalf("ttl: 6h 应为 21600 秒，实际 %d", c.Cache.TTL)
	}
	if c.Cache.Negative != 600 {
		t.Fatalf("negative: 10m 应为 600 秒，实际 %d", c.Cache.Negative)
	}
	if c.Cache.StaleMax != 7*86400 {
		t.Fatalf("stale_max: 7d 应为 %d 秒，实际 %d", 7*86400, c.Cache.StaleMax)
	}
	if c.Watch.Interval != 5400 {
		t.Fatalf("interval: 90m 应为 5400 秒，实际 %d", c.Watch.Interval)
	}
}

// 纯数字仍然按秒：老配置、环境变量、文档里的写法不能变。
func TestYAMLDurationsKeepPlainSeconds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("data_dir: \""+dir+"\"\ncache:\n  ttl: 60\nwatch:\n  interval: 90\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Cache.TTL != 60 || c.Watch.Interval != 90 {
		t.Fatalf("纯数字应按秒解析，实际 ttl=%d interval=%d", c.Cache.TTL, c.Watch.Interval)
	}
}

// 写错的时长要被拒绝，而不是静默变成 0。
//
// 这一条比「接受 6h」更要紧：`ttl: 6hours` 拼错时如果被当成 0，
// 缓存就永远不过期（或者永远过期，取决于 0 的语义），而配置文件上
// 一个字都看不出问题。报错里要能看出写错的是哪个值。
func TestYAMLDurationsRejectGarbage(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"6hours", "abc", "-5m"} {
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte("data_dir: \""+dir+"\"\ncache:\n  ttl: "+bad+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("%q 应被拒绝", bad)
		} else if !strings.Contains(err.Error(), bad) {
			t.Fatalf("报错应指出写错的值 %q，实际：%v", bad, err)
		}
	}
}

// 负数（不管是哪种写法）都要拒。
func TestYAMLDurationsRejectNegative(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("data_dir: \""+dir+"\"\ncache:\n  ttl: -60\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("负的 ttl 应被拒绝")
	}
}
