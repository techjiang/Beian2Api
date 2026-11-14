package store

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/asoe/beian2api/internal/model"
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

func TestHistoryAppendAndLimit(t *testing.T) {
	s := open(t)
	for _, d := range []int64{1, 2, 3} {
		r := model.Record{Domain: "a.com", ICPCode: "x", FetchedAt: d}
		if err := s.AppendHistory(r); err != nil {
			t.Fatal(err)
		}
	}
	// 同一时间点会覆盖，所以应只有 1 条。
	h, err := s.History("a.com", 0)
	if err != nil || len(h) != 1 {
		t.Fatalf("同日应覆盖，实际 %d 条 (%v)", len(h), err)
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
