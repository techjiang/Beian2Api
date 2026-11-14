// Package service 把配置、上游、存储组装成业务能力：
// 缓存优先、多源竞速、失败回退、历史留痕、变更监控。
package service

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/asoe/beian2api/internal/config"
	"github.com/asoe/beian2api/internal/model"
	"github.com/asoe/beian2api/internal/provider"
	"github.com/asoe/beian2api/internal/store"
)

// Service 是业务门面。
type Service struct {
	cfg   *config.Config
	st    *store.Store
	tasks []provider.Task
	log   *slog.Logger

	flight sync.Map // domain -> *call，用来合并并发重复查询
}

// New 构造 Service。
func New(cfg *config.Config, st *store.Store, tasks []provider.Task, log *slog.Logger) *Service {
	return &Service{cfg: cfg, st: st, tasks: tasks, log: log}
}

// Store 暴露底层存储（备份/统计用）。
func (s *Service) Store() *store.Store { return s.st }

// ErrInvalidDomain 表示域名不合法。
var ErrInvalidDomain = errors.New("域名格式不正确")

type call struct {
	done chan struct{}
	res  *model.Result
	err  error
}

// Query 查询一个域名的备案信息。
func (s *Service) Query(ctx context.Context, rawDomain string, opts Options) (*model.Result, error) {
	domain := model.NormalizeDomain(rawDomain)
	if !model.IsValidDomain(domain) {
		return nil, ErrInvalidDomain
	}
	// refresh 必须真回源，不参与合并。
	if opts.Refresh {
		return s.query(ctx, domain, opts)
	}

	// 单飞：同一域名的并发请求只打一次上游，其余等同一个结果。
	fresh := &call{done: make(chan struct{})}
	if v, loaded := s.flight.LoadOrStore(domain, fresh); loaded {
		c := v.(*call)
		select {
		case <-c.done:
			return c.res, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// 回源跑在一个与「发起者」脱钩的 ctx 上。
	//
	// 之前是把发起者的 ctx 直接交给回源：第一个请求断连（关页面、代理超时），
	// 回源被取消，结果写进 call 里——后面所有等同一个结果的请求拿到的是
	// 一次 context canceled，明明它们自己还活着。一个浏览器的关闭能连带
	// 打掉一批正在等同一域名的客户端。
	//
	// 现在发起者自己按 ctx 提前返回，回源继续跑完并把结果留给后来者。
	// 回源本身有每个上游的超时兜底，不会无限期挂着。
	go func() {
		defer close(fresh.done)
		defer s.flight.Delete(domain)
		fresh.res, fresh.err = s.query(context.WithoutCancel(ctx), domain, opts)
	}()

	select {
	case <-fresh.done:
		return fresh.res, fresh.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) query(ctx context.Context, domain string, opts Options) (*model.Result, error) {
	start := time.Now()
	now := model.Now()

	entry, cerr := s.st.GetCache(domain)
	fresh := cerr == nil && entry.ExpireAt > now
	if fresh && !opts.Refresh {
		s.st.Incr("hits", 1)
		return &model.Result{
			Domain:    domain,
			Found:     entry.Found,
			Records:   entry.Records,
			Cached:    true,
			Source:    entry.Source,
			CostMS:    ms(start),
			FetchedAt: entry.FetchedAt,
		}, nil
	}

	out := provider.Race(ctx, domain, s.tasks)
	if out.Source != "" {
		found := len(out.Records) > 0
		expire := now + int64(s.cfg.Cache.TTL)
		if !found {
			expire = now + int64(s.cfg.Cache.Negative)
		}
		e := &store.Entry{
			Domain:    domain,
			Found:     found,
			Records:   out.Records,
			Source:    out.Source,
			FetchedAt: now,
			ExpireAt:  expire,
		}
		if err := s.st.PutCache(e); err != nil {
			s.log.Warn("写入缓存失败", "domain", domain, "err", err)
		}
		s.st.Incr("misses", 1)
		if found {
			s.recordHistory(domain, out.Records)
		}
		return &model.Result{
			Domain:    domain,
			Found:     found,
			Records:   out.Records,
			Source:    out.Source,
			CostMS:    ms(start),
			FetchedAt: now,
		}, nil
	}

	// 全部上游失败：回退到本地快照，哪怕已过期也总比报错强。
	if cerr == nil && entry != nil {
		age := now - entry.FetchedAt
		if s.cfg.Cache.StaleMax <= 0 || age <= int64(s.cfg.Cache.StaleMax) {
			s.st.Incr("stale", 1)
			return &model.Result{
				Domain:    domain,
				Found:     entry.Found,
				Records:   entry.Records,
				Stale:     true,
				Cached:    true,
				Source:    entry.Source,
				CostMS:    ms(start),
				FetchedAt: entry.FetchedAt,
				Notice:    "上游数据源暂不可用，已返回本地最后快照（" + time.Unix(entry.FetchedAt, 0).Format("2006-01-02 15:04") + "）",
			}, nil
		}
	}

	// 调用方自己走了（断连、超时）：如实返回 ctx 错误，而不是「上游不可用」。
	// 后者会让人以为数据源出了问题，实际上是我们不等了。
	if out.Cancelled {
		return nil, ctx.Err()
	}

	detail := "上游数据源均不可用"
	if len(out.Errors) > 0 {
		parts := make([]string, 0, len(out.Errors))
		for k, v := range out.Errors {
			parts = append(parts, k+": "+v)
		}
		sort.Strings(parts)
		detail += "（" + strings.Join(parts, "; ") + "）"
	}
	return nil, errors.New(detail)
}

// recordHistory 只在内容有变化时追加历史，天然避免数据膨胀。
func (s *Service) recordHistory(domain string, recs []model.Record) {
	if len(recs) == 0 {
		return
	}
	// 一个域名可能对应多条备案（主体下多个网站），代表记录的选择规则
	// 收敛在 model.Primary：巡检、徽章、历史必须挑同一条，否则 diff 会凭空多出差异。
	cur, ok := model.Primary(recs)
	if !ok {
		return
	}
	hist, _ := s.st.History(domain, 1)
	if len(hist) > 0 && hist[len(hist)-1].Fingerprint() == cur.Fingerprint() {
		return
	}
	if err := s.st.AppendHistory(cur); err != nil {
		s.log.Warn("写入历史失败", "domain", domain, "err", err)
	}
}

// Options 控制单次查询行为。
type Options struct {
	Refresh bool // 跳过缓存，强制回源
}

// BatchItem 是批量查询的一项结果。
type BatchItem struct {
	Domain string        `json:"domain"`
	Result *model.Result `json:"result,omitempty"`
	Error  string        `json:"error,omitempty"`
}

// Batch 并发查询多个域名，限制并发度以免打爆上游。
func (s *Service) Batch(ctx context.Context, domains []string, opts Options, concurrency int) []BatchItem {
	if concurrency <= 0 {
		concurrency = 5
	}
	if concurrency > 16 {
		concurrency = 16
	}
	out := make([]BatchItem, len(domains))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, d := range domains {
		wg.Add(1)
		go func(i int, d string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r, err := s.Query(ctx, d, opts)
			item := BatchItem{Domain: model.NormalizeDomain(d)}
			if err != nil {
				item.Error = err.Error()
			} else {
				item.Result = r
			}
			out[i] = item
		}(i, d)
	}
	wg.Wait()
	return out
}

// History 返回某域名的历史快照。
func (s *Service) History(domain string, limit int) ([]model.Record, error) {
	d := model.NormalizeDomain(domain)
	if !model.IsValidDomain(d) {
		return nil, ErrInvalidDomain
	}
	return s.st.History(d, limit)
}

// Diff 比较两个快照，返回发生变化的字段。
type FieldChange struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// DiffRecords 逐字段比较两条记录。
func DiffRecords(a, b *model.Record) []FieldChange {
	if a == nil || b == nil {
		return nil
	}
	pairs := []struct {
		name string
		x, y string
	}{
		{"主办单位", a.UnitName, b.UnitName},
		{"单位性质", a.UnitType, b.UnitType},
		{"网站名称", a.SiteName, b.SiteName},
		{"备案号", a.ICPCode, b.ICPCode},
		{"备案性质", a.Nature, b.Nature},
		{"审核时间", a.AuditTime, b.AuditTime},
	}
	var out []FieldChange
	for _, p := range pairs {
		if p.x != p.y {
			out = append(out, FieldChange{Field: p.name, From: p.x, To: p.y})
		}
	}
	return out
}

func ms(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000
}
