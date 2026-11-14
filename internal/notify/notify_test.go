package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func sampleEvent() Event {
	return Event{
		Kind: EventChanged, Domain: "example.com", At: 1700000000, ICPCode: "京ICP备2号-1",
		Diff:    []Field{{Field: "备案号", From: "京ICP备1号-1", To: "京ICP备2号-1"}},
		BaseURL: "https://beian.example.com",
	}
}

func capture(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		got = append(got, m)
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestKindInference(t *testing.T) {
	cases := []struct{ kind, url, want string }{
		{"", "https://oapi.dingtalk.com/robot/send?access_token=x", KindDingTalk},
		{"", "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=x", KindWeCom},
		{"", "https://open.feishu.cn/open-apis/bot/v2/hook/x", KindFeishu},
		{"", "https://hooks.slack.com/services/x", KindSlack},
		{"", "https://my-gateway.local/hook", KindGeneric},
		{"Lark", "https://x/y", KindFeishu},
		{"wechat", "https://x/y", KindWeCom},
	}
	for _, c := range cases {
		if got := NormalizeKind(c.kind, c.url); got != c.want {
			t.Fatalf("%s %s => %s, 期望 %s", c.kind, c.url, got, c.want)
		}
	}
}

func TestEachKindHasOwnPayload(t *testing.T) {
	for _, kind := range []string{KindDingTalk, KindWeCom, KindFeishu, KindSlack, KindGeneric} {
		b, err := payload(Channel{Kind: kind}, sampleEvent())
		if err != nil {
			t.Fatalf("%s 载荷构造失败: %v", kind, err)
		}
		if !json.Valid(b) {
			t.Fatalf("%s 载荷不是合法 JSON", kind)
		}
		if !strings.Contains(string(b), "example.com") {
			t.Fatalf("%s 载荷里应包含域名: %s", kind, b)
		}
	}
	// 钉钉必须是 markdown + title（否则客户端不显示标题）
	b, _ := payload(Channel{Kind: KindDingTalk}, sampleEvent())
	if !strings.Contains(string(b), `"msgtype":"markdown"`) || !strings.Contains(string(b), `"title":"备案变更：example.com"`) {
		t.Fatalf("钉钉载荷不符合协议: %s", b)
	}
	// 飞书必须是 interactive 卡片
	b, _ = payload(Channel{Kind: KindFeishu}, sampleEvent())
	if !strings.Contains(string(b), `"msg_type":"interactive"`) {
		t.Fatalf("飞书载荷不符合协议: %s", b)
	}
}

func TestDeliveredToMatchingKinds(t *testing.T) {
	srv, got := capture(t)
	d := New([]Channel{
		{Name: "dd", Kind: KindDingTalk, URL: srv.URL},
		{Name: "we", Kind: KindWeCom, URL: srv.URL},
	}, testLogger()).WithRetries(1, time.Millisecond)
	res := d.Send(context.Background(), sampleEvent())
	if len(res) != 2 || !res[0].OK || !res[1].OK {
		t.Fatalf("两个通道都应成功: %+v", res)
	}
	if len(*got) != 2 {
		t.Fatalf("应收到 2 条请求，实际 %d", len(*got))
	}
	if d.States()["dd"].OK != 1 {
		t.Fatalf("状态未累加: %+v", d.States())
	}
}

// 钉钉/企业微信在 token 失效、关键词不匹配时返回 200，错误藏在 errcode 里。
// 只看状态码会把「一条都没发出去」记成成功。
func TestSemanticErrorIsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errcode":310000,"errmsg":"keywords not in content"}`))
	}))
	defer srv.Close()
	d := New([]Channel{{Name: "dd", Kind: KindDingTalk, URL: srv.URL}}, testLogger()).WithRetries(3, time.Millisecond)
	res := d.Send(context.Background(), sampleEvent())
	if len(res) != 1 || res[0].OK {
		t.Fatalf("200 + errcode!=0 应判定失败: %+v", res)
	}
	if !strings.Contains(res[0].Error, "310000") {
		t.Fatalf("错误信息应带上游 errcode: %s", res[0].Error)
	}
	// 语义错误重试也没用，只该尝试一次
	if res[0].Attempts != 1 {
		t.Fatalf("语义错误不应重试，实际尝试 %d 次", res[0].Attempts)
	}
	st := d.States()["dd"]
	if st.LastOK || st.Failed != 1 {
		t.Fatalf("状态应记为失败: %+v", st)
	}
}

func TestRetriesOnServerError(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"errcode":0}`))
	}))
	defer srv.Close()
	d := New([]Channel{{Name: "g", Kind: KindGeneric, URL: srv.URL}}, testLogger()).WithRetries(3, time.Millisecond)
	res := d.Send(context.Background(), sampleEvent())
	if len(res) != 1 || !res[0].OK || res[0].Attempts != 3 {
		t.Fatalf("5xx 应重试到成功: %+v", res)
	}
}

func TestEventFilter(t *testing.T) {
	srv, got := capture(t)
	d := New([]Channel{
		{Name: "only-revoked", Kind: KindWeCom, URL: srv.URL, Events: []string{EventRevoked}},
	}, testLogger())
	d.Send(context.Background(), sampleEvent()) // changed，应被过滤
	if len(*got) != 0 {
		t.Fatal("未订阅的事件不该推送")
	}
	ev := sampleEvent()
	ev.Kind = EventRevoked
	d.Send(context.Background(), ev)
	if len(*got) != 1 {
		t.Fatal("订阅的事件应推送")
	}
	// 测试事件不受订阅限制，方便验证配置
	d.Send(context.Background(), Event{Kind: EventTest})
	if len(*got) != 2 {
		t.Fatal("测试事件应无视订阅过滤")
	}
}

func TestDisabledAndDuplicateNames(t *testing.T) {
	srv, _ := capture(t)
	d := New([]Channel{
		{Kind: KindGeneric, URL: srv.URL},
		{Kind: KindGeneric, URL: srv.URL},
		{Name: "off", Kind: KindGeneric, URL: srv.URL, Disabled: true},
		{Name: "empty", Kind: KindGeneric, URL: ""},
	}, testLogger())
	chs := d.Channels()
	if len(chs) != 2 {
		t.Fatalf("禁用与空地址应被剔除，实际 %d", len(chs))
	}
	if chs[0].Name != KindGeneric || chs[1].Name != KindGeneric+"-2" {
		t.Fatalf("重名应自动区分: %+v", chs)
	}
}

func TestDingTalkSign(t *testing.T) {
	u := dingTalkSignedURL("https://oapi.dingtalk.com/robot/send?access_token=x", "sec", time.UnixMilli(1700000000000))
	if !strings.Contains(u, "timestamp=1700000000000") || !strings.Contains(u, "&sign=") {
		t.Fatalf("加签参数不对: %s", u)
	}
	if strings.Count(u, "?") != 1 {
		t.Fatalf("已有 query 时应用 & 拼接: %s", u)
	}
	ts, sign := feishuSign("sec", time.Unix(1700000000, 0))
	if ts != "1700000000" || sign == "" {
		t.Fatalf("飞书签名不对: %s %s", ts, sign)
	}
}

func TestOnlyGenericAcceptsEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200) // 自建网关常见：200 + 空 body
	}))
	defer srv.Close()
	d := New([]Channel{
		{Name: "g", Kind: KindGeneric, URL: srv.URL},
		{Name: "dd", Kind: KindDingTalk, URL: srv.URL},
	}, testLogger()).WithRetries(1, time.Millisecond)
	res := d.Send(context.Background(), sampleEvent())
	for _, r := range res {
		if !r.OK {
			t.Fatalf("空 body 的 200 应视为成功: %+v", r)
		}
	}
}
