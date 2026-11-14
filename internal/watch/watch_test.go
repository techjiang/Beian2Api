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
