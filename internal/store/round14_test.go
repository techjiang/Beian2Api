package store

import (
	"testing"
	"time"

	"github.com/asoe/beian2api/internal/model"
)

// 每记一条变更，本小时的 "changes" 计数就要跟着涨。
//
// seriesFields 里一直登记着 "changes"，后台概览的趋势图也画着
// 「变更（底部柱条）」，但全项目没有第二处调 Incr("changes")——
// 于是那排柱条永远是空的。它存在的唯一理由就是回答「什么时候有变动」。
func TestAppendChangeBumpsSeries(t *testing.T) {
	s := open(t)

	for i := 0; i < 3; i++ {
		if err := s.AppendChange(fmtChangeID(int64(i)), map[string]any{"domain": "a.com"}); err != nil {
			t.Fatal(err)
		}
	}
	pts, err := s.Series(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 {
		t.Fatalf("应返回 1 个小时点，实际 %d", len(pts))
	}
	if pts[0].Changes != 3 {
		t.Fatalf("本小时的 changes 应为 3，实际 %d", pts[0].Changes)
	}
	// 计数器本身也要能读到（概览页除了曲线还显示总数）。
	if got := s.Stat("changes"); got != 3 {
		t.Fatalf("changes 计数器应为 3，实际 %d", got)
	}
}

// 删监控项要如实回报「它原本在不在」。
//
// bbolt 的 Delete 对不存在的 key 返回 nil，把 nil 当成功，批量移出就会
// 弹「移出 2 个」而那 2 个域名根本没在列表里。
func TestDeleteWatchIfExists(t *testing.T) {
	s := open(t)

	if err := s.PutWatch(&WatchItem{Domain: "a.com", AddedAt: model.Now()}); err != nil {
		t.Fatal(err)
	}
	existed, err := s.DeleteWatchIfExists("a.com")
	if err != nil || !existed {
		t.Fatalf("删存在的监控项应回 existed=true：%v %v", existed, err)
	}
	existed, err = s.DeleteWatchIfExists("a.com")
	if err != nil || existed {
		t.Fatalf("删不存在的监控项应回 existed=false：%v %v", existed, err)
	}
}

// fmtChangeID 造一个「时间戳-域名」形状的变更 key，和 watch.appendChange 一致。
func fmtChangeID(i int64) string {
	return time.Unix(1700000000+i, 0).UTC().Format("20060102150405") + "-a.com"
}
