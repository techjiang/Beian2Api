// Package config 负责配置加载：默认值 -> YAML 文件 -> 环境变量，逐层覆盖。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Provider 描述一个上游数据源的开关与权重。
type Provider struct {
	Name    string `yaml:"name" json:"name"`
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Weight  int    `yaml:"weight" json:"weight"`
	Timeout int    `yaml:"timeout" json:"timeout"` // 秒
}

// Config 是全部运行时配置。
type Config struct {
	Listen   string `yaml:"listen" json:"listen"`
	DataDir  string `yaml:"data_dir" json:"data_dir"`
	LogLevel string `yaml:"log_level" json:"log_level"`
	BaseURL  string `yaml:"base_url" json:"base_url"`

	Cache struct {
		TTL      int `yaml:"ttl" json:"ttl"`             // 命中缓存有效期（秒）
		Negative int `yaml:"negative" json:"negative"`   // 未备案结果缓存（秒）
		StaleMax int `yaml:"stale_max" json:"stale_max"` // 允许回退的陈旧数据上限（秒）
	} `yaml:"cache" json:"cache"`

	RateLimit struct {
		Enabled bool `yaml:"enabled" json:"enabled"`
		RPS     int  `yaml:"rps" json:"rps"`
		Burst   int  `yaml:"burst" json:"burst"`
	} `yaml:"rate_limit" json:"rate_limit"`

	Auth struct {
		Enabled bool     `yaml:"enabled" json:"enabled"`
		Keys    []string `yaml:"keys" json:"-"`
	} `yaml:"auth" json:"auth"`

	Providers []Provider `yaml:"providers" json:"providers"`

	Watch struct {
		Enabled  bool   `yaml:"enabled" json:"enabled"`
		Interval int    `yaml:"interval" json:"interval"` // 秒
		Workers  int    `yaml:"workers" json:"workers"`
		Webhook  string `yaml:"webhook" json:"webhook"` // 兼容旧配置，等价于一个 generic 通道
	} `yaml:"watch" json:"watch"`

	Notify Notify `yaml:"notify" json:"notify"`

	CORS []string `yaml:"cors" json:"cors"`
}

// NotifyChannel 是一个推送通道配置。
type NotifyChannel struct {
	Name     string   `yaml:"name" json:"name"`
	Kind     string   `yaml:"kind" json:"kind"` // dingtalk / wecom / feishu / slack / generic，留空按 URL 识别
	URL      string   `yaml:"url" json:"url"`
	Secret   string   `yaml:"secret" json:"-"`
	Events   []string `yaml:"events" json:"events"`
	Disabled bool     `yaml:"disabled" json:"disabled"`
}

// Notify 是推送相关配置。
type Notify struct {
	// Retries 是单条消息的最大投递次数（含首次）。
	Retries  int             `yaml:"retries" json:"retries"`
	Channels []NotifyChannel `yaml:"channels" json:"channels"`
}

// Default 返回开箱即用的默认配置。零配置文件也能跑起来。
func Default() *Config {
	c := &Config{
		Listen:   ":8080",
		DataDir:  "./data",
		LogLevel: "info",
		BaseURL:  "",
		CORS:     []string{"*"},
	}
	c.Cache.TTL = 21600    // 6h
	c.Cache.Negative = 600 // 10min
	c.Cache.StaleMax = 604800
	c.RateLimit.Enabled = true
	c.RateLimit.RPS = 20
	c.RateLimit.Burst = 60
	c.Watch.Enabled = true
	c.Watch.Interval = 86400
	c.Watch.Workers = 4
	c.Notify.Retries = 3
	c.Providers = []Provider{
		{Name: "miit", Enabled: true, Weight: 100, Timeout: 8},
		{Name: "apihz", Enabled: true, Weight: 40, Timeout: 8},
	}
	return c
}

// Load 读取配置。path 为空时自动探测 ./config.yaml 与 ./data/config.yaml。
func Load(path string) (*Config, error) {
	c := Default()
	if path == "" {
		for _, p := range []string{"config.yaml", filepath.Join(c.DataDir, "config.yaml")} {
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取配置 %s: %w", path, err)
		}
		if err := yaml.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("解析配置 %s: %w", path, err)
		}
	}
	c.applyEnv()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) applyEnv() {
	if v := os.Getenv("BEIAN2API_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("BEIAN2API_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("BEIAN2API_BASE_URL"); v != "" {
		c.BaseURL = v
	}
	if v := os.Getenv("BEIAN2API_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("BEIAN2API_API_KEYS"); v != "" {
		c.Auth.Enabled = true
		c.Auth.Keys = splitList(v)
	}
	if v := os.Getenv("BEIAN2API_PROVIDERS"); v != "" {
		c.Providers = parseProviders(v)
	}
	if v := os.Getenv("BEIAN2API_WEBHOOK"); v != "" {
		c.Watch.Webhook = v
	}
	if v := os.Getenv("BEIAN2API_NOTIFY_CHANNELS"); v != "" {
		// 形如 "dingtalk:https://oapi.dingtalk.com/robot/send?access_token=x,wecom:https://..."
		// 容器里一行配完，不用挂 yaml。
		c.Notify.Channels = parseNotifyChannels(v)
	}
	if v := os.Getenv("BEIAN2API_WATCH_INTERVAL"); v != "" {
		// 支持 "6h" / "30m" / "86400" 三种写法，容器里配一行就够。
		if n, err := strconv.Atoi(v); err == nil {
			c.Watch.Interval = n
		} else if d, err := time.ParseDuration(v); err == nil && d > 0 {
			c.Watch.Interval = int(d / time.Second)
		}
	}
	if v := os.Getenv("BEIAN2API_WATCH_ENABLED"); v != "" {
		c.Watch.Enabled = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("BEIAN2API_TTL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Cache.TTL = n
		}
	}
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parseProviders 解析形如 "miit:1,apihz:0.4" 的紧凑写法，权重 >0 即启用。
func parseProviders(v string) []Provider {
	var out []Provider
	for _, item := range splitList(v) {
		name, weightStr, _ := strings.Cut(item, ":")
		w := 100
		if weightStr != "" {
			if f, err := strconv.ParseFloat(weightStr, 64); err == nil {
				w = int(f * 100)
			}
		}
		out = append(out, Provider{Name: name, Enabled: w > 0, Weight: w, Timeout: 8})
	}
	return out
}

// Validate 做基本校验并补齐依赖默认值。
func (c *Config) Validate() error {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if c.Cache.TTL < 0 || c.Cache.Negative < 0 || c.Cache.StaleMax < 0 {
		return fmt.Errorf("缓存时长不能为负数")
	}
	if c.Cache.StaleMax == 0 {
		c.Cache.StaleMax = 604800
	}
	if c.RateLimit.RPS == 0 {
		c.RateLimit.RPS = 20
	}
	if c.RateLimit.Burst == 0 {
		c.RateLimit.Burst = c.RateLimit.RPS * 3
	}
	if c.Watch.Interval <= 0 {
		c.Watch.Interval = 86400
	}
	if c.Watch.Workers <= 0 {
		c.Watch.Workers = 4
	}
	if len(c.Providers) == 0 {
		c.Providers = Default().Providers
	}
	if c.Notify.Retries <= 0 {
		c.Notify.Retries = 3
	}
	if c.Notify.Retries > 10 {
		c.Notify.Retries = 10
	}
	for i := range c.Notify.Channels {
		ch := &c.Notify.Channels[i]
		if strings.TrimSpace(ch.URL) == "" {
			return fmt.Errorf("notify.channels[%d] 缺少 url", i)
		}
		if !strings.HasPrefix(ch.URL, "http://") && !strings.HasPrefix(ch.URL, "https://") {
			return fmt.Errorf("notify.channels[%d].url 必须是 http(s) 地址", i)
		}
		ch.Kind = strings.ToLower(strings.TrimSpace(ch.Kind))
	}
	for i := range c.Providers {
		if c.Providers[i].Name == "" {
			return fmt.Errorf("providers[%d] 缺少 name", i)
		}
		if c.Providers[i].Timeout <= 0 {
			c.Providers[i].Timeout = 8
		}
	}
	sortProviders(c.Providers)
	return nil
}

// sortProviders 按权重降序，权重相同保持声明顺序（稳定排序）。
func sortProviders(ps []Provider) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && ps[j].Weight > ps[j-1].Weight; j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
}

// Channels 返回全部推送通道：显式配置的 notify.channels 优先；
// 只配了旧的 watch.webhook 时，把它当成一个 generic 通道，老配置继续能用。
func (c *Config) Channels() []NotifyChannel {
	var out []NotifyChannel
	out = append(out, c.Notify.Channels...)
	if c.Watch.Webhook != "" {
		for _, ch := range out {
			if ch.URL == c.Watch.Webhook {
				return out
			}
		}
		out = append(out, NotifyChannel{Name: "webhook", Kind: "generic", URL: c.Watch.Webhook})
	}
	return out
}

// parseNotifyChannels 解析 "kind:url" 形式的紧凑配置。url 里的冒号不影响切割，
// 因为只按第一个冒号切，且要求前半段不含 "//"。
func parseNotifyChannels(v string) []NotifyChannel {
	var out []NotifyChannel
	for _, item := range splitList(v) {
		kind, u, ok := strings.Cut(item, ":")
		// "https://host/hook" 不带 kind：前缀本身就是 scheme，不能当成通道类型。
		if !ok || strings.Contains(kind, "/") || kind == "http" || kind == "https" {
			out = append(out, NotifyChannel{URL: item})
			continue
		}
		out = append(out, NotifyChannel{Kind: kind, URL: u})
	}
	return out
}

// EnabledProviders 返回启用中的上游名字，按权重排序。
func (c *Config) EnabledProviders() []string {
	var out []string
	for _, p := range c.Providers {
		if p.Enabled && p.Weight > 0 {
			out = append(out, p.Name)
		}
	}
	return out
}

// Dump 把当前配置序列化成 YAML，供 `beian2api config` 输出。
func (c *Config) Dump() ([]byte, error) { return yaml.Marshal(c) }

// WatchInterval 返回监控间隔。
func (c *Config) WatchInterval() time.Duration { return time.Duration(c.Watch.Interval) * time.Second }
