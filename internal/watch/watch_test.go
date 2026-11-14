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
	"github.com/asoe/beian2api/internal/notify"
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
	return New(svc, st, time.Hour, 2, nil, log), st, sb
}

// testNotifier 构造一个只含单个通道的推送器，重试压到最快，避免测试等退避。
func testNotifier(t *testing.T, url, kind string) *notify.Dispatcher {
	t.Helper()
	d := notify.New([]notify.Channel{{Name: "t", Kind: kind, URL: url}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return d.WithRetries(1, time.Millisecond)
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
	w := New(svc, st, time.Hour, 1, testNotifier(t, srv.URL, "generic"), log).WithBaseURL("https://beian.example.com")

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
	w := New(svc, st, time.Hour, 1, testNotifier(t, "http://127.0.0.1:1/none", "generic"), log)
	_ = st.PutWatch(&store.WatchItem{Domain: "example.com"})
	w.RunOnce(context.Background())
	sb.recs = []model.Record{normalize("京ICP备2号-1", "A")}
	changes := w.RunOnce(context.Background())
	if len(changes) != 1 {
		t.Fatalf("推送失败不应影响变更检测，实际 %d", len(changes))
	}
	w.notifier.Wait()
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

// 变更要按通道类型分别成型，并把详情链接带上：
// 群里看到「备案号变了」却点不进去查，等于只报了一半。
func TestDispatchPerKind(t *testing.T) {
	var mu sync.Mutex
	got := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		if _, ok := m["msgtype"]; ok {
			got["dingtalk"] = m
		} else {
			got["feishu"] = m
		}
		mu.Unlock()
		_, _ = w.Write([]byte(`{"errcode":0,"code":0}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	_ = cfg.Validate()
	st, _ := store.Open(dir)
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sb := &stub{recs: []model.Record{normalize("京ICP备1号-1", "示例公司")}}
	svc := service.New(cfg, st, []provider.Task{{Provider: sb, Timeout: time.Second}}, log)
	d := notify.New([]notify.Channel{
		{Name: "dd", Kind: notify.KindDingTalk, URL: srv.URL},
		{Name: "fs", Kind: notify.KindFeishu, URL: srv.URL},
	}, log).WithRetries(1, time.Millisecond)
	w := New(svc, st, time.Hour, 2, d, log).WithBaseURL("https://beian.example.com")

	_ = st.PutWatch(&store.WatchItem{Domain: "example.com"})
	w.RunOnce(context.Background())
	sb.recs = []model.Record{normalize("京ICP备2号-1", "示例公司")}
	if ch := w.RunOnce(context.Background()); len(ch) != 1 {
		t.Fatalf("应检测到 1 条变更，实际 %d", len(ch))
	}
	d.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("两个通道都应收到消息，实际 %d", len(got))
	}
	dd := got["dingtalk"]
	md, _ := dd["markdown"].(map[string]any)
	if md == nil || !strings.Contains(md["text"].(string), "详情：https://beian.example.com/?domain=example.com") {
		t.Fatalf("钉钉消息应带详情链接: %v", dd)
	}
	fs := got["feishu"]
	if fs["msg_type"] != "interactive" {
		t.Fatalf("飞书应收到卡片消息: %v", fs)
	}
	states := d.States()
	if states["dd"].Failed != 0 || states["dd"].OK != 1 {
		t.Fatalf("投递状态不对: %+v", states)
	}
}

// 推送慢不能拖住巡检：一次巡检要在推送结束前就返回。
func TestDispatchDoesNotBlock(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{"errcode":0}`))
	}))
	defer srv.Close()
	defer close(release)

	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	_ = cfg.Validate()
	st, _ := store.Open(dir)
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sb := &stub{recs: []model.Record{normalize("京ICP备1号-1", "A")}}
	svc := service.New(cfg, st, []provider.Task{{Provider: sb, Timeout: time.Second}}, log)
	d := notify.New([]notify.Channel{{Name: "g", Kind: notify.KindGeneric, URL: srv.URL}}, log)
	w := New(svc, st, time.Hour, 1, d, log)
	_ = st.PutWatch(&store.WatchItem{Domain: "example.com"})
	w.RunOnce(context.Background())
	sb.recs = []model.Record{normalize("京ICP备2号-1", "A")}

	done := make(chan struct{})
	go func() { w.RunOnce(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("推送阻塞了巡检主流程")
	}
}

// 上游连续失败要在第 N 轮报一次，恢复时再报一次，中间不重复刷屏。
func TestUpstreamErrorAlertAndRecover(t *testing.T) {
	w, st, sb := setup(t)
	w.WithFailThreshold(3)
	_ = st.PutWatch(&store.WatchItem{Domain: "example.com"})
	w.RunOnce(context.Background()) // 建立基线

	sb.err = errors.New("dial tcp: i/o timeout")
	for i := 1; i <= 2; i++ {
		if ch := w.RunOnce(context.Background()); len(ch) != 0 {
			t.Fatalf("第 %d 轮未到阈值，不应告警，实际 %d 条", i, len(ch))
		}
	}
	ch := w.RunOnce(context.Background())
	if len(ch) != 1 || ch[0].Kind != EventUpstreamError {
		t.Fatalf("连续 3 轮失败应告警 1 次，实际 %+v", ch)
	}
	// 继续失败也不能每轮都报
	for i := 0; i < 3; i++ {
		if ch := w.RunOnce(context.Background()); len(ch) != 0 {
			t.Fatalf("已告警过不应重复推送，实际 %d 条", len(ch))
		}
	}

	sb.err = nil
	ch = w.RunOnce(context.Background())
	if len(ch) != 1 || ch[0].Kind != EventRecovered {
		t.Fatalf("恢复应报 1 次，实际 %+v", ch)
	}
	if ch := w.RunOnce(context.Background()); len(ch) != 0 {
		t.Fatalf("恢复后不应重复报，实际 %d 条", len(ch))
	}
	item, _ := st.GetWatch("example.com")
	if item.FailStreak != 0 || item.Alerted || item.LastError != "" {
		t.Fatalf("恢复后状态应清零: %+v", item)
	}
}

// 恢复的同时备案也变了：只报一条变更，且带上「已恢复」这一层。
func TestRecoveredWithChange(t *testing.T) {
	w, st, sb := setup(t)
	w.WithFailThreshold(1)
	_ = st.PutWatch(&store.WatchItem{Domain: "example.com"})
	w.RunOnce(context.Background())

	sb.err = errors.New("boom")
	if ch := w.RunOnce(context.Background()); len(ch) != 1 || ch[0].Kind != EventUpstreamError {
		t.Fatalf("应报上游异常，实际 %+v", ch)
	}
	sb.err = nil
	sb.recs = []model.Record{normalize("京ICP备7号-1", "示例公司")}
	ch := w.RunOnce(context.Background())
	if len(ch) != 1 || ch[0].Kind != "changed" {
		t.Fatalf("恢复+变更应只报一条变更，实际 %+v", ch)
	}
	found := false
	for _, d := range ch[0].Diff {
		if d.Field == "查询状态" && d.To == "已恢复" {
			found = true
		}
	}
	if !found {
		t.Fatalf("变更里应带上恢复说明: %+v", ch[0].Diff)
	}
}

// 上游全挂但本地有快照时，service 返回 stale 结果而不是错误。
// 巡检不能把这当成「一切正常」，否则上游连着挂几周都没人知道。
func TestStaleResultTreatedAsFailure(t *testing.T) {
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
	w := New(svc, st, time.Hour, 1, nil, log).WithFailThreshold(1)
	_ = st.PutWatch(&store.WatchItem{Domain: "example.com"})
	w.RunOnce(context.Background()) // 建立基线并写入缓存

	sb.err = errors.New("boom")
	ch := w.RunOnce(context.Background())
	if len(ch) != 1 || ch[0].Kind != EventUpstreamError {
		t.Fatalf("stale 结果应算作上游异常，实际 %+v", ch)
	}
	item, _ := st.GetWatch("example.com")
	if !strings.Contains(item.LastError, "快照") {
		t.Fatalf("应记录快照回退原因: %q", item.LastError)
	}
}

// ctxStub 只在 ctx 取消时如实返回 ctx 错误，模拟真实适配器的行为。
type ctxStub struct{}

func (ctxStub) Name() string { return "ctxstub" }
func (ctxStub) Lookup(ctx context.Context, d string) ([]model.Record, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// 整轮巡检被取消（进程退出、2 分钟窗口到点）不是上游的错。
// 记成故障会让一个完全正常的域名平白多一条「连续失败」，
// 甚至把告警推给运维——而实际上什么都没坏。
func TestCanceledRunNotCountedAsFailure(t *testing.T) {
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
	svc := service.New(cfg, st, []provider.Task{{Provider: ctxStub{}, Timeout: 5 * time.Second}}, log)
	w := New(svc, st, time.Hour, 1, nil, log).WithFailThreshold(1)
	_ = st.PutWatch(&store.WatchItem{Domain: "example.com"})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	ch := w.RunOnce(ctx)
	if len(ch) != 0 {
		t.Fatalf("取消不该产生告警，实际 %+v", ch)
	}
	item, _ := st.GetWatch("example.com")
	if item.FailStreak != 0 || item.LastError != "" {
		t.Fatalf("取消不该被记成失败: %+v", item)
	}
}

// 多站点域名：巡检必须和 service 写入历史时挑同一条代表记录。
//
// 历史按「站点序号最小」记，巡检如果拿 Records[0] 比，同一次真实变更
// 会报成两条不同字段，而且上游列出顺序一变就报出根本不存在的差异。
func TestRunOnceUsesSamePrimaryAsHistory(t *testing.T) {
	w, st, sb := setup(t)
	sb.recs = []model.Record{
		normalize("粤ICP备1号-3", "示例公司"),
		normalize("粤ICP备1号-1", "示例公司"),
	}
	_ = st.PutWatch(&store.WatchItem{Domain: "example.com"})
	if ch := w.RunOnce(context.Background()); len(ch) != 0 {
		t.Fatalf("首轮只建基线，实际 %d 条", len(ch))
	}
	item, _ := st.GetWatch("example.com")
	if item.LastICP != "粤ICP备1号-1" {
		t.Fatalf("基线应取站点序号最小的那条，实际 %s", item.LastICP)
	}

	// 只把上游的列出顺序倒过来，内容一点没变：不该报变更。
	sb.recs = []model.Record{
		normalize("粤ICP备1号-1", "示例公司"),
		normalize("粤ICP备1号-3", "示例公司"),
	}
	if ch := w.RunOnce(context.Background()); len(ch) != 0 {
		t.Fatalf("顺序变化不是变更，实际 %d 条: %+v", len(ch), ch)
	}
}

// 分页状态：总数应当等于真实个数，不是「这一页有多少条」。
func TestStatusPageTotals(t *testing.T) {
	w, st, _ := setup(t)
	for _, d := range []string{"a.com", "b.com", "c.com"} {
		_ = st.PutWatch(&store.WatchItem{Domain: d})
	}
	s := w.StatusPage(true, 2, 0)
	if s.Total != 3 || len(s.Items) != 2 || !s.HasMore {
		t.Fatalf("首页 2 条、总数 3、还有更多: %+v", s)
	}
	s2 := w.StatusPage(true, 2, 2)
	if s2.Total != 3 || len(s2.Items) != 1 || s2.HasMore {
		t.Fatalf("第二页应剩 1 条且没有更多: %+v", s2)
	}
	if w.Total() != 3 {
		t.Fatalf("Total() 应只数个数，实际 %d", w.Total())
	}
}
