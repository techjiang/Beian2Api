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
	if err := s.DeleteCache("example.com"); err != nil {
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
	if r.ChangesTrimmed != 0 || r.HistoryTrimmed != 0 {
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
	if r.ChangesTrimmed != 3 || r.HistoryTrimmed != 4 {
		t.Fatalf("裁剪数量不对: %+v", r)
	}
	if n := s.persisted("hits"); n != 7 {
		t.Fatalf("Maintain 应顺带刷盘计数器，实际落盘 %d", n)
	}
}
