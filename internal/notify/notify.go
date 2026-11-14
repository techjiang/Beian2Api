// Package notify 把备案变更推送到外部通道：钉钉、企业微信、飞书、Slack、任意 HTTP。
//
// 之所以单独成包而不是在 watch 里拼 JSON：各家 IM 的机器人协议不一样，
// 而且它们有一个共同陷阱——HTTP 200 不代表送达。钉钉和企业微信在
// 「token 失效」「关键词不匹配」「被限流」时照样返回 200，错误藏在响应体的
// errcode 里。只看状态码会把「一条都没发出去」记成成功，比不推送更糟。
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// 支持的通道类型。
const (
	KindDingTalk = "dingtalk"
	KindWeCom    = "wecom"
	KindFeishu   = "feishu"
	KindSlack    = "slack"
	KindGeneric  = "generic"
)

// 事件类型。
const (
	EventChanged    = "changed"
	EventRevoked    = "revoked"
	EventRegistered = "registered"
	// EventUpstreamError / EventRecovered 是「上游打不通」这条链路的事件。
	// 一个域名连续几轮都没法回源，比备案号变了更急：你不知道它现在是死是活。
	EventUpstreamError = "upstream_error"
	EventRecovered     = "recovered"
	EventTest          = "test"
)

// Channel 是一个推送通道配置。
type Channel struct {
	Name     string   `yaml:"name" json:"name"`
	Kind     string   `yaml:"kind" json:"kind"` // 留空按 URL 自动识别
	URL      string   `yaml:"url" json:"url"`
	Secret   string   `yaml:"secret" json:"-"`      // 钉钉加签 / 飞书签名密钥
	Events   []string `yaml:"events" json:"events"` // 只推这些事件；空 = 全部
	Disabled bool     `yaml:"disabled" json:"disabled"`
}

// Field 是一个字段级变化。
type Field struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// Event 是一条待推送的消息。
type Event struct {
	Kind    string  `json:"kind"`
	Domain  string  `json:"domain"`
	At      int64   `json:"at"`
	ICPCode string  `json:"icp_code"`
	Diff    []Field `json:"diff,omitempty"`
	BaseURL string  `json:"base_url,omitempty"`
}

// State 是某个通道最近一次的投递状态。
type State struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	LastAt    int64  `json:"last_at"`
	LastOK    bool   `json:"last_ok"`
	LastError string `json:"last_error,omitempty"`
	Attempts  int    `json:"attempts"`
	OK        int    `json:"ok"`
	Failed    int    `json:"failed"`
}

// Result 是一次投递的结果。
type Result struct {
	Channel  string `json:"channel"`
	Kind     string `json:"kind"`
	OK       bool   `json:"ok"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error,omitempty"`
}

// Logger 是推送日志所需的最小接口。
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// Recorder 持久化投递状态；由调用方注入，避免 notify 依赖存储实现。
type Recorder func(State)

// Dispatcher 向全部启用通道投递消息。
type Dispatcher struct {
	channels []Channel
	client   *http.Client
	log      Logger
	record   Recorder
	retries  int
	backoff  time.Duration

	mu     sync.Mutex
	states map[string]State

	wg sync.WaitGroup
}

const (
	defaultRetries = 3
	defaultBackoff = 600 * time.Millisecond
	maxRespBytes   = 8 << 10
)

// New 构造 Dispatcher。通道名重复或为空时自动补名，避免状态互相覆盖。
func New(channels []Channel, log Logger) *Dispatcher {
	d := &Dispatcher{
		client:  &http.Client{Timeout: 10 * time.Second},
		log:     log,
		retries: defaultRetries,
		backoff: defaultBackoff,
		states:  map[string]State{},
	}
	seen := map[string]int{}
	for _, ch := range channels {
		if strings.TrimSpace(ch.URL) == "" || ch.Disabled {
			continue
		}
		ch.Kind = NormalizeKind(ch.Kind, ch.URL)
		if ch.Name == "" {
			ch.Name = ch.Kind
		}
		if n := seen[ch.Name]; n > 0 {
			seen[ch.Name] = n + 1
			ch.Name = fmt.Sprintf("%s-%d", ch.Name, n+1)
		} else {
			seen[ch.Name] = 1
		}
		d.channels = append(d.channels, ch)
	}
	return d
}

// WithRecorder 注入状态持久化回调。
func (d *Dispatcher) WithRecorder(fn Recorder) *Dispatcher {
	d.record = fn
	return d
}

// WithRetries 覆盖重试策略（测试用）。
func (d *Dispatcher) WithRetries(retries int, backoff time.Duration) *Dispatcher {
	if retries > 0 {
		d.retries = retries
	}
	if backoff > 0 {
		d.backoff = backoff
	}
	return d
}

// Channels 返回全部启用通道。
func (d *Dispatcher) Channels() []Channel {
	if d == nil {
		return nil
	}
	return d.channels
}

// Enabled 报告是否有任何可用通道。
func (d *Dispatcher) Enabled() bool { return d != nil && len(d.channels) > 0 }

// States 返回各通道最近状态快照。
func (d *Dispatcher) States() map[string]State {
	out := map[string]State{}
	if d == nil {
		return out
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, v := range d.states {
		out[k] = v
	}
	return out
}

// Seed 用落盘状态初始化内存状态（重启后仍能看到上次推送是否成功）。
func (d *Dispatcher) Seed(states map[string]State) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, v := range states {
		d.states[k] = v
	}
}

// SendAsync 后台投递，不阻塞巡检主流程。
func (d *Dispatcher) SendAsync(ev Event) {
	if !d.Enabled() {
		return
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.Send(context.Background(), ev)
	}()
}

// Wait 等待全部后台投递结束（测试与优雅关闭用）。
func (d *Dispatcher) Wait() {
	if d == nil {
		return
	}
	d.wg.Wait()
}

// Send 同步投递到全部匹配的通道，返回每个通道的结果。
func (d *Dispatcher) Send(ctx context.Context, ev Event) []Result {
	if d == nil {
		return nil
	}
	var out []Result
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, ch := range d.channels {
		if !ch.Match(ev.Kind) {
			continue
		}
		wg.Add(1)
		go func(ch Channel) {
			defer wg.Done()
			r := d.deliver(ctx, ch, ev)
			mu.Lock()
			out = append(out, r)
			mu.Unlock()
		}(ch)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Channel < out[j].Channel })
	for _, r := range out {
		if !r.OK {
			d.log.Warn("推送失败", "channel", r.Channel, "kind", r.Kind, "attempts", r.Attempts, "err", r.Error)
		}
	}
	return out
}

// Match 判断该通道是否要接收这个事件。空 Events 表示全收。
func (c Channel) Match(kind string) bool {
	if kind == EventTest || len(c.Events) == 0 {
		return true
	}
	for _, e := range c.Events {
		if strings.EqualFold(strings.TrimSpace(e), kind) {
			return true
		}
	}
	return false
}

// deliver 带重试地发一条消息。
func (d *Dispatcher) deliver(ctx context.Context, ch Channel, ev Event) Result {
	res := Result{Channel: ch.Name, Kind: ch.Kind}
	var lastErr error
	for attempt := 1; attempt <= d.retries; attempt++ {
		res.Attempts = attempt
		err := d.once(ctx, ch, ev)
		if err == nil {
			res.OK = true
			res.Error = ""
			d.setOK(ch)
			return res
		}
		lastErr = err
		var fatal fatalError
		if errors.As(err, &fatal) {
			break // 语义性错误（填错 token、关键词不匹配）重试也没用
		}
		if attempt < d.retries {
			select {
			case <-ctx.Done():
				lastErr = ctx.Err()
				attempt = d.retries
			case <-time.After(time.Duration(attempt) * d.backoff):
			}
		}
	}
	res.Error = lastErr.Error()
	d.setFail(ch, res.Error, res.Attempts)
	return res
}

// fatalError 表示重试无法修复的错误。
type fatalError struct{ msg string }

func (e fatalError) Error() string { return e.msg }

func (d *Dispatcher) once(ctx context.Context, ch Channel, ev Event) error {
	target := ch.URL
	body, err := payload(ch, ev)
	if err != nil {
		return fatalError{"构造载荷失败: " + err.Error()}
	}
	if ch.Kind == KindDingTalk && ch.Secret != "" {
		target = dingTalkSignedURL(ch.URL, ch.Secret, time.Now())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fatalError{"推送地址不合法: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Beian2API-Notifier/1.0")

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))

	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fatalError{fmt.Sprintf("HTTP %d: %s", resp.StatusCode, snippet(raw))}
	}
	// 200 也可能是失败：IM 把错误放在响应体里。
	if err := checkSemantic(ch.Kind, raw); err != nil {
		return err
	}
	return nil
}

// checkSemantic 判断 IM 是否在 200 响应里报告了错误。
func checkSemantic(kind string, raw []byte) error {
	if len(bytes.TrimSpace(raw)) == 0 || kind == KindGeneric {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil // 非 JSON（比如自建网关回了 "ok"），按成功算
	}
	// 钉钉/企业微信/飞书都用 errcode / code 两个字段之一。
	for _, key := range []string{"errcode", "code", "StatusCode"} {
		v, ok := m[key]
		if !ok {
			continue
		}
		if num, ok := toInt(v); ok && num != 0 {
			msg := fmt.Sprintf("接口返回错误 %s=%d: %s", key, num, firstNonEmpty(m))
			if retryableCode(num) {
				return errors.New(msg)
			}
			return fatalError{msg}
		}
	}
	// 飞书的成功响应里 code 为 0，失败时是 msg 非空；部分版本只给 msg。
	if s, ok := m["msg"].(string); ok && strings.Contains(strings.ToLower(s), "error") {
		return fmt.Errorf("接口返回错误: %s", s)
	}
	return nil
}

// retryableCode 判断上游错误码是否值得重试：只有限流才算。
//
// 其余（token 失效、关键词不匹配、机器人被移除）都是配置问题，
// 重试三次只是把同一条错误打三遍，还拖慢整轮巡检。
func retryableCode(code int64) bool {
	switch code {
	case -1, 130101, 45009, 130429: // 钉钉/企业微信的限流与系统繁忙
		return true
	}
	return false
}

func toInt(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

func firstNonEmpty(m map[string]any) string {
	for _, k := range []string{"errmsg", "msg", "message", "StatusMessage"} {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func (d *Dispatcher) setOK(ch Channel) {
	now := time.Now().Unix()
	d.mu.Lock()
	st := d.states[ch.Name]
	st.Name, st.Kind = ch.Name, ch.Kind
	st.LastAt, st.LastOK, st.LastError, st.Attempts = now, true, "", 1
	st.OK++
	d.states[ch.Name] = st
	d.mu.Unlock()
	if d.record != nil {
		d.record(st)
	}
}

func (d *Dispatcher) setFail(ch Channel, msg string, attempts int) {
	now := time.Now().Unix()
	d.mu.Lock()
	st := d.states[ch.Name]
	st.Name, st.Kind = ch.Name, ch.Kind
	st.LastAt, st.LastOK, st.LastError, st.Attempts = now, false, msg, attempts
	st.Failed++
	d.states[ch.Name] = st
	d.mu.Unlock()
	if d.record != nil {
		d.record(st)
	}
}

// NormalizeKind 归一化通道类型，留空时按 URL 猜。
func NormalizeKind(kind, rawURL string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case KindDingTalk, "dingding":
		return KindDingTalk
	case KindWeCom, "qywx", "weixin", "wechat":
		return KindWeCom
	case KindFeishu, "lark":
		return KindFeishu
	case KindSlack:
		return KindSlack
	case KindGeneric, "http", "json":
		return KindGeneric
	}
	u := strings.ToLower(rawURL)
	switch {
	case strings.Contains(u, "oapi.dingtalk.com"):
		return KindDingTalk
	case strings.Contains(u, "qyapi.weixin.qq.com"):
		return KindWeCom
	case strings.Contains(u, "open.feishu.cn"), strings.Contains(u, "open.larksuite.com"):
		return KindFeishu
	case strings.Contains(u, "hooks.slack.com"):
		return KindSlack
	}
	return KindGeneric
}

// dingTalkSignedURL 按钉钉要求加 timestamp 与 sign。
func dingTalkSignedURL(raw, secret string, now time.Time) string {
	ts := fmt.Sprintf("%d", now.UnixMilli())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	sep := "?"
	if strings.Contains(raw, "?") {
		sep = "&"
	}
	return raw + sep + "timestamp=" + ts + "&sign=" + sign
}

// feishuSign 按飞书要求生成签名：以 "timestamp\nsecret" 为密钥，对空串做 HMAC。
func feishuSign(secret string, now time.Time) (string, string) {
	ts := fmt.Sprintf("%d", now.Unix())
	mac := hmac.New(sha256.New, []byte(ts+"\n"+secret))
	mac.Write([]byte{})
	return ts, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
