// Package config 负责配置加载：默认值 -> YAML 文件 -> 环境变量，逐层覆盖。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Seconds 是「以秒为单位、但可以写成 6h / 30m / 90」的时长配置值。
//
// 为什么需要它：README 教人用 BEIAN2API_WATCH_INTERVAL=6h，环境变量那条路
// 认时间写法；而 YAML 里的 interval / cache.ttl 只认整数秒。于是把同样的
// 6h 写进 config.yaml，服务直接起不来，报一句
// `cannot unmarshal !!str `6h` into int`——同一套配置，两种写法，一种能用。
// 配时长时写 6h 是最自然的手感，这里让它成立。
type Seconds int

// UnmarshalYAML 同时接受整数秒与带单位的时间串。
//
// 带单位时走 time.ParseDuration（6h / 30m / 1h30m），
// 纯数字仍然按秒，和旧配置、环境变量、文档保持一致。
// 写错时报错要指到具体键上：`cache.ttl` 里写了个 6h 却不生效，
// 比直接拒绝更糟——缓存的过期时间会静默变成别的数。
func (s *Seconds) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		*s = 0
		return nil
	}
	if n, err := strconv.Atoi(raw); err == nil {
		*s = Seconds(n)
		return nil
	}
	d, err := parseDurationLoose(raw)
	if err != nil {
		return fmt.Errorf("%q 不是合法时长：写整数秒（600）或带单位（10m、6h、7d）", raw)
	}
	*s = Seconds(d / time.Second)
	return nil
}

// parseDurationLoose 在 time.ParseDuration 之上补一个 `d`（天）单位。
//
// 缓存回退窗口默认是 7 天，写 168h 能算出同样的值，但没人会那样想，
// 而写 7d 会被标准库拒绝（它只认到小时）。这类配置项的语义单位本来就是
// 「天」，多认一个 d 比让人对着 168h 做换算更实在。
func parseDurationLoose(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64); err == nil {
			if n < 0 {
				return 0, fmt.Errorf("%q 是负数", raw)
			}
			return time.Duration(n * float64(24*time.Hour)), nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("%q 是负数", raw)
	}
	return d, nil
}

// Provider 描述一个上游数据源的开关与权重。
type Provider struct {
	Name    string `yaml:"name" json:"name"`
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Weight  int    `yaml:"weight" json:"weight"`
	Timeout int    `yaml:"timeout" json:"timeout"` // 秒
	// ID / Key 是第三方接口凭据，目前只有 apihz 用得上。
	// 用 yaml/json 双标签而不是只管 yaml：`beian2api config` 要能看出
	// 凭据「配了没配」，但值本身不该出现在任何对外 JSON 里，所以 json 用 "-"。
	ID  string `yaml:"id" json:"-"`
	Key string `yaml:"key" json:"-"`
	// HasCredential 用于 dump 时提示凭据是否已配置（不泄露内容）。
	HasCredential bool `yaml:"-" json:"has_credential,omitempty"`
}

// Config 是全部运行时配置。
type Config struct {
	Listen   string `yaml:"listen" json:"listen"`
	DataDir  string `yaml:"data_dir" json:"data_dir"`
	LogLevel string `yaml:"log_level" json:"log_level"`
	BaseURL  string `yaml:"base_url" json:"base_url"`
	// TrustProxy 为真时才采信 X-Forwarded-For / X-Real-IP。
	//
	// 默认关：这几个头是客户端随便填的，无条件采信等于把限流的 key
	// 交给攻击者自己选——每个请求换个 X-Real-IP，就每个请求都拿到一桶新的令牌。
	// 只有确实跑在反向代理后面时才该打开。
	TrustProxy bool `yaml:"trust_proxy" json:"trust_proxy"`

	Cache struct {
		TTL      Seconds `yaml:"ttl" json:"ttl"`             // 命中缓存有效期
		Negative Seconds `yaml:"negative" json:"negative"`   // 未备案结果缓存
		StaleMax Seconds `yaml:"stale_max" json:"stale_max"` // 允许回退的陈旧数据上限
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
		Enabled  bool    `yaml:"enabled" json:"enabled"`
		Interval Seconds `yaml:"interval" json:"interval"` // 巡检间隔
		Workers  int     `yaml:"workers" json:"workers"`
		// FailThreshold 是「连续几轮回源失败才推送告警」，1 会被偶发抖动刷屏。
		FailThreshold int    `yaml:"fail_threshold" json:"fail_threshold"`
		Webhook       string `yaml:"webhook" json:"webhook"` // 兼容旧配置，等价于一个 generic 通道
	} `yaml:"watch" json:"watch"`

	Notify Notify `yaml:"notify" json:"notify"`

	// Storage 控制只增数据（变更流水、历史快照）的滚动保留上限。
	//
	// 为什么要配这个：这两个 bucket 只增不删，监控域名多、变更频繁的部署
	// 跑上一年，数据文件会一直长大，而首页永远只显示最近 10 条。
	// 上限设成 0 表示不裁剪（自己接管风险）。
	Storage struct {
		RetainChanges int `yaml:"retain_changes" json:"retain_changes"`
		RetainHistory int `yaml:"retain_history" json:"retain_history"`
		// RetainHistoryTotal 是历史快照的全局上限（跨所有域名）。
		//
		// 只有按域名的配额是不够的：/api/v1/query 公开可调，随便刷一批随机
		// 域名，每个域名一条快照、各自都没超限，合起来却能让文件无限长。
		RetainHistoryTotal int `yaml:"retain_history_total" json:"retain_history_total"`
	} `yaml:"storage" json:"storage"`

	CORS []string `yaml:"cors" json:"cors"`

	// envErrs 收集「环境变量写了但用不了」的错误，由 Validate 统一抛出。
	//
	// 收集而不是立刻返回，是因为人往往一次改好几个环境变量：
	// 一次报全部，比改一个、重启、再看到下一个好得多。
	envErrs []error
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
	c.Watch.FailThreshold = 3
	c.Notify.Retries = 3
	c.Storage.RetainChanges = 2000
	c.Storage.RetainHistory = 200
	c.Storage.RetainHistoryTotal = 20000
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
	if v := os.Getenv("BEIAN2API_TRUST_PROXY"); v != "" {
		c.TrustProxy = truthyEnv(v)
	}
	if v := os.Getenv("BEIAN2API_API_KEYS"); v != "" {
		c.Auth.Enabled = true
		c.Auth.Keys = splitList(v)
	}
	if v := os.Getenv("BEIAN2API_PROVIDERS"); v != "" {
		// 保留 yaml 里同名源的细节（timeout / id / key），只让环境变量决定
		// 「启用哪些、权重多少」。
		//
		// 之前这里是直接整个替换：`config.yaml` 里配好 apihz 的自有凭据、
		// 或者把某个源的超时从默认 8 秒调成 3 秒，只要环境里有
		// `BEIAN2API_PROVIDERS=miit:1,apihz:0.4`（README 就是让人这么写的），
		// 这些细节会被**静默抹掉**——凭据退回公共测试凭据、超时退回 8 秒，
		// 而 `beian2api config` 看起来一切正常，只是 id/key 变成了空。
		c.Providers = mergeProviders(c.Providers, parseProviders(v))
	}
	// 凭据单独给环境变量：一行 BEIAN2API_PROVIDERS 里塞不下，
	// 而且凭据不该和「要不要启用某个源」混在一起。
	if v := os.Getenv("BEIAN2API_APIHZ_ID"); v != "" {
		applyProviderCredential(c.Providers, "apihz", v, os.Getenv("BEIAN2API_APIHZ_KEY"))
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
		// 写错必须报错，不能静默退回默认值——见 envSeconds 的说明。
		if sec, err := parseSecondsLoose(v); err != nil {
			c.envErr("BEIAN2API_WATCH_INTERVAL", v, "写整数秒（86400）或带单位（30m、6h、7d）")
		} else if sec <= 0 {
			c.envErr("BEIAN2API_WATCH_INTERVAL", v, "必须大于 0")
		} else {
			c.Watch.Interval = sec
		}
	}
	if v := os.Getenv("BEIAN2API_WATCH_FAIL_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n <= 0 {
			c.envErr("BEIAN2API_WATCH_FAIL_THRESHOLD", v, "必须是正整数")
		} else {
			c.Watch.FailThreshold = n
		}
	}
	if v := os.Getenv("BEIAN2API_WATCH_ENABLED"); v != "" {
		// 和其它布尔环境变量走同一个解析：这里原来只认 "1" 和 "true"，
		// 于是 `BEIAN2API_WATCH_ENABLED=false` 生效、`=yes` 却被当成开启，
		// 而 `=no` / `=off` 这种写法照样把巡检打开——配的人以为关掉了。
		c.Watch.Enabled = truthyEnv(v)
	}
	if v := os.Getenv("BEIAN2API_RETAIN_CHANGES"); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 0 {
			c.envErr("BEIAN2API_RETAIN_CHANGES", v, "必须是非负整数（0 表示不裁剪）")
		} else {
			c.Storage.RetainChanges = n
		}
	}
	if v := os.Getenv("BEIAN2API_RETAIN_HISTORY"); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 0 {
			c.envErr("BEIAN2API_RETAIN_HISTORY", v, "必须是非负整数（0 表示不裁剪）")
		} else {
			c.Storage.RetainHistory = n
		}
	}
	if v := os.Getenv("BEIAN2API_RETAIN_HISTORY_TOTAL"); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 0 {
			c.envErr("BEIAN2API_RETAIN_HISTORY_TOTAL", v, "必须是非负整数（0 表示不裁剪）")
		} else {
			c.Storage.RetainHistoryTotal = n
		}
	}
	// TTL / 负缓存 / 回退窗口都走同一个时长解析：`BEIAN2API_TTL=6h` 与
	// config.yaml 里的 `ttl: 6h` 必须是同一个值。
	//
	// 之前这里只认整数秒：同一个 6h，写进 yaml 生效（第十六轮刚修的），
	// 写进环境变量却被 `strconv.Atoi` 判错、然后**静默忽略**——服务照常起来，
	// 缓存 TTL 停在默认的 6 小时。配的人以为改过了，而改的和没改一样。
	if v := os.Getenv("BEIAN2API_TTL"); v != "" {
		c.setEnvDuration("BEIAN2API_TTL", v, &c.Cache.TTL)
	}
	if v := os.Getenv("BEIAN2API_NEGATIVE_TTL"); v != "" {
		c.setEnvDuration("BEIAN2API_NEGATIVE_TTL", v, &c.Cache.Negative)
	}
	if v := os.Getenv("BEIAN2API_STALE_MAX"); v != "" {
		c.setEnvDuration("BEIAN2API_STALE_MAX", v, &c.Cache.StaleMax)
	}
}

// envErr 记下一个「环境变量写了但用不了」的错误。
//
// 环境变量的坏值和配置文件的坏值必须一样吵。配置里写 `ttl: 6hours` 会直接
// 拒绝启动（解析失败），而环境变量写 `BEIAN2API_TTL=6hours` 之前是静默忽略：
// 服务起来了，用的是默认值，日志里一个字都没有。容器部署里最容易踩——
// 你在 compose 里改了那一行，重启，然后一切"正常"，只是没生效。
func (c *Config) envErr(key, value, want string) {
	c.envErrs = append(c.envErrs, fmt.Errorf("环境变量 %s=%q 无法识别：%s", key, value, want))
}

// setEnvDuration 解析一个时长型环境变量并写回。
func (c *Config) setEnvDuration(key, value string, dst *Seconds) {
	sec, err := parseSecondsLoose(value)
	if err != nil {
		c.envErr(key, value, "写整数秒（600）或带单位（10m、6h、7d）")
		return
	}
	*dst = sec
}

// parseSecondsLoose 把「纯数字秒」或「带单位时长」都解析成 Seconds。
//
// 纯数字必须先走 Atoi：`time.ParseDuration("90")` 会报错（缺单位），
// 而 `90` 在这套配置里的历史含义就是 90 秒——yaml 那边也是这么认的。
func parseSecondsLoose(raw string) (Seconds, error) {
	raw = strings.TrimSpace(raw)
	if n, err := strconv.Atoi(raw); err == nil {
		return Seconds(n), nil
	}
	d, err := parseDurationLoose(raw)
	if err != nil {
		return 0, err
	}
	return Seconds(d / time.Second), nil
}

func truthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y", "on":
		return true
	}
	return false
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

// mergeProviders 用环境变量给出的清单决定「启用哪些源、权重多少」，
// 但每个源的其它细节（timeout、id、key）沿用配置文件里的值。
//
// 为什么不是「环境变量整份覆盖」：一行 `miit:1,apihz:0.4` 只写得出名字和
// 权重，写不出凭据和超时。整份覆盖的结果是「为了调一下权重，把凭据顺手
// 丢了」——而凭据丢了不报错，只是退回公共测试凭据、被限流时表现为
// 「偶尔查不到」，从任何地方都看不出原因。
func mergeProviders(base, override []Provider) []Provider {
	byName := make(map[string]Provider, len(base))
	for _, p := range base {
		byName[strings.ToLower(p.Name)] = p
	}
	out := make([]Provider, 0, len(override))
	for _, p := range override {
		if old, ok := byName[strings.ToLower(p.Name)]; ok {
			p.Timeout = old.Timeout
			p.ID, p.Key = old.ID, old.Key
		}
		out = append(out, p)
	}
	return out
}

// applyProviderCredential 给某个数据源填凭据，源不存在时忽略。
func applyProviderCredential(ps []Provider, name, id, key string) {
	for i := range ps {
		if strings.EqualFold(ps[i].Name, name) {
			ps[i].ID, ps[i].Key = id, key
			return
		}
	}
}

// knownEvents 是推送可订阅的事件全集，与 notify 包中的事件常量一一对应。
var knownEvents = map[string]struct{}{
	"changed": {}, "revoked": {}, "registered": {},
	"upstream_error": {}, "recovered": {}, "test": {},
}

func eventNames() []string {
	out := make([]string, 0, len(knownEvents))
	for k := range knownEvents {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Validate 做基本校验并补齐依赖默认值。
func (c *Config) Validate() error {
	// 环境变量里的坏值最先报：后面的默认值回填会把它们掩盖掉
	// （比如 interval 写错被当成 0，紧接着被填成默认的 24 小时）。
	if len(c.envErrs) > 0 {
		msgs := make([]string, 0, len(c.envErrs))
		for _, e := range c.envErrs {
			msgs = append(msgs, e.Error())
		}
		return errors.New(strings.Join(msgs, "；"))
	}
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
	// auth.enabled 为真但一把 Key 都没有 —— 这时服务端没有任何可校验的凭据，
	// 结果是「全站裸奔」，而配置文件上明明写着 enabled: true。
	// 启动时直接拒绝，比让人以为已经保护好了安全得多。
	if c.Auth.Enabled && len(c.Auth.Keys) == 0 {
		return fmt.Errorf("auth.enabled 为 true 但 auth.keys 为空：" +
			"这样启动等于不鉴权。用 `beian2api keygen` 生成一把，或设 BEIAN2API_API_KEYS")
	}
	if !c.Auth.Enabled && len(c.Auth.Keys) > 0 {
		// 反向的坑：Key 配了、enabled 忘了打开，接口其实是公开的。
		// 与其猜用户想要哪个，不如直接说出来。
		return fmt.Errorf("配了 auth.keys 但 auth.enabled 为 false：" +
			"当前不会鉴权。想要保护请改成 enabled: true，想公开请删掉 keys")
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
	if c.Watch.FailThreshold <= 0 {
		c.Watch.FailThreshold = 3
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
		// 事件名写错是最难发现的一类配置错误：不会报错，只会「永远不推送」。
		// 有人把 upstream_error 写成 upstream-error，就一直收不到告警，
		// 还以为是上游没问题。这里直接拒绝，并列出合法取值。
		for j, ev := range ch.Events {
			e := strings.ToLower(strings.TrimSpace(ev))
			if _, ok := knownEvents[e]; !ok {
				return fmt.Errorf("notify.channels[%d].events[%d] 未知事件 %q，可选：%s",
					i, j, ev, strings.Join(eventNames(), ", "))
			}
			ch.Events[j] = e
		}
	}
	// 三个上限都是「0 表示不裁剪」。
	//
	// retain_history_total 这里原来有一条 `if 0 { 赋值 20000 }`：它的本意是
	// 「没配就用默认值」，但 yaml / 环境变量解析完已经分不清「没配」和
	// 「显式写了 0」了——默认值在 Default() 里就填好了，走到这儿还是 0
	// 只可能是因为有人明确写了 0（不裁剪）。把它改写回 20000，等于让
	// 「不裁剪」这个合法取值永远配不出来，而配置文件上一个字都没错。
	if c.Storage.RetainChanges < 0 || c.Storage.RetainHistory < 0 || c.Storage.RetainHistoryTotal < 0 {
		return fmt.Errorf("storage.retain_changes / retain_history / retain_history_total 不能为负数")
	}
	if c.Storage.RetainHistory > 0 && c.Storage.RetainHistoryTotal > 0 &&
		c.Storage.RetainHistoryTotal < c.Storage.RetainHistory {
		// 全局上限比单域名上限还小，等于所有域名共用一份配额，
		// 写入时会被立刻裁掉，历史基本存不住。这几乎必然是配错了。
		return fmt.Errorf("storage.retain_history_total(%d) 不能小于 retain_history(%d)",
			c.Storage.RetainHistoryTotal, c.Storage.RetainHistory)
	}
	for i := range c.Providers {
		if c.Providers[i].Name == "" {
			return fmt.Errorf("providers[%d] 缺少 name", i)
		}
		if c.Providers[i].Timeout <= 0 {
			c.Providers[i].Timeout = 8
		}
		c.Providers[i].HasCredential = c.Providers[i].ID != "" && c.Providers[i].Key != ""
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
func (c *Config) WatchInterval() time.Duration {
	return time.Duration(c.Watch.Interval) * time.Second
}
