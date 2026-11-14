// Package watch 提供备案变更巡检：定时回源比对，记录差异并推送 Webhook。
// 这是本系统相对"查一次就完"的备案 API 的差异化能力。
package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/asoe/beian2api/internal/model"
	"github.com/asoe/beian2api/internal/service"
	"github.com/asoe/beian2api/internal/store"
)

// Watcher 是巡检调度器。
type Watcher struct {
	svc      *service.Service
	st       *store.Store
	interval time.Duration
	workers  int
	webhook  string
	client   *http.Client
	log      Logger

	// runMu 串行化巡检：定时器和「立即巡检」按钮可能同时触发，
	// 两轮并发跑会在同一份基线上互相覆盖，变更计数和变更流水都会重复。
	runMu sync.Mutex

	// mu 只保护下面几个统计字段，粒度小，不影响巡检本身。
	mu        sync.Mutex
	running   bool
	lastRun   int64
	lastCount int
	nextRun   int64

	stop chan struct{}
	once sync.Once
}

// fpAbsent 是「确认没有备案」的指纹占位值。
//
// 不能用空串表示：空串在 WatchItem 里代表「还没有基线」。
// 两者混用会导致「备案被注销」永远检测不到——之前就是这个 bug：
// 上游说没有备案时直接 return，既不比对也不记录。
const fpAbsent = "absent"

// Logger 是巡检日志所需的最小接口。
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// Change 是一次变更事件。
type Change struct {
	Domain   string                `json:"domain"`
	At       int64                 `json:"at"`
	ICPCode  string                `json:"icp_code"`
	Kind     string                `json:"kind"` // changed | revoked | registered
	Diff     []service.FieldChange `json:"diff"`
	Previous *model.Record         `json:"previous,omitempty"`
	Current  *model.Record         `json:"current,omitempty"`
}

// New 构造 Watcher。
func New(svc *service.Service, st *store.Store, interval time.Duration, workers int, webhook string, log Logger) *Watcher {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	if workers <= 0 {
		workers = 4
	}
	return &Watcher{
		svc: svc, st: st, interval: interval, workers: workers, webhook: webhook,
		client: &http.Client{Timeout: 10 * time.Second},
		log:    log, stop: make(chan struct{}),
	}
}

// Start 启动后台巡检循环。
//
// 首轮时机要参考上次巡检时间，不能无条件等 1 分钟就开跑：
// 服务重启不应该重置巡检节奏。否则把间隔设成 24h、又每天重启一次的部署，
// 实际会变成「每天重启后准点全量回源」，等于绕过间隔配置去打上游。
func (w *Watcher) Start(ctx context.Context) {
	go func() {
		first := w.firstDelay()
		t := time.NewTimer(first)
		defer t.Stop()
		w.setNext(time.Now().Add(first).Unix())
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.stop:
				return
			case <-t.C:
				w.RunOnce(ctx)
				t.Reset(w.interval)
				w.setNext(time.Now().Add(w.interval).Unix())
			}
		}
	}()
}

// firstDelay 决定启动后第一轮巡检等多久。
func (w *Watcher) firstDelay() time.Duration {
	last := w.st.Stat(metaLastRun)
	if last <= 0 {
		return time.Minute
	}
	due := time.Unix(last, 0).Add(w.interval)
	if d := time.Until(due); d > 0 {
		return d
	}
	// 已经过期：给 30 秒缓冲就补跑，别在启动瞬间和冷启动抢资源。
	return 30 * time.Second
}

// Stop 停止巡检。
func (w *Watcher) Stop() { w.once.Do(func() { close(w.stop) }) }

// RunOnce 立即巡检一轮，返回变更事件。
//
// 串行执行：手动「立即巡检」撞上定时任务时，后到的那轮排队等待，
// 而不是两轮同时基于同一份基线比对——那会把同一次变更记两遍。
func (w *Watcher) RunOnce(ctx context.Context) []Change {
	w.runMu.Lock()
	defer w.runMu.Unlock()
	w.mu.Lock()
	w.running = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	items, err := w.st.ListWatch()
	if err != nil || len(items) == 0 {
		return nil
	}
	jobs := make(chan store.WatchItem)
	changes := make(chan Change, len(items))
	var wg sync.WaitGroup
	for i := 0; i < w.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				if ch, ok := w.check(ctx, it); ok {
					changes <- ch
				}
			}
		}()
	}
	for _, it := range items {
		jobs <- it
	}
	close(jobs)
	wg.Wait()
	close(changes)

	var out []Change
	for c := range changes {
		out = append(out, c)
		w.dispatch(c)
	}
	w.recordRun(len(out))
	if len(out) > 0 {
		w.log.Info("备案巡检发现变更", "count", len(out))
	}
	return out
}

// recordRun 记住这一轮的结束时间与结果条数。
// 同时落进 meta bucket：重启后首页仍能显示「上次巡检」，而不是一片「待首轮」。
func (w *Watcher) recordRun(n int) {
	now := model.Now()
	w.mu.Lock()
	w.lastRun, w.lastCount = now, n
	w.mu.Unlock()
	_ = w.st.SetStat(metaLastRun, now)
	_ = w.st.SetStat(metaLastCount, int64(n))
}

const (
	metaLastRun   = "watch.last_run_at"
	metaLastCount = "watch.last_count"
)

func (w *Watcher) check(ctx context.Context, it store.WatchItem) (Change, bool) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// 必须在查询之前取旧快照：查询会顺带写入新快照，之后就读不到"上一版"了。
	var prev *model.Record
	if hist, _ := w.st.History(it.Domain, 1); len(hist) > 0 {
		p := hist[len(hist)-1]
		prev = &p
	}

	res, err := w.svc.Query(cctx, it.Domain, service.Options{Refresh: true})
	now := model.Now()
	if err != nil {
		it.LastError = err.Error()
		it.CheckedAt = now
		_ = w.st.PutWatch(&it)
		return Change{}, false
	}
	it.CheckedAt = now
	it.LastError = ""

	// 上游明确答复"没有备案"也是一次真实的状态变化：域名可能被注销了。
	// 这是备案巡检最该报出来的一种变更，之前直接 return 掉了。
	if !res.Found || len(res.Records) == 0 {
		if it.LastFP == "" {
			// 首次巡检就没有备案：建立基线，不报警。
			it.LastFP = fpAbsent
			it.LastICP = ""
			_ = w.st.PutWatch(&it)
			return Change{}, false
		}
		if it.LastFP == fpAbsent {
			_ = w.st.PutWatch(&it)
			return Change{}, false
		}
		old := it.LastICP
		it.LastFP = fpAbsent
		it.LastICP = ""
		it.Changes++
		_ = w.st.PutWatch(&it)
		ch := Change{
			Domain: it.Domain, At: now, Kind: "revoked", Previous: prev,
			Diff: []service.FieldChange{
				{Field: "备案状态", From: "已备案" + withParen(old), To: "未查到备案（可能已注销）"},
				{Field: "备案号", From: old, To: ""},
			},
		}
		w.appendChange(ch)
		return ch, true
	}

	cur := res.Records[0]
	fp := cur.Fingerprint()
	it.LastICP = cur.ICPCode

	if it.LastFP == "" {
		it.LastFP = fp
		_ = w.st.PutWatch(&it)
		return Change{}, false
	}
	if it.LastFP == fp {
		_ = w.st.PutWatch(&it)
		return Change{}, false
	}

	kind := "changed"
	if it.LastFP == fpAbsent {
		kind = "registered" // 之前没有备案，现在有了
	}
	it.LastFP = fp
	it.Changes++
	_ = w.st.PutWatch(&it)

	ch := Change{Domain: it.Domain, At: now, ICPCode: cur.ICPCode, Kind: kind, Current: &cur, Previous: prev}
	if kind == "registered" || prev == nil {
		ch.Diff = []service.FieldChange{{Field: "备案号", From: "", To: cur.ICPCode}}
	} else {
		ch.Diff = service.DiffRecords(prev, &cur)
	}
	w.appendChange(ch)
	return ch, true
}

// appendChange 落盘留痕，供 /api/v1/changes 与 RSS 读取。
func (w *Watcher) appendChange(ch Change) {
	id := fmt.Sprintf("%020d-%s", ch.At, ch.Domain)
	if err := w.st.AppendChange(id, ch); err != nil {
		w.log.Warn("记录变更失败", "err", err)
	}
}

func withParen(s string) string {
	if s == "" {
		return ""
	}
	return "（" + s + "）"
}

// dispatch 推送 Webhook（支持通用 JSON 与钉钉/企业微信/飞书文本格式）。
func (w *Watcher) dispatch(c Change) {
	if w.webhook == "" {
		return
	}
	text := fmt.Sprintf("备案%s：%s", kindLabel(c.Kind), c.Domain)
	if c.ICPCode != "" {
		text += "\n备案号：" + c.ICPCode
	}
	for _, d := range c.Diff {
		text += fmt.Sprintf("\n%s：%s → %s", d.Field, emptyDash(d.From), emptyDash(d.To))
	}
	payload := map[string]any{
		"event":    "icp.changed",
		"kind":     c.Kind,
		"domain":   c.Domain,
		"at":       c.At,
		"icp_code": c.ICPCode,
		"diff":     c.Diff,
		"text":     text,
		"msgtype":  "text",
		"content":  map[string]string{"text": text},
		// 钉钉/企业微信兼容字段
		"markdown": map[string]string{"title": "备案变更", "text": text},
		"msg_type": "text",
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	go func() {
		req, err := http.NewRequest(http.MethodPost, w.webhook, bytes.NewReader(b))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := w.client.Do(req)
		if err != nil {
			w.log.Warn("Webhook 推送失败", "err", err)
			return
		}
		resp.Body.Close()
	}()
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Status 是巡检概况。
type Status struct {
	Enabled   bool              `json:"enabled"`
	Interval  int               `json:"interval"`
	Total     int               `json:"total"`
	Running   bool              `json:"running"`
	LastRunAt int64             `json:"last_run_at"`
	LastCount int               `json:"last_count"`
	NextRunAt int64             `json:"next_run_at"`
	Items     []store.WatchItem `json:"items"`
}

// Status 返回巡检概况。
func (w *Watcher) Status(enabled bool) Status {
	items, _ := w.st.ListWatch()
	if items == nil {
		items = []store.WatchItem{}
	}
	w.mu.Lock()
	running, lastRun, lastCount, nextRun := w.running, w.lastRun, w.lastCount, w.nextRun
	w.mu.Unlock()
	if lastRun == 0 {
		// 进程刚起来，回落到持久化记录，别让首页显示成「从没巡检过」。
		lastRun, lastCount = w.st.Stat(metaLastRun), int(w.st.Stat(metaLastCount))
	}
	return Status{
		Enabled: enabled, Interval: int(w.interval / time.Second), Total: len(items),
		Running: running, LastRunAt: lastRun, LastCount: lastCount, NextRunAt: nextRun,
		Items: items,
	}
}

func (w *Watcher) setNext(ts int64) {
	w.mu.Lock()
	w.nextRun = ts
	w.mu.Unlock()
}

func kindLabel(kind string) string {
	switch kind {
	case "revoked":
		return "注销"
	case "registered":
		return "新增"
	default:
		return "变更"
	}
}
