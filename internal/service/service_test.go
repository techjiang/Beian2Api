package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asoe/beian2api/internal/config"
	"github.com/asoe/beian2api/internal/model"
	"github.com/asoe/beian2api/internal/provider"
	"github.com/asoe/beian2api/internal/store"
)

type stub struct {
	recs  []model.Record
	err   error
	delay time.Duration
	calls atomic.Int64
}

func (s *stub) Name() string { return "stub" }
func (s *stub) Lookup(ctx context.Context, domain string) ([]model.Record, error) {
	s.calls.Add(1)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.recs, s.err
}

func newSvc(t *testing.T, tasks ...provider.Task) (*Service, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Cache.TTL = 3600
	cfg.Cache.Negative = 60
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close(); os.RemoveAll(dir) })
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, st, tasks, log), st
}

func rec(icp string) model.Record {
	r := model.Record{Domain: "example.com", UnitName: "示例公司", ICPCode: icp, Source: "stub", FetchedAt: model.Now()}
	r.Normalize()
	return r
}

func TestQueryCaches(t *testing.T) {
	s := &stub{recs: []model.Record{rec("京ICP备1号-1")}}
	svc, _ := newSvc(t, provider.Task{Provider: s, Timeout: time.Second})

	r1, err := svc.Query(context.Background(), "example.com", Options{})
	if err != nil || !r1.Found || r1.Cached {
		t.Fatalf("首次查询应回源: %+v %v", r1, err)
	}
	r2, err := svc.Query(context.Background(), "example.com", Options{})
	if err != nil || !r2.Cached {
		t.Fatalf("第二次应命中缓存: %+v %v", r2, err)
	}
	if s.calls.Load() != 1 {
		t.Fatalf("上游只应被调用一次，实际 %d", s.calls.Load())
	}
	r3, _ := svc.Query(context.Background(), "example.com", Options{Refresh: true})
	if r3.Cached || s.calls.Load() != 2 {
		t.Fatalf("refresh 应强制回源，calls=%d", s.calls.Load())
	}
}

func TestQueryNegativeCache(t *testing.T) {
	s := &stub{err: provider.ErrNotFound}
	svc, _ := newSvc(t, provider.Task{Provider: s, Timeout: time.Second})
	r, err := svc.Query(context.Background(), "example.com", Options{})
	if err != nil || r.Found || !r.Cached && r.Source == "" {
		t.Fatalf("无备案应返回 found=false: %+v %v", r, err)
	}
	r2, _ := svc.Query(context.Background(), "example.com", Options{})
	if !r2.Cached {
		t.Fatal("无备案结果也应被短暂缓存")
	}
}

func TestQueryStaleFallback(t *testing.T) {
	s := &stub{recs: []model.Record{rec("京ICP备2号-1")}}
	svc, _ := newSvc(t, provider.Task{Provider: s, Timeout: time.Second})
	if _, err := svc.Query(context.Background(), "example.com", Options{}); err != nil {
		t.Fatal(err)
	}
	// 让缓存过期，并把上游打挂。
	s.err = errors.New("boom")
	s.recs = nil
	svc.tasks[0].Provider = s
	_, _ = svc.st.DeleteCache("example.com")

	// 先手工塞一条过期快照，验证回退路径。
	_ = svc.st.PutCache(&store.Entry{
		Domain: "example.com", Found: true, Records: []model.Record{rec("京ICP备2号-1")},
		Source: "stub", FetchedAt: model.Now() - 100, ExpireAt: model.Now() - 10,
	})
	r, err := svc.Query(context.Background(), "example.com", Options{})
	if err != nil {
		t.Fatalf("上游全挂时应回退快照，实际报错 %v", err)
	}
	if !r.Stale || !r.Found || r.Notice == "" {
		t.Fatalf("应标记 stale 并给出提示: %+v", r)
	}
}

func TestQueryAllFailedNoCache(t *testing.T) {
	s := &stub{err: errors.New("boom")}
	svc, _ := newSvc(t, provider.Task{Provider: s, Timeout: time.Second})
	if _, err := svc.Query(context.Background(), "example.com", Options{}); err == nil {
		t.Fatal("无缓存且上游全挂时应报错")
	}
}

func TestQueryInvalidDomain(t *testing.T) {
	svc, _ := newSvc(t, provider.Task{Provider: &stub{}, Timeout: time.Second})
	for _, d := range []string{"", "abc", "http://", "-x.com"} {
		if _, err := svc.Query(context.Background(), d, Options{}); !errors.Is(err, ErrInvalidDomain) {
			t.Errorf("%q 应被判为非法域名", d)
		}
	}
}

func TestSingleflight(t *testing.T) {
	s := &stub{recs: []model.Record{rec("京ICP备3号-1")}, delay: 120 * time.Millisecond}
	svc, _ := newSvc(t, provider.Task{Provider: s, Timeout: time.Second})

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			if _, err := svc.Query(context.Background(), "example.com", Options{}); err != nil {
				t.Error(err)
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if s.calls.Load() != 1 {
		t.Fatalf("并发重复查询应被合并，上游调用 %d 次", s.calls.Load())
	}
}

func TestHistoryAndDiff(t *testing.T) {
	s := &stub{recs: []model.Record{rec("京ICP备4号-1")}}
	svc, _ := newSvc(t, provider.Task{Provider: s, Timeout: time.Second})
	if _, err := svc.Query(context.Background(), "example.com", Options{}); err != nil {
		t.Fatal(err)
	}
	s.recs = []model.Record{rec("京ICP备4号-2")}
	if _, err := svc.Query(context.Background(), "example.com", Options{Refresh: true}); err != nil {
		t.Fatal(err)
	}
	hist, err := svc.History("example.com", 0)
	if err != nil || len(hist) != 2 {
		// 备案号从 -1 变成 -2 是一次真实变更，两条快照都要留下。
		t.Fatalf("每次变更都应留下历史快照，实际 %d 条 (%v)", len(hist), err)
	}
	if hist[1].ICPCode != "京ICP备4号-2" {
		t.Fatalf("最新快照应在末尾: %+v", hist)
	}
	d := DiffRecords(&model.Record{ICPCode: "a"}, &model.Record{ICPCode: "b"})
	if len(d) != 1 || d[0].Field != "备案号" {
		t.Fatalf("diff 结果不对: %+v", d)
	}
}

func TestRefreshBypassesSingleflight(t *testing.T) {
	// refresh=true 必须真的回源，不能被正在进行的普通查询合并掉。
	s := &stub{recs: []model.Record{rec("京ICP备5号-1")}, delay: 150 * time.Millisecond}
	svc, _ := newSvc(t, provider.Task{Provider: s, Timeout: time.Second})

	go func() { _, _ = svc.Query(context.Background(), "example.com", Options{}) }()
	time.Sleep(20 * time.Millisecond)
	if _, err := svc.Query(context.Background(), "example.com", Options{Refresh: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if s.calls.Load() != 2 {
		t.Fatalf("refresh 应独立回源，上游调用 %d 次", s.calls.Load())
	}
}

func TestRecordHistoryPicksSmallestSiteSeq(t *testing.T) {
	a := rec("京ICP备1号-5")
	b := rec("京ICP备1号-2")
	st := &stub{recs: []model.Record{a, b}}
	svc, _ := newSvc(t, provider.Task{Provider: st, Timeout: time.Second})
	if _, err := svc.Query(context.Background(), "example.com", Options{}); err != nil {
		t.Fatal(err)
	}
	hist, _ := svc.History("example.com", 0)
	if len(hist) != 1 || hist[0].ICPCode != "京ICP备1号-2" {
		t.Fatalf("历史应取站点序号最小的记录，实际 %+v", hist)
	}
}

// 同一个域名的并发请求合并成一次上游调用；但如果发起者自己断连了，
// 不能把「它不等了」变成所有人一起失败。
//
// 这是真实场景：一个用户关掉页面，回源被 ctx 取消，结果写进 call，
// 后面所有等同一个结果的请求拿到的都是 context canceled。
func TestSingleflightLeaderCancelDoesNotPoisonFollowers(t *testing.T) {
	s := &stub{recs: []model.Record{rec("京ICP备1号-1")}, delay: 120 * time.Millisecond}
	svc, _ := newSvc(t, provider.Task{Provider: s, Timeout: time.Second})

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	type res struct {
		r   *model.Result
		err error
	}
	leaderCh := make(chan res, 1)
	go func() {
		r, err := svc.Query(leaderCtx, "example.com", Options{})
		leaderCh <- res{r, err}
	}()

	// 等回源真的开始了，再让 follower 加入同一个单飞。
	time.Sleep(30 * time.Millisecond)
	followerCh := make(chan res, 1)
	go func() {
		r, err := svc.Query(context.Background(), "example.com", Options{})
		followerCh <- res{r, err}
	}()

	time.Sleep(30 * time.Millisecond)
	cancelLeader() // 第一个请求的客户端走了

	lr := <-leaderCh
	if lr.err == nil {
		t.Fatal("发起者自己断连，应该拿到 ctx 错误")
	}

	fr := <-followerCh
	if fr.err != nil {
		t.Fatalf("发起者断连不该连带打掉其他等待者，实际 %v", fr.err)
	}
	if fr.r == nil || !fr.r.Found || fr.r.Records[0].ICPCode != "京ICP备1号-1" {
		t.Fatalf("等待者应拿到正常的查询结果，实际 %+v", fr.r)
	}
	if n := s.calls.Load(); n != 1 {
		t.Fatalf("合并后上游只应被调用一次，实际 %d", n)
	}
}

// 多站点域名：历史快照的代表记录要稳定，且顺序变化不算变更。
func TestRecordHistoryPrimaryIsStable(t *testing.T) {
	multi := []model.Record{rec("粤ICP备1号-3"), rec("粤ICP备1号-1")}
	s := &stub{recs: multi}
	svc, st := newSvc(t, provider.Task{Provider: s, Timeout: time.Second})
	if _, err := svc.Query(context.Background(), "example.com", Options{}); err != nil {
		t.Fatal(err)
	}
	// 上游把顺序倒过来（内容没变），不该再追加一条历史。
	s.recs = []model.Record{multi[1], multi[0]}
	if _, err := svc.Query(context.Background(), "example.com", Options{Refresh: true}); err != nil {
		t.Fatal(err)
	}
	hist, err := st.History("example.com", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 {
		t.Fatalf("顺序变化不该产生新快照，实际 %d 条", len(hist))
	}
	if hist[0].ICPCode != "粤ICP备1号-1" {
		t.Fatalf("应记录站点序号最小的那条，实际 %s", hist[0].ICPCode)
	}
}

// 调用方自己走了，不该拿到一份「上游挂了」的快照。
//
// 这条和 provider 里 Cancelled 的分支是一对：provider 已经把「调用方不等了」
// 从 AllFailed 里摘出来，但 service 在它之前就回退了本地快照——巡检拿到一份
// stale 结果，看到 Found 为真、指纹没变，接着按「stale 计入失败」的规则
// 记一次上游故障。一个完全正常的域名，因为浏览器关了个页面，被推了告警。
func TestCancelledDoesNotReturnStaleSnapshot(t *testing.T) {
	s := &stub{recs: []model.Record{rec("京ICP备1号-1")}, delay: 300 * time.Millisecond}
	svc, st := newSvc(t, provider.Task{Provider: s, Timeout: 5 * time.Second})

	// 先查一次，把一条「已过期但仍在回退窗口内」的快照留在库里。
	if _, err := svc.Query(context.Background(), "example.com", Options{}); err != nil {
		t.Fatal(err)
	}
	e, err := st.GetCache("example.com")
	if err != nil {
		t.Fatal(err)
	}
	e.ExpireAt = model.Now() - 10
	if err := st.PutCache(e); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(40 * time.Millisecond); cancel() }()
	res, err := svc.Query(ctx, "example.com", Options{Refresh: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消应如实返回 ctx 错误，实际 err=%v res=%+v", err, res)
	}
	if res != nil {
		t.Fatalf("取消不该返回任何结果（尤其不该是 stale 快照），实际 %+v", res)
	}
}

// 取消也不该被算进 stale 计数器：那个数是「上游顶不住了」的证据，
// 混进调用方自己走的次数就会失真。
func TestCancelledDoesNotCountStale(t *testing.T) {
	s := &stub{recs: []model.Record{rec("京ICP备1号-1")}, delay: 300 * time.Millisecond}
	svc, st := newSvc(t, provider.Task{Provider: s, Timeout: 5 * time.Second})
	if _, err := svc.Query(context.Background(), "example.com", Options{}); err != nil {
		t.Fatal(err)
	}
	e, _ := st.GetCache("example.com")
	e.ExpireAt = model.Now() - 10
	_ = st.PutCache(e)

	before := st.Stat("stale")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(40 * time.Millisecond); cancel() }()
	_, _ = svc.Query(ctx, "example.com", Options{Refresh: true})
	if got := st.Stat("stale"); got != before {
		t.Fatalf("取消不该计入 stale，%d -> %d", before, got)
	}
}
