package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.Cache.TTL != 21600 {
		t.Fatalf("默认值变了: %+v", c)
	}
	if len(c.EnabledProviders()) != 2 {
		t.Fatalf("默认应有两个数据源: %v", c.EnabledProviders())
	}
	if c.EnabledProviders()[0] != "miit" {
		t.Fatal("miit 权重最高，应排第一")
	}
}

func TestLoadYAMLAndEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
listen: ":9090"
data_dir: "` + dir + `"
cache:
  ttl: 60
  negative: 10
providers:
  - name: miit
    enabled: false
    weight: 0
    timeout: 5
  - name: apihz
    enabled: true
    weight: 80
    timeout: 6
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEIAN2API_LISTEN", ":7070")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":7070" {
		t.Fatalf("环境变量应覆盖文件，实际 %s", c.Listen)
	}
	if c.Cache.TTL != 60 || c.Cache.Negative != 10 {
		t.Fatalf("yaml 缓存配置未生效: %+v", c.Cache)
	}
	got := c.EnabledProviders()
	if len(got) != 1 || got[0] != "apihz" {
		t.Fatalf("禁用 miit 后只剩 apihz，实际 %v", got)
	}
}

func TestParseProvidersEnv(t *testing.T) {
	t.Setenv("BEIAN2API_PROVIDERS", "miit:1, apihz:0, custom:0.5")
	c, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err == nil {
		t.Fatalf("不存在的配置文件应报错，实际 %+v", c)
	}
	c = Default()
	c.applyEnv()
	if len(c.Providers) != 3 {
		t.Fatalf("应解析出 3 个数据源，实际 %+v", c.Providers)
	}
	if c.Providers[1].Name != "apihz" || c.Providers[1].Enabled {
		t.Fatalf("权重 0 应被视为禁用: %+v", c.Providers[1])
	}
	if c.Providers[2].Weight != 50 {
		t.Fatalf("0.5 应折算为 50，实际 %d", c.Providers[2].Weight)
	}
}

func TestValidateFixesZeroValues(t *testing.T) {
	c := &Config{}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Listen == "" || c.RateLimit.RPS == 0 || c.Watch.Workers == 0 || c.Cache.StaleMax == 0 {
		t.Fatalf("零值应被补齐: %+v", c)
	}
	if len(c.Providers) == 0 {
		t.Fatal("未配置数据源时应回落默认")
	}
}

func TestProviderSortStable(t *testing.T) {
	ps := []Provider{{Name: "a", Weight: 10}, {Name: "b", Weight: 30}, {Name: "c", Weight: 30}}
	sortProviders(ps)
	if ps[0].Name != "b" || ps[1].Name != "c" || ps[2].Name != "a" {
		t.Fatalf("排序结果不对（同权重应保持原序）: %+v", ps)
	}
}

func TestDump(t *testing.T) {
	c := Default()
	b, err := c.Dump()
	if err != nil || len(b) == 0 {
		t.Fatal("输出配置失败")
	}
}

func TestWatchIntervalFromEnv(t *testing.T) {
	t.Setenv("BEIAN2API_WATCH_INTERVAL", "6h")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.WatchInterval() != 6*time.Hour {
		t.Fatalf("6h 应解析成 6 小时，实际 %v", c.WatchInterval())
	}
	t.Setenv("BEIAN2API_WATCH_INTERVAL", "90")
	c, _ = Load("")
	if c.Watch.Interval != 90 {
		t.Fatalf("纯数字应按秒解析，实际 %d", c.Watch.Interval)
	}
}

func TestNotifyChannelsFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
notify:
  retries: 4
  channels:
    - name: 运维群
      kind: dingtalk
      url: https://oapi.dingtalk.com/robot/send?access_token=x
      secret: SECxxx
      events: [changed, revoked]
    - name: 备用
      url: https://open.feishu.cn/open-apis/bot/v2/hook/y
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Notify.Retries != 4 || len(c.Channels()) != 2 {
		t.Fatalf("通道配置未生效: %+v", c.Notify)
	}
	if c.Channels()[0].Secret != "SECxxx" || len(c.Channels()[0].Events) != 2 {
		t.Fatalf("通道字段丢失: %+v", c.Channels()[0])
	}
}

// 旧的 watch.webhook 不能因为升级就失效：老用户只改了版本，其他都没动。
func TestLegacyWebhookStillWorks(t *testing.T) {
	c := Default()
	c.Watch.Webhook = "https://oapi.dingtalk.com/robot/send?access_token=old"
	chs := c.Channels()
	if len(chs) != 1 || chs[0].URL != c.Watch.Webhook {
		t.Fatalf("旧配置应降级成一个通道: %+v", chs)
	}
	// 新配置里已有同一个地址时不重复推送
	c.Notify.Channels = []NotifyChannel{{Name: "a", URL: c.Watch.Webhook}}
	if n := len(c.Channels()); n != 1 {
		t.Fatalf("同一地址不应重复: %d", n)
	}
}

func TestNotifyChannelsEnv(t *testing.T) {
	t.Setenv("BEIAN2API_NOTIFY_CHANNELS",
		"dingtalk:https://oapi.dingtalk.com/robot/send?access_token=a,https://my.gw/hook")
	c, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err == nil {
		t.Fatalf("不存在的配置文件应报错: %+v", c)
	}
	c = Default()
	c.applyEnv()
	chs := c.Channels()
	if len(chs) != 2 {
		t.Fatalf("应解析出 2 个通道: %+v", chs)
	}
	if chs[0].Kind != "dingtalk" || !strings.Contains(chs[0].URL, "access_token=a") {
		t.Fatalf("kind 与 url 切割不对: %+v", chs[0])
	}
	if chs[1].Kind != "" || chs[1].URL != "https://my.gw/hook" {
		t.Fatalf("未写 kind 时应留空交由自动识别: %+v", chs[1])
	}
}

func TestNotifyValidation(t *testing.T) {
	c := Default()
	c.Notify.Channels = []NotifyChannel{{Name: "bad", URL: "ftp://x"}}
	if err := c.Validate(); err == nil {
		t.Fatal("非 http(s) 地址应报错")
	}
	c.Notify.Channels = []NotifyChannel{{Name: "empty"}}
	if err := c.Validate(); err == nil {
		t.Fatal("缺少 url 应报错")
	}
	c.Notify.Channels = nil
	c.Notify.Retries = 99
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Notify.Retries > 10 {
		t.Fatalf("重试次数上限应生效: %d", c.Notify.Retries)
	}
}
