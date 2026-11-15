package store

import (
	"encoding/json"
	"testing"
)

// 导出/导入要带上小时序列。
//
// series 原来不在 Dump 里，理由是「那是观测记录，不是业务数据」。但搬完
// 机器的人打开后台，那张趋势图是唯一一个从零开始的：页面写着「还没有数据」，
// 看起来像功能坏了，其实是这份文件里本来就没有这一段。20KB 换一张迁移后
// 仍然连续的图，值。
func TestDumpCarriesSeries(t *testing.T) {
	src := open(t)
	for i := 0; i < 5; i++ {
		if _, err := src.Incr("hits", 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := src.Incr("misses", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Maintain(); err != nil {
		t.Fatal(err)
	}
	d, err := src.Dump()
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != 3 {
		t.Fatalf("导出格式应为 v3，实际 %d", d.Version)
	}
	if len(d.Series) == 0 {
		t.Fatal("导出里没有 series：迁移后趋势图会是空的")
	}
	if d.Series[len(d.Series)-1].Hits != 5 {
		t.Fatalf("导出的小时序列数值不对: %+v", d.Series)
	}

	dst := open(t)
	if err := dst.Restore(d); err != nil {
		t.Fatal(err)
	}
	got, err := dst.Series(24)
	if err != nil {
		t.Fatal(err)
	}
	last := got[len(got)-1]
	if last.Hits != 5 || last.Misses != 3 {
		t.Fatalf("导入后序列应保留，实际 %+v", last)
	}
}

// 同一小时在两个库里都有数据时取较大值，不是后写的覆盖。
//
// 两台机器各自独立观测到同一个小时，把增量大的那台覆盖掉等于凭空抹掉
// 一段真实流量。
func TestRestoreSeriesMergesByMax(t *testing.T) {
	dst := open(t)
	for i := 0; i < 12; i++ {
		if _, err := dst.Incr("hits", 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dst.Maintain(); err != nil {
		t.Fatal(err)
	}
	hour := dst.seriesHourForTest(t)

	// 另一台机器在同一小时只记了 9 次命中，但记了 2 次回源。
	dump := &Dump{Version: DumpVersion, Series: []SeriesPoint{{Hour: hour, Hits: 9, Misses: 2}}}
	if err := dst.Restore(dump); err != nil {
		t.Fatal(err)
	}
	pts, err := dst.Series(24)
	if err != nil {
		t.Fatal(err)
	}
	cur := pts[len(pts)-1]
	if cur.Hits != 12 {
		t.Fatalf("同一小时应保留较大的增量 12，实际 %d", cur.Hits)
	}
	if cur.Misses != 2 {
		t.Fatalf("另一侧有值的字段应补上，实际 %d", cur.Misses)
	}
}

// seriesHourForTest 读出库里唯一那一行的小时，避免测试自己猜整点算法。
func (s *Store) seriesHourForTest(t *testing.T) int64 {
	t.Helper()
	pts, err := s.Series(24)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pts {
		if p.Hits > 0 {
			return p.Hour
		}
	}
	t.Fatal("库里没有任何序列行")
	return 0
}

// 老导出文件（v2，没有 series）仍然能导入，且不把库里已有的序列清空。
func TestRestoreOldDumpWithoutSeriesKeepsExisting(t *testing.T) {
	dst := open(t)
	for i := 0; i < 4; i++ {
		if _, err := dst.Incr("hits", 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dst.Maintain(); err != nil {
		t.Fatal(err)
	}
	old := &Dump{Version: 2, Cache: []*Entry{{Domain: "a.com", Found: true, ExpireAt: 9}}}
	if err := dst.Restore(old); err != nil {
		t.Fatal(err)
	}
	pts, err := dst.Series(24)
	if err != nil {
		t.Fatal(err)
	}
	if got := pts[len(pts)-1].Hits; got != 4 {
		t.Fatalf("导入 v2 文件不该清掉库里已有的序列，实际 %d", got)
	}
}

// series 必须能完整往返一次 JSON，别在字段标签上漏东西。
func TestDumpSeriesJSONShape(t *testing.T) {
	s := open(t)
	if _, err := s.Incr("hits", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Maintain(); err != nil {
		t.Fatal(err)
	}
	d, err := s.Dump()
	if err != nil {
		t.Fatal(err)
	}
	buf, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var back Dump
	if err := json.Unmarshal(buf, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Series) == 0 || back.Series[0].Hour == 0 {
		t.Fatalf("series 往返后丢了数据: %+v", back.Series)
	}
}
