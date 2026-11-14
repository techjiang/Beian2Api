package store

import (
	"path/filepath"
	"strconv"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func openSeries(t *testing.T) *Store {
	t.Helper()
	s, err := OpenFile(filepath.Join(t.TempDir(), "series.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func countSeriesRows(t *testing.T, s *Store) int {
	t.Helper()
	n := 0
	if err := s.db.View(func(tx *bolt.Tx) error {
		n = tx.Bucket(bucketSeries).Stats().KeyN
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// 计数器的增量要同时进 meta 和小时序列，两边不能各算各的。
func TestSeriesFollowsCounters(t *testing.T) {
	s := openSeries(t)
	for i := 0; i < 7; i++ {
		if _, err := s.Incr("hits", 1); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Incr("misses", 1); err != nil {
			t.Fatal(err)
		}
	}
	// 不认识的计数器不该污染序列：进序列的只有那几条关键曲线。
	if _, err := s.Incr("something_else", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Maintain(); err != nil {
		t.Fatal(err)
	}
	got, err := s.Series(24)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 24 {
		t.Fatalf("应返回 24 个小时（补零），实际 %d", len(got))
	}
	last := got[len(got)-1]
	if last.Hits != 7 || last.Misses != 3 {
		t.Fatalf("最近一小时应是 7 命中 / 3 未命中，实际 %d / %d", last.Hits, last.Misses)
	}
	if r := last.HitRate(); r < 69 || r > 71 {
		t.Fatalf("命中率应约 70%%，实际 %.1f", r)
	}
	for _, p := range got[:len(got)-1] {
		if p.Total() != 0 {
			t.Fatalf("没数据的小时应补零：%+v", p)
		}
		// 画成 0% 会让人以为命中率掉到零，必须能区分「没请求」和「一个都没命中」。
		if p.HitRate() != -1 {
			t.Fatalf("无请求的小时 HitRate 应为 -1，实际 %.1f", p.HitRate())
		}
	}
}

// 时间轴必须是连续的：缺的小时要补，否则图上的间距不再代表时间。
func TestSeriesFillsGapsAndIsOrdered(t *testing.T) {
	s := openSeries(t)
	got, err := s.Series(6)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("应 6 个点，实际 %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if d := got[i].Hour - got[i-1].Hour; d != 3600 {
			t.Fatalf("相邻两点应正好差 1 小时，实际 %d", d)
		}
	}
	if got[len(got)-1].Hour%3600 != 0 {
		t.Fatal("小时键必须对齐整点")
	}
}

// 还没落盘的增量也要出现在序列里，否则图上「最近一小时」永远是空的。
func TestSeriesIncludesUnflushedPending(t *testing.T) {
	s := openSeries(t)
	if _, err := s.Incr("hits", 4); err != nil {
		t.Fatal(err)
	}
	got, err := s.Series(2)
	if err != nil {
		t.Fatal(err)
	}
	if got[len(got)-1].Hits != 4 {
		t.Fatalf("未落盘的增量应可见，实际 %d", got[len(got)-1].Hits)
	}
}

// 超出窗口的小时要在写入路径上被裁掉，否则序列本身就是另一份只增数据。
func TestSeriesTrimsBeyondWindow(t *testing.T) {
	s := openSeries(t)
	cur := hourKey(nowUnix())
	old := cur - (SeriesRetainHours+10)*3600
	// 直接塞进去，模拟「跑了几天没裁」的老部署。
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSeries).Put(seriesKey(old), []byte(`{"hour":0,"hits":1}`))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Incr("hits", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Maintain(); err != nil {
		t.Fatal(err)
	}
	if err := s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(bucketSeries).Get(seriesKey(old)) != nil {
			t.Fatalf("超窗口的小时 %s 应被裁掉", strconv.FormatInt(old, 10))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n := countSeriesRows(t, s); n != 1 {
		t.Fatalf("只该留下当前这一小时，实际 %d 行", n)
	}
}

// hours 参数要夹在保留窗口内：调用方传 10000 不该让接口返回几万行。
func TestSeriesClampsHours(t *testing.T) {
	s := openSeries(t)
	got, err := s.Series(10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != SeriesRetainHours {
		t.Fatalf("应夹到 %d，实际 %d", SeriesRetainHours, len(got))
	}
	got, err = s.Series(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 24 {
		t.Fatalf("0 应回落到 24，实际 %d", len(got))
	}
}
