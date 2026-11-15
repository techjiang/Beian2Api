package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 400 不等于「没有备案」。
//
// 这个上游把所有失败都塞进同一个 400，靠 msg 区分原因。之前上层一律当成
// ErrNotFound，于是「凭据写错了」和「这个域名没备案」变成同一件事：
// 结果带 10 分钟负缓存，而巡检会把它读成「备案被注销了」，往群里推
// revoked 告警。凭据填错一次，监控里所有域名同时被判成注销。
func TestAPIHZBadCredentialIsUpstreamErrorNotAbsent(t *testing.T) {
	cases := []string{
		"id参数异常，请检查。接口盒子采用集群服务器，集群节点，企业运营，保障服务的稳定。",
		"通讯秘钥不正确！接口盒子采用集群服务器，集群节点，企业运营，保障服务的稳定。",
		"失败，请重试！",
	}
	for _, msg := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"code":400,"msg":"` + msg + `"}`))
		}))
		p := NewAPIHZ(Options{HTTP: mockClient()})
		p.URL = srv.URL
		_, err := p.Lookup(context.Background(), "example.com")
		if errors.Is(err, ErrNotFound) {
			t.Errorf("msg=%q 被误判成「没有备案」——这会让巡检群发假注销告警", msg)
		}
		if !errors.Is(err, ErrUpstream) {
			t.Errorf("msg=%q 应判为上游错误，实际 %v", msg, err)
		}
		srv.Close()
	}
}

// 反过来：上游明确说没有备案时，必须仍然是 ErrNotFound。
//
// 上面那条修完之后，这里有被顺手一起改成 ErrUpstream 的风险——那会让
// 「没备案」这个结论永远进不了负缓存，每次查询都回源，而巡检再也报不出
// 「域名被注销」。两个方向都得钉住。
func TestAPIHZExplicitAbsentStaysNotFound(t *testing.T) {
	cases := []string{
		"查询失败或没有备案。",
		"该域名未备案",
		"无数据",
	}
	for _, msg := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"code":400,"msg":"` + msg + `"}`))
		}))
		p := NewAPIHZ(Options{HTTP: mockClient()})
		p.URL = srv.URL
		if _, err := p.Lookup(context.Background(), "example.com"); !errors.Is(err, ErrNotFound) {
			t.Errorf("msg=%q 应判为没有备案，实际 %v", msg, err)
		}
		srv.Close()
	}
}
