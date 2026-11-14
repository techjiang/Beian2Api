package api

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// deadlineWriter 记录 SetWriteDeadline 的调用，用来验证长任务真的放开了写超时。
type deadlineWriter struct {
	hdr  http.Header
	set  []time.Time
	code int
}

func (w *deadlineWriter) Header() http.Header {
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}

func (w *deadlineWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *deadlineWriter) WriteHeader(c int)           { w.code = c }
func (w *deadlineWriter) SetWriteDeadline(t time.Time) error {
	w.set = append(w.set, t)
	return nil
}

// 长任务接口（立即巡检、在线备份、批量查询、推送测试）必须能把写超时往后推，
// 否则 http.Server 的 30 秒 WriteTimeout 会在任务算完之前掐断连接：
// 服务端日志一片成功，调用方看到的是「连接被重置」。
func TestExtendWriteDeadlineMovesDeadlineAhead(t *testing.T) {
	w := &deadlineWriter{}
	restore := extendWriteDeadline(w, 10*time.Minute)
	if len(w.set) != 1 {
		t.Fatalf("应设置一次写超时，实际 %d 次", len(w.set))
	}
	if d := time.Until(w.set[0]); d < 9*time.Minute {
		t.Fatalf("应放宽到约 10 分钟后，实际 %v", d)
	}
	restore()
	if len(w.set) != 2 {
		t.Fatalf("退出时应恢复写超时，实际 %d 次", len(w.set))
	}
	if d := time.Until(w.set[1]); d > time.Minute {
		t.Fatalf("恢复后的超时应回到常规值，实际 %v", d)
	}
}

// 包装层必须实现 Unwrap，否则 ResponseController 摸不到真实连接，
// SetWriteDeadline 静默失效——那一行代码看着在，实际一点用没有。
func TestStatusWriterUnwraps(t *testing.T) {
	inner := &deadlineWriter{}
	sw := &statusWriter{ResponseWriter: inner, code: 200}
	var w http.ResponseWriter = sw
	if got := w.(interface{ Unwrap() http.ResponseWriter }).Unwrap(); got != inner {
		t.Fatalf("Unwrap 应返回被包装的 ResponseWriter")
	}
	rc := http.NewResponseController(sw)
	if err := rc.SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("ResponseController 应能穿透包装层: %v", err)
	}
	if len(inner.set) != 1 {
		t.Fatalf("写超时应落到真实 ResponseWriter 上，实际 %d 次", len(inner.set))
	}
}

// 批量查询的窗口要随域名数放大，但必须有上限。
func TestBatchWindowScalesWithCount(t *testing.T) {
	if d := batchWindow(1); d != 13*time.Second {
		t.Fatalf("1 个域名应约 13 秒，实际 %v", d)
	}
	if d := batchWindow(50); d < 80*time.Second || d > 2*time.Minute {
		t.Fatalf("50 个域名应约 85 秒，实际 %v", d)
	}
	if d := batchWindow(5000); d != 2*time.Minute {
		t.Fatalf("超大数量应被截到 2 分钟，实际 %v", d)
	}
}

// 端到端：真的起一个 WriteTimeout=400ms 的服务，让 handler 睡 900ms 再写响应。
// 不放宽超时的话，客户端拿到的是 EOF / 空响应；放宽之后能正常收到 JSON。
// 这条用真实连接跑，因为 SetWriteDeadline 只在真实 net.Conn 上才有意义。
func TestExtendWriteDeadlineSurvivesRealWriteTimeout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/long", func(w http.ResponseWriter, r *http.Request) {
		defer extendWriteDeadline(w, 5*time.Second)()
		w.Header().Set("Content-Type", "application/json")
		time.Sleep(900 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/short", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(900 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, WriteTimeout: 400 * time.Millisecond}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	fetch := func(path string) (string, error) {
		c := &http.Client{Timeout: 5 * time.Second}
		resp, err := c.Get("http://" + ln.Addr().String() + path)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return string(b), err
	}

	// 对照组：不放宽超时，响应写不回去。
	if body, err := fetch("/short"); err == nil && body == `{"ok":true}` {
		t.Fatal("对照组应当在 400ms 写超时处失败，否则这个用例证明不了什么")
	}
	// 正式用例：放宽之后必须拿到完整响应。
	body, err := fetch("/long")
	if err != nil {
		t.Fatalf("放宽写超时后不该失败: %v", err)
	}
	if body != `{"ok":true}` {
		t.Fatalf("应收到完整响应，实际 %q", body)
	}
}
