package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/asoe/beian2api/internal/model"
)

// fake 是可编排行为的假适配器。
type fake struct {
	name  string
	delay time.Duration
	recs  []model.Record
	err   error
}

func (f *fake) Name() string { return f.name }

func (f *fake) Lookup(ctx context.Context, domain string) ([]model.Record, error) {
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return f.recs, f.err
}

func recs(icp string) []model.Record { return []model.Record{{ICPCode: icp}} }

func TestRaceFastestWins(t *testing.T) {
	tasks := []Task{
		{Provider: &fake{name: "slow", delay: 300 * time.Millisecond, recs: recs("SLOW")}, Timeout: time.Second},
		{Provider: &fake{name: "fast", delay: 10 * time.Millisecond, recs: recs("FAST")}, Timeout: time.Second},
	}
	start := time.Now()
	out := Race(context.Background(), "example.com", tasks)
	if out.Source != "fast" || len(out.Records) != 1 || out.Records[0].ICPCode != "FAST" {
		t.Fatalf("应取最快的数据源，实际 %+v", out)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("拿到结果后应立即返回，耗时 %v", time.Since(start))
	}
}

func TestRaceNotFoundThenData(t *testing.T) {
	// 一个源说"无备案"，另一个稍慢但有数据：必须采信有数据的。
	tasks := []Task{
		{Provider: &fake{name: "nf", delay: 5 * time.Millisecond, err: ErrNotFound}, Timeout: time.Second},
		{Provider: &fake{name: "data", delay: 120 * time.Millisecond, recs: recs("OK")}, Timeout: time.Second},
	}
	out := Race(context.Background(), "example.com", tasks)
	if out.Source != "data" || len(out.Records) != 1 {
		t.Fatalf("应采信有数据的源，实际 %+v", out)
	}
}

func TestRaceAllNotFound(t *testing.T) {
	tasks := []Task{
		{Provider: &fake{name: "a", delay: time.Millisecond, err: ErrNotFound}, Timeout: time.Second},
		{Provider: &fake{name: "b", delay: 2 * time.Millisecond, err: ErrNotFound}, Timeout: time.Second},
	}
	out := Race(context.Background(), "example.com", tasks)
	if out.Source == "" || out.AllFailed || len(out.Records) != 0 {
		t.Fatalf("应得到一个明确的'无备案'结论，实际 %+v", out)
	}
}

func TestRaceAllFailed(t *testing.T) {
	tasks := []Task{
		{Provider: &fake{name: "a", delay: time.Millisecond, err: ErrUpstream}, Timeout: time.Second},
		{Provider: &fake{name: "b", delay: 2 * time.Millisecond, err: errors.New("boom")}, Timeout: time.Second},
	}
	out := Race(context.Background(), "example.com", tasks)
	if !out.AllFailed {
		t.Fatalf("全部失败时应标记 AllFailed，实际 %+v", out)
	}
	if len(out.Errors) != 2 {
		t.Fatalf("应记录两条错误，实际 %v", out.Errors)
	}
}

func TestRaceTimeout(t *testing.T) {
	tasks := []Task{
		{Provider: &fake{name: "hang", delay: 5 * time.Second}, Timeout: 50 * time.Millisecond},
	}
	start := time.Now()
	out := Race(context.Background(), "example.com", tasks)
	if !out.AllFailed {
		t.Fatalf("超时应算失败，实际 %+v", out)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("超时未被及时切断，耗时 %v", time.Since(start))
	}
}

func TestCleanerAndFirstNonEmpty(t *testing.T) {
	if cleaner("查询失败") != "" || cleaner(" 企业 ") != "企业" || cleaner("null") != "" {
		t.Fatal("cleaner 行为不符合预期")
	}
	if firstNonEmpty("", "查询失败", "北京公司") != "北京公司" {
		t.Fatal("firstNonEmpty 应跳过占位符")
	}
}

// 调用方自己取消（浏览器断连、请求超时）不是上游的错。
// 混成 AllFailed 会让巡检把「用户关掉页面」记成一次上游故障，甚至推一条告警。
func TestRaceCallerCancelled(t *testing.T) {
	tasks := []Task{
		{Provider: &fake{name: "slow", delay: 5 * time.Second, recs: recs("X")}, Timeout: 10 * time.Second},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	start := time.Now()
	out := Race(ctx, "example.com", tasks)
	if !out.Cancelled {
		t.Fatalf("应标记为调用方取消: %+v", out)
	}
	if out.AllFailed {
		t.Fatal("取消不该被当成上游全挂")
	}
	// 必须立刻返回，而不是干等到 5 秒的上游超时
	if d := time.Since(start); d > time.Second {
		t.Fatalf("取消后应立刻返回，实际等了 %v", d)
	}
}

// 权重不是装饰：配置里写着「weight 越大越优先」，就得真的优先。
//
// 之前所有源同时起跑、纯看谁快，把官方源权重调到 100、备用源 40 毫无意义。
// 现在低权重源让位一小会儿（对冲，不是排队）：高权重源慢了照样有人接手。
func TestRacePrefersHigherWeight(t *testing.T) {
	defer func(old time.Duration) { hedgeStep = old }(hedgeStep)
	hedgeStep = 120 * time.Millisecond

	// 高权重源慢一点，低权重源很快：让位窗口内不该抢跑。
	tasks := []Task{
		{Provider: &fake{name: "primary", delay: 40 * time.Millisecond, recs: recs("PRIMARY")}, Weight: 100, Timeout: time.Second},
		{Provider: &fake{name: "backup", delay: 5 * time.Millisecond, recs: recs("BACKUP")}, Weight: 20, Timeout: time.Second},
	}
	out := Race(context.Background(), "example.com", tasks)
	if out.Source != "primary" {
		t.Fatalf("高权重源在让位窗口内能给出结果，就该用它的，实际 %q", out.Source)
	}
}

func TestRaceBackupStillTakesOver(t *testing.T) {
	defer func(old time.Duration) { hedgeStep = old }(hedgeStep)
	hedgeStep = 60 * time.Millisecond

	// 高权重源卡死：低权重源延后起跑，但必须接手，不能干等。
	tasks := []Task{
		{Provider: &fake{name: "primary", delay: 5 * time.Second}, Weight: 100, Timeout: 800 * time.Millisecond},
		{Provider: &fake{name: "backup", delay: 5 * time.Millisecond, recs: recs("BACKUP")}, Weight: 10, Timeout: time.Second},
	}
	start := time.Now()
	out := Race(context.Background(), "example.com", tasks)
	if out.Source != "backup" {
		t.Fatalf("高权重源挂了，低权重源应接手，实际 %+v", out)
	}
	if d := time.Since(start); d > 400*time.Millisecond {
		t.Fatalf("不该等满高权重源的超时，耗时 %v", d)
	}
}

func TestHedgeDelay(t *testing.T) {
	defer func(old time.Duration) { hedgeStep = old }(hedgeStep)
	hedgeStep = 200 * time.Millisecond

	cases := []struct {
		weight, max int
		want        time.Duration
	}{
		{100, 100, 0}, // 最高权重立刻出发
		{100, 40, 0},  // 高于最高权重（异常配置）也不该有负延迟
		{0, 100, 0},   // 无权重信息视为不延后
		{50, 100, 100 * time.Millisecond},
		{1, 100, 198 * time.Millisecond},
	}
	for _, c := range cases {
		if got := hedgeDelay(c.weight, c.max); got != c.want {
			t.Fatalf("hedgeDelay(%d,%d) = %v，期望 %v", c.weight, c.max, got, c.want)
		}
	}
	if got := hedgeDelay(100, 0); got != 0 {
		t.Fatalf("没有权重信息时不该延后，实际 %v", got)
	}
}
