// Package provider 定义上游数据源适配器与调度逻辑。
//
// 设计要点：所有适配器都返回同一形状的 []model.Record；
// 多个适配器按权重并行查询，先成功的赢（race-to-first-success），
// 失败的下游降级，全失败时由 service 层回退到本地快照。
package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/asoe/beian2api/internal/model"
)

// ErrNotFound 表示上游明确回复"没有备案"（这与"查询失败"不同，可负缓存）。
var ErrNotFound = errors.New("no record")

// ErrUpstream 表示上游不可用或返回异常。
var ErrUpstream = errors.New("upstream error")

// Provider 是一个数据源。
type Provider interface {
	Name() string
	Lookup(ctx context.Context, domain string) ([]model.Record, error)
}

// Options 是构造适配器的通用配置。
type Options struct {
	Timeout time.Duration
	HTTP    *http.Client
	// ID / Key 是第三方接口的凭据（目前只有 apihz 用得上）。
	//
	// 之前这两个值只存在于 apihz 适配器内部的常量里，配置里根本没有
	// 能填它的地方——README 写着「生产环境建议换成自己的」，而实际上
	// 换不了，所有人都只能挤同一份公共测试凭据，被限流时束手无策。
	ID  string
	Key string
}

// NewHTTPClient 返回一个带连接池与超时的 HTTP 客户端。
func NewHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

// Registry 保存已注册的适配器工厂。
type Registry struct {
	mu        sync.RWMutex
	factories map[string]func(Options) Provider
}

// NewRegistry 创建一个空注册表并内置全部适配器。
func NewRegistry() *Registry {
	r := &Registry{factories: map[string]func(Options) Provider{}}
	r.Register("miit", func(o Options) Provider { return NewMIIT(o) })
	r.Register("apihz", func(o Options) Provider { return NewAPIHZ(o).WithCredential(o.ID, o.Key) })
	r.Register("mock", NewMock)
	return r
}

// Register 注册适配器工厂。
func (r *Registry) Register(name string, f func(Options) Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[strings.ToLower(name)] = f
}

// Names 返回全部已注册的适配器名。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.factories))
	for k := range r.factories {
		out = append(out, k)
	}
	return out
}

// Build 按名字构造适配器。
func (r *Registry) Build(name string, o Options) (Provider, error) {
	r.mu.RLock()
	f, ok := r.factories[strings.ToLower(name)]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("未知数据源 %q", name)
	}
	return f(o), nil
}

// Task 是一个带权重的适配器实例。
type Task struct {
	Provider Provider
	Weight   int
	Timeout  time.Duration
}

// Outcome 是并发查询的汇总结果。
type Outcome struct {
	Records []model.Record
	Source  string
	// AllFailed 为 true 时表示所有上游都挂了（并非"无备案"）。
	AllFailed bool
	// Cancelled 为 true 表示调用方自己走了（客户端断连、请求超时），
	// 不是因为上游坏了。两者必须分开：混成 AllFailed 会让巡检把
	// 「浏览器关掉页面」记成一次上游故障，甚至推一条告警出去。
	Cancelled bool
	Errors    map[string]string
}

// hedgeStep 是「权重落后多少」对应的起跑延迟上限。
//
// 权重原来是纯装饰：配置里写着「weight 越大越优先」，竞速里却完全不看它，
// 所有源同时起跑、谁快谁赢。于是把官方源的权重调到 100、备用源 40，
// 效果和把两者设成一样没有区别。
//
// 现在低权重的源晚一点起跑：权重最高的立刻出发，其余按权重差按比例延后，
// 最大不超过 hedgeStep。这是对冲（hedging）而不是排队——高权重源慢了或者
// 挂了，后面的源照样会出发，只是让位一小会儿。权重全相等时延迟为 0，
// 「所有源同时竞速」的行为完全不变。
var hedgeStep = 200 * time.Millisecond

// hedgeDelay 计算某个权重相对最高权重的起跑延迟。
func hedgeDelay(weight, maxWeight int) time.Duration {
	if maxWeight <= 0 || weight >= maxWeight || weight <= 0 {
		return 0
	}
	step := hedgeStep * time.Duration(maxWeight-weight) / time.Duration(maxWeight)
	if step < 0 {
		return 0
	}
	return step
}

// Race 并行查询多个上游，返回第一个拿到数据的答案。
//
// 语义划分很关键：
//   - 有记录 → 直接采用，取到即取消其余请求；
//   - 明确"无备案"（ErrNotFound）→ 作为兜底答案，但继续等别人，可能有人有数据；
//   - 上游报错 → 记录错误，不影响结论。
//
// 全部失败时 AllFailed=true，由上层回退到陈旧快照。
func Race(ctx context.Context, domain string, tasks []Task) Outcome {
	out := Outcome{Errors: make(map[string]string, len(tasks))}
	if len(tasks) == 0 {
		out.AllFailed = true
		return out
	}
	type reply struct {
		name    string
		records []model.Record
		err     error
	}
	ch := make(chan reply, len(tasks))
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	maxWeight := 0
	for _, t := range tasks {
		if t.Weight > maxWeight {
			maxWeight = t.Weight
		}
	}

	for _, t := range tasks {
		go func(t Task) {
			tctx := cctx
			if t.Timeout > 0 {
				var c context.CancelFunc
				tctx, c = context.WithTimeout(cctx, t.Timeout)
				defer c()
			}
			// 低权重源让位一小会儿，见 hedgeDelay 的注释。
			if d := hedgeDelay(t.Weight, maxWeight); d > 0 {
				select {
				case <-time.After(d):
				case <-tctx.Done():
					ch <- reply{name: t.Provider.Name(), err: tctx.Err()}
					return
				}
			}
			recs, err := t.Provider.Lookup(tctx, domain)
			ch <- reply{name: t.Provider.Name(), records: recs, err: err}
		}(t)
	}

	notFoundFrom := ""
	decided := false
	// 拿到"无备案"后只再等一小会儿：给快源一个机会，也不让慢源拖死响应。
	// 5.6s 的等待对使用者是灾难，而多等 800ms 几乎察觉不到。
	var grace <-chan time.Time
drain:
	for i := 0; i < len(tasks); i++ {
		var r reply
		select {
		case <-ctx.Done():
			// 调用方的 ctx 已经取消：再等下去只是白等——结果没人要，
			// 而且会被上层当成「上游不可用」，污染巡检对故障的判断。
			out.Cancelled = true
			break drain
		case <-grace:
			if !decided && notFoundFrom != "" {
				out.Source, out.Records = notFoundFrom, []model.Record{}
				decided = true
			}
			// 结论已定就收工，不再等剩下的源。
			//
			// 之前这里是 continue，循环条件又是「收满 len(tasks) 个回复」，
			// 于是决定虽然做完了，人还是站在那儿等慢源。有数据的那一支侥幸
			// 没事（cancel() 会让其余源立刻返回），而「无备案」这一支没有
			// cancel：三个源里最快的那个 10ms 就答了「查不到」，最慢的那个
			// 挂到超时才回，这 800ms 的宽限就变成了最长那个源的超时时间。
			// 配置里单源超时 8 秒，一次「查不到」的查询就要 8 秒。
			//
			// 不再收也不会漏东西：ch 的缓冲是 len(tasks)，没被读走的回复
			// 照样能写出去，goroutine 不会卡住。
			break drain
		case r = <-ch:
		}
		if decided {
			// 有数据的那一支：cancel() 已经通知其余源收工，这里同样直接返回。
			break drain
		}
		switch {
		case r.err == nil && len(r.records) > 0:
			out.Records, out.Source, decided = r.records, r.name, true
			cancel()
		case r.err == nil || errors.Is(r.err, ErrNotFound):
			if notFoundFrom == "" {
				notFoundFrom = r.name
				grace = time.After(800 * time.Millisecond)
			}
		case r.err != nil:
			out.Errors[r.name] = r.err.Error()
		}
	}
	if out.Source == "" && !out.Cancelled {
		if notFoundFrom != "" {
			out.Source = notFoundFrom // 上游明确表示无备案
			out.Records = []model.Record{}
		} else {
			out.AllFailed = true
		}
	}
	return out
}

// cleaner 是适配器共用的字符串清洗：把各种"查询失败"占位符变成空串。
func cleaner(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case s == "", s == "查询失败", s == "null", s == "-", s == "暂无", s == "无":
		return ""
	}
	return s
}

// joinNonEmpty 取第一个非空值。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := cleaner(v); s != "" {
			return s
		}
	}
	return ""
}

// buildURL 拼 URL 并保留已有 query。
func buildURL(base string, params map[string]string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}
