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

	stop chan struct{}
	once sync.Once
}

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
func (w *Watcher) Start(ctx context.Context) {
	go func() {
		// 启动后延迟 1 分钟做首轮，避免和冷启动抢资源。
		t := time.NewTimer(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.stop:
				return
			case <-t.C:
				w.RunOnce(ctx)
				t.Reset(w.interval)
			}
		}
	}()
}

// Stop 停止巡检。
func (w *Watcher) Stop() { w.once.Do(func() { close(w.stop) }) }

// RunOnce 立即巡检一轮，返回变更事件。
func (w *Watcher) RunOnce(ctx context.Context) []Change {
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
	if len(out) > 0 {
		w.log.Info("备案巡检发现变更", "count", len(out))
	}
	return out
}

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
	if !res.Found || len(res.Records) == 0 {
		it.CheckedAt = now
		it.LastError = ""
		_ = w.st.PutWatch(&it)
		return Change{}, false
	}
	cur := res.Records[0]
	fp := cur.Fingerprint()
	it.CheckedAt = now
	it.LastError = ""
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

	it.LastFP = fp
	it.Changes++
	_ = w.st.PutWatch(&it)

	ch := Change{Domain: it.Domain, At: now, ICPCode: cur.ICPCode, Current: &cur, Previous: prev}
	if prev != nil {
		ch.Diff = service.DiffRecords(prev, &cur)
	} else {
		ch.Diff = []service.FieldChange{{Field: "备案号", From: "", To: cur.ICPCode}}
	}
	// 落盘留痕，供 /api/v1/changes 与 RSS 读取。
	id := fmt.Sprintf("%020d-%s", now, it.Domain)
	if err := w.st.AppendChange(id, ch); err != nil {
		w.log.Warn("记录变更失败", "err", err)
	}
	return ch, true
}

// dispatch 推送 Webhook（支持通用 JSON 与钉钉/企业微信/飞书文本格式）。
func (w *Watcher) dispatch(c Change) {
	if w.webhook == "" {
		return
	}
	text := fmt.Sprintf("备案变更：%s\n备案号：%s", c.Domain, c.ICPCode)
	for _, d := range c.Diff {
		text += fmt.Sprintf("\n%s：%s → %s", d.Field, emptyDash(d.From), emptyDash(d.To))
	}
	payload := map[string]any{
		"event":    "icp.changed",
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
	Enabled  bool              `json:"enabled"`
	Interval int               `json:"interval"`
	Total    int               `json:"total"`
	Items    []store.WatchItem `json:"items"`
}

// Status 返回巡检概况。
func (w *Watcher) Status(enabled bool) Status {
	items, _ := w.st.ListWatch()
	if items == nil {
		items = []store.WatchItem{}
	}
	return Status{Enabled: enabled, Interval: int(w.interval / time.Second), Total: len(items), Items: items}
}
