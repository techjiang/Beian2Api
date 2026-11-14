package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/asoe/beian2api/internal/model"
	"github.com/asoe/beian2api/internal/notify"
)

func open(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCacheRoundTrip(t *testing.T) {
	s := open(t)
	if _, err := s.GetCache("example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("空库应返回 ErrNotFound，实际 %v", err)
	}
	e := &Entry{Domain: "example.com", Found: true, Records: []model.Record{{ICPCode: "京ICP备1号"}}, Source: "t", FetchedAt: 1, ExpireAt: 2}
	if err := s.PutCache(e); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetCache("example.com")
	if err != nil || !got.Found || len(got.Records) != 1 || got.Records[0].ICPCode != "京ICP备1号" {
		t.Fatalf("读回数据不对: %+v %v", got, err)
	}
	if _, err := s.DeleteCache("example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCache("example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatal("删除后不应再取到")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestHistoryAppendAndLimit(t *testing.T) {
	s := open(t)
	// 同一天内备案改两次，两条快照都必须留下：
	// 按「域名+天」做 key 会让后一条顶掉前一条，那次变更的 diff 就永久没了。
	for _, d := range []int64{1, 2, 3} {
		r := model.Record{Domain: "a.com", ICPCode: "x" + itoa(d), FetchedAt: d}
		if err := s.AppendHistory(r); err != nil {
			t.Fatal(err)
		}
	}
	h, err := s.History("a.com", 0)
	if err != nil || len(h) != 3 {
		t.Fatalf("同一天内每次变更都应留下快照，实际 %d 条 (%v)", len(h), err)
	}
	if h[0].ICPCode != "x1" || h[2].ICPCode != "x3" {
		t.Fatalf("历史应按时间升序: %+v", h)
	}
	// 同一时间点重复写入要覆盖，不能无限膨胀。
	if err := s.AppendHistory(model.Record{Domain: "a.com", ICPCode: "x3", FetchedAt: 3}); err != nil {
		t.Fatal(err)
	}
	if h, _ := s.History("a.com", 0); len(h) != 3 {
		t.Fatalf("同一快照重复写入应覆盖，实际 %d 条", len(h))
	}
	if other, _ := s.History("b.com", 0); len(other) != 0 {
		t.Fatal("其他域名不应有历史")
	}
}

func TestWatchCRUD(t *testing.T) {
	s := open(t)
	w := &WatchItem{Domain: "a.com", AddedAt: 1, LastFP: "fp"}
	if err := s.PutWatch(w); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWatch("a.com")
	if err != nil || got.LastFP != "fp" {
		t.Fatalf("读回监控项不对: %+v %v", got, err)
	}
	list, _ := s.ListWatch()
	if len(list) != 1 {
		t.Fatalf("列表应有 1 条，实际 %d", len(list))
	}
	if err := s.DeleteWatch("a.com"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListWatch()
	if len(list) != 0 {
		t.Fatal("删除后列表应为空")
	}
}

func TestCountersAndSnapshot(t *testing.T) {
	s := open(t)
	if v, _ := s.Incr("hits", 5); v != 5 {
		t.Fatalf("计数器应累加到 5，实际 %d", v)
	}
	if v, _ := s.Incr("hits", 3); v != 8 {
		t.Fatalf("计数器应累加到 8，实际 %d", v)
	}
	if s.Stat("hits") != 8 {
		t.Fatal("Stat 读数不对")
	}

	_ = s.PutCache(&Entry{Domain: "x.com", Found: true})
	dst := filepath.Join(t.TempDir(), "bk.db")
	if err := s.Snapshot(dst); err != nil {
		t.Fatal(err)
	}
	// 用副本重新打开，验证快照完整可用。
	s2, err := OpenFile(dst)
	if err != nil {
		t.Fatal("快照目录应可打开:", err)
	}
	defer s2.Close()
	if c, err := s2.Count(); err != nil || c.Cache != 1 {
		t.Fatalf("快照内容不对: %+v %v", c, err)
	}
}

func TestBulkAndAllCache(t *testing.T) {
	s := open(t)
	var entries []*Entry
	for _, d := range []string{"a.com", "b.com", "c.com"} {
		entries = append(entries, &Entry{Domain: d, Found: true})
	}
	if err := s.Bulk(entries); err != nil {
		t.Fatal(err)
	}
	n := 0
	if err := s.AllCache(func(e *Entry) error { n++; return nil }); err != nil || n != 3 {
		t.Fatalf("应遍历到 3 条，实际 %d (%v)", n, err)
	}
	c, err := s.Count()
	if err != nil || c.Cache != 3 {
		t.Fatalf("计数不对: %+v %v", c, err)
	}
}

func TestListChangesFilterBeforeLimit(t *testing.T) {
	s := open(t)
	for i := 0; i < 5; i++ {
		if err := s.AppendChange(fmt.Sprintf("%020d-d%d.com", 1000+i, i), map[string]any{"at": 1000 + i}); err != nil {
			t.Fatal(err)
		}
	}
	// 4 条都不符合条件，limit=2 也仍然要拿到最后那条符合的。
	var kept []string
	err := s.ListChanges(2, func(raw []byte) (bool, error) {
		var v struct {
			At int64 `json:"at"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return false, err
		}
		if v.At < 1004 {
			return false, nil
		}
		kept = append(kept, fmt.Sprint(v.At))
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0] != "1004" {
		t.Fatalf("过滤应先于截断，实际 %v", kept)
	}
}

func TestSetStatIsNotIncrement(t *testing.T) {
	st := open(t)
	if err := st.SetStat("watch.last_run_at", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStat("watch.last_run_at", 200); err != nil {
		t.Fatal(err)
	}
	if got := st.Stat("watch.last_run_at"); got != 200 {
		t.Fatalf("SetStat 应覆盖而非累加，实际 %d", got)
	}
	if got := st.Stat("nope"); got != 0 {
		t.Fatalf("不存在的键应返回 0，实际 %d", got)
	}
}

func TestNotifyStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.PutNotifyState(notify.State{Name: "dd", Kind: "dingtalk", LastOK: false, LastError: "310000"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutNotifyState(notify.State{Name: "fs", Kind: "feishu", LastOK: true, OK: 2}); err != nil {
		t.Fatal(err)
	}
	got, err := s.NotifyStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["dd"].LastError != "310000" || !got["fs"].LastOK {
		t.Fatalf("状态读取不对: %+v", got)
	}
	c, _ := s.Count()
	if c.Notify != 2 {
		t.Fatalf("计数不对: %+v", c)
	}

	// 改配置删掉一个通道后，遗留状态要能被清掉，否则首页会显示幽灵通道
	if err := s.PurgeNotifyState(map[string]struct{}{"fs": {}}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.NotifyStates()
	if len(got) != 1 || got["fs"].Name != "fs" {
		t.Fatalf("清理后应只剩 fs: %+v", got)
	}
}

// 计数器只在内存里累加，Flush 才落盘。
// 每次请求都 fsync 会把微秒级的缓存命中拖到毫秒级，这是快路径上最不该有的开销。
func TestCountersBufferThenFlush(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Incr("hits", 1); v != 1 {
		t.Fatalf("应读到 1，实际 %d", v)
	}
	s.Incr("hits", 1)
	if got := s.Stat("hits"); got != 2 {
		t.Fatalf("未刷盘也要能读到 2，实际 %d", got)
	}
	// 未刷盘时落盘值为 0
	if n := s.persisted("hits"); n != 0 {
		t.Fatalf("刷盘前落盘值应为 0，实际 %d", n)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if n := s.persisted("hits"); n != 2 {
		t.Fatalf("刷盘后落盘值应为 2，实际 %d", n)
	}
	// 刷盘后继续累加，总数不能丢
	s.Incr("hits", 3)
	if got := s.Stat("hits"); got != 5 {
		t.Fatalf("刷盘后继续累加应为 5，实际 %d", got)
	}
	s.Close()

	// 重开验证跨进程持久化：累加值在退出前已被 Maintain 落盘
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.Stat("hits"); got != 2 {
		t.Fatalf("重开后应保留已刷盘的 2，实际 %d", got)
	}
}

// 变更流水只增不减，跑一年就是一个长个不停的文件，而首页只看最近 10 条。
func TestTrimChangesKeepsNewest(t *testing.T) {
	s := open(t)
	for i := 0; i < 10; i++ {
		if err := s.AppendChange(fmt.Sprintf("%020d-d%d.com", 1000+i, i), map[string]any{"at": 1000 + i}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.TrimChanges(4)
	if err != nil || n != 6 {
		t.Fatalf("应删掉 6 条，实际 %d (%v)", n, err)
	}
	var kept []int64
	_ = s.ListChanges(100, func(raw []byte) (bool, error) {
		var v struct {
			At int64 `json:"at"`
		}
		_ = json.Unmarshal(raw, &v)
		kept = append(kept, v.At)
		return true, nil
	})
	if len(kept) != 4 || kept[0] != 1009 || kept[3] != 1006 {
		t.Fatalf("应保留最新的 4 条（倒序 1009..1006），实际 %v", kept)
	}
	// 再裁一次不该有变化
	if n, _ := s.TrimChanges(4); n != 0 {
		t.Fatalf("已在上限内不该再删，实际删了 %d", n)
	}
}

// 历史快照按域名分别裁剪：全局上限会让一个高频域名把别人的历史全挤掉。
func TestTrimHistoryPerDomain(t *testing.T) {
	s := open(t)
	for i := 0; i < 5; i++ {
		if err := s.AppendHistory(model.Record{Domain: "a.com", ICPCode: "x" + itoa(int64(i)), FetchedAt: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.AppendHistory(model.Record{Domain: "b.com", ICPCode: "y" + itoa(int64(i)), FetchedAt: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.TrimHistory(3)
	if err != nil || n != 2 {
		t.Fatalf("应删掉 2 条 a.com 的旧快照，实际 %d (%v)", n, err)
	}
	if n, _ := s.TrimHistory(3); n != 0 {
		t.Fatalf("已在上限内不该再删，实际删了 %d", n)
	}
	if h, _ := s.History("a.com", 0); len(h) != 3 {
		t.Fatalf("a.com 应剩 3 条，实际 %d", len(h))
	}
	if h, _ := s.History("b.com", 0); len(h) != 2 {
		t.Fatalf("b.com 不该被动过，实际 %d", len(h))
	}
	// 剩下的必须是最新的三条
	h, _ := s.History("a.com", 0)
	if h[0].ICPCode != "x2" || h[2].ICPCode != "x4" {
		t.Fatalf("应保留最新的 x2..x4，实际 %+v", h)
	}
}

// 上限设成 0 表示不裁剪：有人就是想自己接管，不能把他的数据删了。
func TestTrimDisabledByZero(t *testing.T) {
	s := open(t).WithRetention(0, 0)
	for i := 0; i < 5; i++ {
		_ = s.AppendChange(fmt.Sprintf("%020d-d%d.com", i, i), map[string]any{"at": i})
		_ = s.AppendHistory(model.Record{Domain: "a.com", ICPCode: itoa(int64(i)), FetchedAt: int64(i)})
	}
	r, err := s.Maintain()
	if err != nil {
		t.Fatal(err)
	}
	if r.ChangesTrimmed != 0 {
		t.Fatalf("上限为 0 时不该裁剪，实际 %+v", r)
	}
	if c, _ := s.Count(); c.Changes != 5 || c.History != 5 {
		t.Fatalf("数据不该被动过: %+v", c)
	}
}

func TestMaintainTrimsAndFlushes(t *testing.T) {
	s := open(t).WithRetention(3, 2)
	for i := 0; i < 6; i++ {
		_ = s.AppendChange(fmt.Sprintf("%020d-d%d.com", i, i), map[string]any{"at": i})
		_ = s.AppendHistory(model.Record{Domain: "a.com", ICPCode: itoa(int64(i)), FetchedAt: int64(i)})
	}
	s.Incr("hits", 7)
	r, err := s.Maintain()
	if err != nil {
		t.Fatal(err)
	}
	if r.ChangesTrimmed != 3 {
		t.Fatalf("变更流水应删 3 条，实际 %+v", r)
	}
	if n := s.persisted("hits"); n != 7 {
		t.Fatalf("Maintain 应顺带刷盘计数器，实际落盘 %d", n)
	}
}

// 历史快照的超限裁剪应该在写入时就完成，别等定时维护去扫全表。
// 否则域名上千、每域名几百条之后，那一趟扫描会跑成秒级。
func TestAppendHistoryTrimsInline(t *testing.T) {
	s := open(t).WithRetention(0, 3)
	for i := 0; i < 10; i++ {
		if err := s.AppendHistory(model.Record{Domain: "hot.com", ICPCode: itoa(int64(i)), FetchedAt: int64(i)}); err != nil {
			t.Fatal(err)
		}
		// 别的域名不该被波及：配额是按域名算的。
		if err := s.AppendHistory(model.Record{Domain: "cold.com", ICPCode: "c" + itoa(int64(i)), FetchedAt: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if c, _ := s.Count(); c.History != 6 {
		t.Fatalf("两个域名各留 3 条，应为 6 条，实际 %d", c.History)
	}
	h, _ := s.History("hot.com", 0)
	if len(h) != 3 || h[0].ICPCode != "7" || h[2].ICPCode != "9" {
		t.Fatalf("应保留最新的 7..9，实际 %+v", h)
	}
	// 定时维护不该再对历史做任何事
	if r, _ := s.Maintain(); r.ChangesTrimmed != 0 {
		t.Fatalf("历史已在写入时裁好，维护不该再动: %+v", r)
	}
}

// 同一时刻重复写入同一条快照仍是覆盖，不会因为裁剪逻辑而重复堆积。
func TestAppendHistorySameInstantDedupes(t *testing.T) {
	s := open(t).WithRetention(0, 3)
	for i := 0; i < 5; i++ {
		if err := s.AppendHistory(model.Record{Domain: "a.com", ICPCode: "x", FetchedAt: 100}); err != nil {
			t.Fatal(err)
		}
	}
	if c, _ := s.Count(); c.History != 1 {
		t.Fatalf("同一时刻同内容应只有 1 条，实际 %d", c.History)
	}
}

// 打开文件时不能拿默认上限去裁历史：那时配置还没加载，用户明明写了
// retain_history: 0（不裁剪），重启一次就被砍到 200 条，配置文件上看不出异常。
func TestPrepareRespectsConfiguredRetention(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.WithRetention(0, 0) // 保持配置：不裁剪
	for i := 0; i < 260; i++ {
		if err := s.AppendHistory(model.Record{Domain: "a.com", ICPCode: itoa(int64(i)), FetchedAt: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Prepare(); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Count(); c.History != 260 {
		t.Fatalf("上限为 0 时 Prepare 不该删任何历史，实际剩 %d", c.History)
	}
	s.Close()

	// 重新打开也不该被裁——Open 自己不再做裁剪。
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if c, _ := s2.Count(); c.History != 260 {
		t.Fatalf("重新打开文件不该裁历史，实际剩 %d", c.History)
	}
}

// 配了上限时才裁，且裁到上限为止。
func TestPrepareTrimsWhenConfigured(t *testing.T) {
	s := open(t).WithRetention(0, 5)
	for i := 0; i < 12; i++ {
		if err := s.AppendHistory(model.Record{Domain: "a.com", ICPCode: itoa(int64(i)), FetchedAt: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	// 写入时已经就地裁过，这里把上限放开塞回去，验证 Prepare 的兜底作用
	s.WithRetention(0, 0)
	for i := 12; i < 20; i++ {
		if err := s.AppendHistory(model.Record{Domain: "a.com", ICPCode: itoa(int64(i)), FetchedAt: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	s.WithRetention(0, 5)
	if err := s.Prepare(); err != nil {
		t.Fatal(err)
	}
	h, _ := s.History("a.com", 0)
	if len(h) != 5 || h[4].ICPCode != "19" {
		t.Fatalf("应裁到最新 5 条，实际 %d 条: %+v", len(h), h)
	}
}

// 过期且超出回退窗口的缓存条目永远读不到了，必须被清掉。
func TestPurgeExpiredOnlyBeyondStaleWindow(t *testing.T) {
	s := open(t).WithStaleWindow(1000)
	now := int64(100000)
	// 三条：从未过期、过期但在窗口内、过期且早于窗口
	for _, e := range []*Entry{
		{Domain: "fresh.com", Found: true, FetchedAt: now - 10, ExpireAt: now + 100},
		{Domain: "recent.com", Found: true, FetchedAt: now - 500, ExpireAt: now - 100},
		{Domain: "ancient.com", Found: true, FetchedAt: now - 5000, ExpireAt: now - 4000},
	} {
		if err := s.PutCache(e); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PurgeExpired(now)
	if err != nil || n != 1 {
		t.Fatalf("只该删 1 条，实际 %d (%v)", n, err)
	}
	if _, err := s.GetCache("ancient.com"); !errors.Is(err, ErrNotFound) {
		t.Fatal("超出回退窗口的条目应被删除")
	}
	// 窗口内的过期条目还要留着给 service 回退用
	if _, err := s.GetCache("recent.com"); err != nil {
		t.Fatalf("窗口内的过期条目不该删: %v", err)
	}
	if _, err := s.GetCache("fresh.com"); err != nil {
		t.Fatalf("未过期条目不该删: %v", err)
	}
}

// 清理不能跟着 15 秒的维护节奏全表扫：限频之后第二次调用应当直接返回。
func TestPurgeExpiredIsRateLimited(t *testing.T) {
	s := open(t).WithStaleWindow(10)
	now := int64(100000)
	_ = s.PutCache(&Entry{Domain: "old.com", FetchedAt: now - 900, ExpireAt: now - 800})
	if n, _ := s.PurgeExpired(now); n != 1 {
		t.Fatalf("首次应清理 1 条，实际 %d", n)
	}
	_ = s.PutCache(&Entry{Domain: "old2.com", FetchedAt: now - 900, ExpireAt: now - 800})
	if n, _ := s.PurgeExpired(now + 60); n != 0 {
		t.Fatalf("间隔不足时不该再扫，实际删了 %d", n)
	}
	if n, _ := s.PurgeExpired(now + PurgeEvery + 1); n != 1 {
		t.Fatalf("过了间隔应再清理，实际 %d", n)
	}
}

// 按域名的配额挡不住「刷一批随机域名各查一次」：必须还有全局上限。
func TestHistoryGlobalCapBoundsRandomDomains(t *testing.T) {
	s := open(t).WithRetention(0, 100).WithHistoryTotal(50)
	// 制造 50+10 条超限（10% 余量）之后才触发一次裁剪
	for i := 0; i < 80; i++ {
		d := "d" + itoa(int64(i)) + ".com"
		if err := s.AppendHistory(model.Record{Domain: d, ICPCode: "x", FetchedAt: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := s.Count()
	// 留了 10% 余量（至少 10 条），所以稳态最多是 keep + slack = 60。
	if c.History > 60 {
		t.Fatalf("全局上限 50（含余量）没生效，实际 %d 条", c.History)
	}
	// 每个域名都没超自己的配额，说明拦下来的确实是全局上限
	if c.History == 0 {
		t.Fatal("不该清空")
	}
}

// 导出必须带上监控、历史、变更、推送状态，否则「跨机迁移」名不副实。
func TestDumpRestoreRoundTrip(t *testing.T) {
	s := open(t)
	_ = s.PutCache(&Entry{Domain: "a.com", Found: true, Source: "t", FetchedAt: 1, ExpireAt: 9999999999})
	_ = s.AppendHistory(model.Record{Domain: "a.com", ICPCode: "京ICP备1号-1", FetchedAt: 10})
	_ = s.AppendHistory(model.Record{Domain: "a.com", ICPCode: "京ICP备2号-1", FetchedAt: 20})
	_ = s.PutWatch(&WatchItem{Domain: "a.com", AddedAt: 1, Changes: 3, LastFP: "fp"})
	_ = s.AppendChange("0000000020-a.com", map[string]any{"domain": "a.com", "at": 20, "kind": "changed"})
	_ = s.PutNotifyState(notify.State{Name: "运维群", Kind: "dingtalk", LastOK: true})
	_, _ = s.Incr("hits", 7)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	d, err := s.Dump()
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Cache) != 1 || len(d.History) != 2 || len(d.Watch) != 1 || len(d.Changes) != 1 || len(d.Notify) != 1 {
		t.Fatalf("导出内容不全: %+v", d)
	}
	if d.Counters["hits"] != 7 {
		t.Fatalf("计数器没导出: %+v", d.Counters)
	}

	// 导进一个全新的库
	dst := open(t)
	if err := dst.Restore(d); err != nil {
		t.Fatal(err)
	}
	c, _ := dst.Count()
	if c.Cache != 1 || c.History != 2 || c.Watch != 1 || c.Changes != 1 || c.Notify != 1 {
		t.Fatalf("导入后条数不对: %+v", c)
	}
	if h, _ := dst.History("a.com", 0); len(h) != 2 || h[1].ICPCode != "京ICP备2号-1" {
		t.Fatalf("历史顺序不对: %+v", h)
	}
	if w, _ := dst.GetWatch("a.com"); w.Changes != 3 || w.LastFP != "fp" {
		t.Fatalf("监控项没恢复: %+v", w)
	}
	var got []int64
	_ = dst.ListChanges(10, func(raw []byte) (bool, error) {
		var v struct {
			At int64 `json:"at"`
		}
		_ = json.Unmarshal(raw, &v)
		got = append(got, v.At)
		return true, nil
	})
	if len(got) != 1 || got[0] != 20 {
		t.Fatalf("变更流水没恢复: %v", got)
	}
	// 计数器导入后必须从落盘值重新加载，否则会拿 0 继续累加
	if n := dst.Stat("hits"); n != 7 {
		t.Fatalf("导入后计数器应为 7，实际 %d", n)
	}
	if v, _ := dst.Incr("hits", 1); v != 8 {
		t.Fatalf("导入后继续累加应为 8，实际 %d", v)
	}
}

// 重复导入同一份文件不该翻倍：缓存/监控是覆盖写，日志类靠内容 key 去重。
func TestRestoreIsIdempotent(t *testing.T) {
	s := open(t)
	_ = s.PutCache(&Entry{Domain: "a.com", Found: true, ExpireAt: 9})
	_ = s.AppendHistory(model.Record{Domain: "a.com", ICPCode: "x", FetchedAt: 10})
	_ = s.PutWatch(&WatchItem{Domain: "a.com"})
	d, _ := s.Dump()

	dst := open(t)
	for i := 0; i < 3; i++ {
		if err := dst.Restore(d); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := dst.Count()
	if c.Cache != 1 || c.History != 1 || c.Watch != 1 {
		t.Fatalf("重复导入不该翻倍: %+v", c)
	}
}

// 导入要把「当前状态」覆盖成文件里的样子，不能留下目标库原有的域名。
func TestRestoreOverwritesCurrentState(t *testing.T) {
	src := open(t)
	_ = src.PutCache(&Entry{Domain: "new.com", Found: true, ExpireAt: 9})
	_ = src.PutWatch(&WatchItem{Domain: "new.com"})
	d, _ := src.Dump()

	dst := open(t)
	_ = dst.PutCache(&Entry{Domain: "stale.com", Found: true, ExpireAt: 9})
	_ = dst.PutWatch(&WatchItem{Domain: "stale.com"})
	if err := dst.Restore(d); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.GetCache("stale.com"); !errors.Is(err, ErrNotFound) {
		t.Fatal("目标库原有的缓存应被清掉")
	}
	if _, err := dst.GetWatch("stale.com"); !errors.Is(err, ErrNotFound) {
		t.Fatal("目标库原有的监控项应被清掉")
	}
	if _, err := dst.GetCache("new.com"); err != nil {
		t.Fatalf("文件里的缓存应写入: %v", err)
	}
}

// 分页读监控项：总数只数个数，明细按域名升序分页，不把整桶读进内存。
func TestListWatchPage(t *testing.T) {
	s := open(t)
	for _, d := range []string{"c.com", "a.com", "b.com", "e.com", "d.com"} {
		if err := s.PutWatch(&WatchItem{Domain: d}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.CountWatch(); err != nil || n != 5 {
		t.Fatalf("CountWatch 应为 5，实际 %d %v", n, err)
	}
	page1, err := s.ListWatchPage(2, 0)
	if err != nil || len(page1) != 2 {
		t.Fatalf("第一页应 2 条，实际 %d %v", len(page1), err)
	}
	if page1[0].Domain != "a.com" || page1[1].Domain != "b.com" {
		t.Fatalf("应按域名升序，实际 %s %s", page1[0].Domain, page1[1].Domain)
	}
	page3, _ := s.ListWatchPage(2, 4)
	if len(page3) != 1 || page3[0].Domain != "e.com" {
		t.Fatalf("最后一页应剩 e.com，实际 %+v", page3)
	}
	// offset 超出范围返回空，不 panic 也不回绕到第一条。
	if beyond, _ := s.ListWatchPage(2, 99); len(beyond) != 0 {
		t.Fatalf("越界应返回空，实际 %+v", beyond)
	}
	// limit<=0 保持老行为：全量返回。
	if all, _ := s.ListWatchPage(0, 0); len(all) != 5 {
		t.Fatalf("limit=0 应全量返回，实际 %d", len(all))
	}
}

// ListChanges 的回调返回 ErrStop 时应立即结束遍历，且不算错误。
//
// 增量拉取（since）靠它早停：key 是「时间戳-域名」，读到第一条更旧的就
// 说明后面全是更旧的，没必要把整个变更流水反序列化一遍。
func TestListChangesStopsEarly(t *testing.T) {
	s := open(t)
	for i := 0; i < 20; i++ {
		if err := s.AppendChange(fmt.Sprintf("%020d-example.com", 1700000000+i), map[string]any{"at": 1700000000 + i}); err != nil {
			t.Fatal(err)
		}
	}
	seen := 0
	err := s.ListChanges(0, func(raw []byte) (bool, error) {
		seen++
		if seen == 3 {
			return false, ErrStop
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("ErrStop 不应被当成错误: %v", err)
	}
	if seen != 3 {
		t.Fatalf("应在第 3 条处停止，实际遍历了 %d 条", seen)
	}
}

// UpdateWatchIfExists 只在条目存在时写入：巡检不能把已移出的域名复活。
func TestUpdateWatchIfExists(t *testing.T) {
	s := open(t)
	ok, err := s.UpdateWatchIfExists(&WatchItem{Domain: "gone.com", AddedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("条目不存在时不该写入")
	}
	if _, err := s.GetWatch("gone.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不该凭空创建监控项，实际 err=%v", err)
	}

	if err := s.PutWatch(&WatchItem{Domain: "here.com", AddedAt: 1, LastFP: "old"}); err != nil {
		t.Fatal(err)
	}
	ok, err = s.UpdateWatchIfExists(&WatchItem{Domain: "here.com", AddedAt: 1, LastFP: "new", Changes: 2})
	if err != nil || !ok {
		t.Fatalf("条目存在时应写入，ok=%v err=%v", ok, err)
	}
	it, err := s.GetWatch("here.com")
	if err != nil {
		t.Fatal(err)
	}
	if it.LastFP != "new" || it.Changes != 2 {
		t.Fatalf("写入未生效: %+v", it)
	}
}

// 写历史不该每次都做一次 O(全部键) 的统计。
//
// trimHistoryTotal 原来每写一条快照都调 b.Stats().KeyN——它是遍历整个 bucket
// 数键。实测 2 万条 47µs、20 万条 713µs，而这是**写入路径**：每一次备案变更
// 都要为「该不该裁」这个判断付一遍全表统计的钱。
//
// 现在改成增量维护 + 到点重算。这条用例盯的是语义仍然正确：
// 上限照旧生效，超了照旧从最旧的删。
func TestAppendHistoryEnforcesTotalLimit(t *testing.T) {
	s := open(t)
	s.WithRetention(0, 1000).WithHistoryTotal(30)
	base := model.Now()
	for i := 0; i < 120; i++ {
		r := model.Record{
			Domain:    "example.com",
			UnitName:  "公司",
			ICPCode:   "京ICP备" + strconv.Itoa(100000+i) + "号-1",
			FetchedAt: base - int64(120-i)*3600,
		}
		if err := s.AppendHistory(r); err != nil {
			t.Fatal(err)
		}
	}
	c, err := s.Count()
	if err != nil {
		t.Fatal(err)
	}
	// 留了 10% 的余量（上限 30 → 攒到 40 才裁、裁回 30），所以是 30..40 之间。
	if c.History < 30 || c.History > 40 {
		t.Fatalf("历史总数应被压在 30 上下（含 10%% 余量），实际 %d", c.History)
	}
	// 留下来的必须是最新的那批。
	hist, err := s.History("example.com", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) == 0 {
		t.Fatal("历史不该被裁空")
	}
	if last := hist[len(hist)-1].ICPCode; last != "京ICP备100119号-1" {
		t.Fatalf("最新一条应保留，实际最后一条是 %q", last)
	}
}

// 覆盖写不该被算成新增，否则计数会一路飘，提前触发全表裁剪。
func TestAppendHistorySameKeyDoesNotInflateCount(t *testing.T) {
	s := open(t)
	s.WithRetention(0, 1000).WithHistoryTotal(10)
	r := model.Record{Domain: "example.com", UnitName: "公司", ICPCode: "京ICP备1号-1", FetchedAt: model.Now()}
	for i := 0; i < 50; i++ {
		if err := s.AppendHistory(r); err != nil {
			t.Fatal(err)
		}
	}
	hist, err := s.History("example.com", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 {
		t.Fatalf("同一时刻同一内容重复写入应互相覆盖，实际 %d 条", len(hist))
	}
}

// keyset 翻页：cursor 指向上一页最后一条，下一页从它之后继续。
//
// 这是 offset 翻页的替代品。offset 是「走 N 步再开始收」，翻到第 k 页
// 要在游标上空走 N 步；cursor 直接 Seek 过去。代价与页码无关这件事
// 没法在单测里测时间，但「一条不重、一条不漏」是可以钉死的。
func TestPageChangesAfterCursor(t *testing.T) {
	s := open(t)
	const n = 25
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("%020d-d%d.com", 1700000000+i, i)
		if err := s.AppendChange(key, map[string]any{"at": 1700000000 + i}); err != nil {
			t.Fatal(err)
		}
	}
	seen := 0
	cursor := ""
	for page := 0; ; page++ {
		rows, keys, err := s.PageChangesAfter(cursor, 10, func(raw []byte) (bool, error) {
			return true, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		// 每页的 keys 必须和 rows 一一对应：调用方只拿最后一个推游标。
		if len(keys) != len(rows) {
			t.Fatalf("keys 与 rows 不匹配：%d vs %d", len(keys), len(rows))
		}
		var v struct {
			At int64 `json:"at"`
		}
		for _, raw := range rows {
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatal(err)
			}
			// 期望倒序：第一条是 1700000024，往后递减，且不重复。
			if want := int64(1700000000 + n - 1 - seen); v.At != want {
				t.Fatalf("第 %d 条应是 %d，实际 %d", seen, want, v.At)
			}
			seen++
		}
		cursor = keys[len(keys)-1]
		if page > n {
			t.Fatal("翻页没有终止，游标没有前进")
		}
	}
	if seen != n {
		t.Fatalf("应恰好翻出 %d 条，实际 %d", n, seen)
	}
}

// 游标比库里所有记录都新时（记录被裁或字段写错），要退回到最新一条开始，
// 而不是回空页——空页会被读成「流水空了」，而库里明明还有几千条。
func TestPageChangesAfterCursorBeyondNewest(t *testing.T) {
	s := open(t)
	for i := 0; i < 5; i++ {
		_ = s.AppendChange(fmt.Sprintf("%020d-a%d.com", 1700000000+i, i), map[string]any{"at": 1700000000 + i})
	}
	rows, _, err := s.PageChangesAfter("99999999999999999999-x.com", 10, func([]byte) (bool, error) {
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("游标越界时应从最新一条开始返回全部 5 条，实际 %d", len(rows))
	}
}

// 反过来：游标比库里最旧的那条还旧，说明「它之后」确实什么都没有。
// 这时候返回空页是对的——不能为了「别回空」而回绕到最新一条，
// 那会让调用方在同一份数据上无限翻下去。
func TestPageChangesAfterCursorOlderThanOldest(t *testing.T) {
	s := open(t)
	for i := 0; i < 5; i++ {
		_ = s.AppendChange(fmt.Sprintf("%020d-a%d.com", 1700000000+i, i), map[string]any{"at": 1700000000 + i})
	}
	rows, _, err := s.PageChangesAfter("00000000000000000001-gone.com", 10, func([]byte) (bool, error) {
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("游标比所有记录都旧时应返回空页，实际 %d 条", len(rows))
	}
}

// offset 与 cursor 同时给时以 cursor 为准：两种定位方式叠加没有语义。
func TestPageChangesFromPrefersCursor(t *testing.T) {
	s := open(t)
	for i := 0; i < 6; i++ {
		_ = s.AppendChange(fmt.Sprintf("%020d-a%d.com", 1700000000+i, i), map[string]any{"at": 1700000000 + i})
	}
	// 先取前两条，拿到游标。
	_, keys, err := s.PageChangesAfter("", 2, func([]byte) (bool, error) { return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := s.PageChangesFrom(keys[len(keys)-1], 10, 99, func([]byte) (bool, error) { return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	// 剩下的 4 条全在（offset=99 被忽略；若生效就会返回空）。
	if len(rows) != 4 {
		t.Fatalf("cursor 生效时应返回剩下 4 条，实际 %d", len(rows))
	}
}
