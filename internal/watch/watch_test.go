package watch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asoe/beian2api/internal/config"
	"github.com/asoe/beian2api/internal/model"
	"github.com/asoe/beian2api/internal/provider"
	"github.com/asoe/beian2api/internal/service"
	"github.com/asoe/beian2api/internal/store"
)

type stub struct {
	recs []model.Record
	err  error
}

func (s *stub) Name() string { return "stub" }
func (s *stub) Lookup(ctx context.Context, d string) ([]model.Record, error) {
	return s.recs, s.err
}

func setup(t *testing.T) (*Watcher, *store.Store, *stub) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sb := &stub{recs: []model.Record{normalize("京ICP备1号-1", "示例公司")}}
	svc := service.New(cfg, st, []provider.Task{{Provider: sb, Timeout: time.Second}}, log)
	return New(svc, st, time.Hour, 2, "", log), st, sb
}

func normalize(icp, unit string) model.Record {
	r := model.Record{Domain: "example.com", UnitName: unit, ICPCode: icp, Source: "stub", FetchedAt: model.Now()}
	r.Normalize()
	return r
}

func TestRunOnceDetectsChange(t *testing.T) {
	w, st, sb := setup(t)
	// 建立基线
	first := w.RunOnce(context.Background())
	if len(first) != 0 {
		t.Fatal("首轮无监控项，不应有变更")
	}
	svcItem := &store.WatchItem{Domain: "example.com", AddedAt: model.Now()}
	if err := st.PutWatch(svcItem); err != nil {
		t.Fatal(err)
	}
	if ch := w.RunOnce(context.Background()); len(ch) != 0 {
		t.Fatalf("首轮巡检只应建立基线，实际 %d 条变更", len(ch))
	}
	// 上游备案号变了
	sb.recs = []model.Record{normalize("京ICP备2号-1", "示例公司")}
	ch := w.RunOnce(context.Background())
	if len(ch) != 1 {
		t.Fatalf("应检测到 1 条变更，实际 %d", len(ch))
	}
	if ch[0].Domain != "example.com" || len(ch[0].Diff) == 0 {
		t.Fatalf("变更内容不对: %+v", ch[0])
	}
	item, err := st.GetWatch("example.com")
	if err != nil || item.Changes != 1 {
		t.Fatalf("变更计数不对: %+v %v", item, err)
	}
	// 再跑一轮，内容没变就不该再报
	if ch := w.RunOnce(context.Background()); len(ch) != 0 {
		t.Fatalf("内容未变不应重复报警，实际 %d", len(ch))
	}
}

func TestRunOnceUpstreamError(t *testing.T) {
	w, st, sb := setup(t)
	_ = st.PutWatch(&store.WatchItem{Domain: "example.com"})
	sb.err = errors.New("boom")
	if ch := w.RunOnce(context.Background()); len(ch) != 0 {
		t.Fatal("上游失败不应产生变更")
	}
	item, _ := st.GetWatch("example.com")
	if item.LastError == "" {
		t.Fatal("应记录错误信息")
	}
}

func TestStatus(t *testing.T) {
	w, st, _ := setup(t)
	_ = st.PutWatch(&store.WatchItem{Domain: "a.com"})
	s := w.Status(true)
	if !s.Enabled || s.Total != 1 || len(s.Items) != 1 {
		t.Fatalf("状态不对: %+v", s)
	}
}

func TestWebhookDelivery(t *testing.T) {
	got := make(chan map[string]any, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("webhook 载荷不是 JSON: %v", err)
		}
		got <- payload
		w.WriteHeader(200)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sb := &stub{recs: []model.Record{normalize("京ICP备1号-1", "示例公司")}}
	svc := service.New(cfg, st, []provider.Task{{Provider: sb, Timeout: time.Second}}, log)
	w := New(svc, st, time.Hour, 1, srv.URL, log)

	_ = st.PutWatch(&store.WatchItem{Domain: "example.com"})
	w.RunOnce(context.Background()) // 建立基线

	sb.recs = []model.Record{normalize("京ICP备2号-1", "示例公司")}
	changes := w.RunOnce(context.Background())
	if len(changes) != 1 {
		t.Fatalf("应检测到 1 条变更，实际 %d", len(changes))
	}

	select {
	case p := <-got:
		if p["event"] != "icp.changed" || p["domain"] != "example.com" {
			t.Fatalf("webhook 载荷不对: %v", p)
		}
		if !strings.Contains(p["text"].(string), "备案号") {
			t.Fatalf("text 应包含变更说明: %v", p["text"])
		}
		// 兼容钉钉/企业微信的字段
		if _, ok := p["msgtype"]; !ok {
			t.Fatal("缺少 IM 兼容字段 msgtype")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("webhook 未被调用")
	}
}

func TestWebhookUnreachable(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	_ = cfg.Validate()
	st, _ := store.Open(dir)
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sb := &stub{recs: []model.Record{normalize("京ICP备1号-1", "A")}}
	svc := service.New(cfg, st, []provider.Task{{Provider: sb, Timeout: time.Second}}, log)
	// 指向一个必然失败的地址，验证不会 panic、主流程照常返回
	w := New(svc, st, time.Hour, 1, "http://127.0.0.1:1/none", log)
	_ = st.PutWatch(&store.WatchItem{Domain: "example.com"})
	w.RunOnce(context.Background())
	sb.recs = []model.Record{normalize("京ICP备2号-1", "A")}
	changes := w.RunOnce(context.Background())
	if len(changes) != 1 {
		t.Fatalf("推送失败不应影响变更检测，实际 %d", len(changes))
	}
	time.Sleep(200 * time.Millisecond)
}

// 备案被注销也是变更：上游从"有记录"变成"明确无备案"。
// 之前这条路径直接 return，导致最该报警的变更永远检测不到。
func TestRunOnceDetectsRevoked(t *testing.T) {
	w, st, sb := setup(t)
	_ = st.PutWatch(&store.WatchItem{Domain: "example.com"})
	if ch := w.RunOnce(context.Background()); len(ch) != 0 {
		t.Fatalf("首轮只应建立基线，实际 %d", len(ch))
	}
	baseline, _ := st.GetWatch("example.com")
	if baseline.LastICP == "" {
		t.Fatal("基线应记下备案号")
	}

	sb.recs = nil // 上游明确表示无备案
	ch := w.RunOnce(context.Background())
	if len(ch) != 1 {
		t.Fatalf("注销应报 1 条变更，实际 %d", len(ch))
	}
	if ch[0].Kind != "revoked" || ch[0].ICPCode != "" {
		t.Fatalf("变更类型不对: %+v", ch[0])
	}
	if len(ch[0].Diff) == 0 || ch[0].Diff[0].To == "" {
		t.Fatalf("应说明状态变化: %+v", ch[0].Diff)
	}
	item, _ := st.GetWatch("example.com")
	if item.Changes != 1 || item.LastICP != "" {
		t.Fatalf("监控项状态不对: %+v", item)
	}

	// 持续无备案不该反复报警
	if ch := w.RunOnce(context.Background()); len(ch) != 0 {
		t.Fatalf("未再变化不应重复报警，实际 %d", len(ch))
	}

	// 域名重新备案回来，应作为新增上报
	sb.recs = []model.Record{normalize("京ICP备3号-1", "示例公司")}
	ch = w.RunOnce(context.Background())
	if len(ch) != 1 || ch[0].Kind != "registered" {
		t.Fatalf("重新备案应报新增，实际 %+v", ch)
	}
}

// 首次巡检就没有备案：只建立基线，不能凭空报一条注销。
func TestRunOnceFirstSeenNotFounded(t *testing.T) {
	w, st, sb := setup(t)
	sb.recs = nil
	_ = st.PutWatch(&store.WatchItem{Domain: "example.com"})
	if ch := w.RunOnce(context.Background()); len(ch) != 0 {
		t.Fatalf("首次无备案不应报警，实际 %d", len(ch))
	}
	item, _ := st.GetWatch("example.com")
	if item.LastFP != fpAbsent {
		t.Fatalf("基线应记为无备案: %+v", item)
	}
}

// 巡检串行：并发的两轮不会把同一次变更记两遍。
func TestRunOnceSerialized(t *testing.T) {
	w, st, sb := setup(t)
	_ = st.PutWatch(&store.WatchItem{Domain: "example.com"})
	w.RunOnce(context.Background())
	sb.recs = []model.Record{normalize("京ICP备9号-1", "示例公司")}

	var total int
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := len(w.RunOnce(context.Background()))
			mu.Lock()
			total += n
			mu.Unlock()
		}()
	}
	wg.Wait()
	if total != 1 {
		t.Fatalf("同一次变更只应上报 1 次，实际 %d", total)
	}
	n := 0
	_ = st.ListChanges(100, func([]byte) (bool, error) { n++; return true, nil })
	if n != 1 {
		t.Fatalf("变更流水应只有 1 条，实际 %d", n)
	}
}

func TestStatusRunInfo(t *testing.T) {
	w, st, _ := setup(t)
	_ = st.PutWatch(&store.WatchItem{Domain: "a.com"})
	w.setNext(1234)
	w.RunOnce(context.Background())
	s := w.Status(true)
	if s.LastRunAt == 0 || s.NextRunAt != 1234 || s.Running {
		t.Fatalf("运行信息不对: %+v", s)
	}
}

// 重启不应该重置巡检节奏：间隔 24h、上次 1 小时前巡检过，首轮应等到 23 小时后。
func TestFirstDelayRespectsLastRun(t *testing.T) {
	w, st, _ := setup(t)
	w.interval = 24 * time.Hour
	if d := w.firstDelay(); d != time.Minute {
		t.Fatalf("从没巡检过时应 1 分钟后首轮，实际 %v", d)
	}
	_ = st.SetStat(metaLastRun, model.Now()-3600)
	d := w.firstDelay()
	if d < 22*time.Hour || d > 23*time.Hour+time.Minute {
		t.Fatalf("应等到约 23 小时后，实际 %v", d)
	}
	// 已经到期（比如停机两天），尽快补跑但不立即
	_ = st.SetStat(metaLastRun, model.Now()-2*86400)
	if d := w.firstDelay(); d != 30*time.Second {
		t.Fatalf("过期后应 30 秒补跑，实际 %v", d)
	}
}
