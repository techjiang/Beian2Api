package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asoe/beian2api/internal/model"
)

func mockClient() *http.Client { return &http.Client{Timeout: 3 * time.Second} }

func TestAPIHZHHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("domain") != "example.com" {
			t.Errorf("domain 参数没传对: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200, "icp": "京ICP备12345678号-1", "unit": "示例公司",
			"type": "企业", "domain": "example.com", "time": "2024-01-01", "td": "1-1",
		})
	}))
	defer srv.Close()

	p := NewAPIHZ(Options{HTTP: mockClient()})
	p.URL = srv.URL
	recs, err := p.Lookup(context.Background(), "example.com")
	if err != nil || len(recs) != 1 {
		t.Fatalf("查询失败: %v %v", recs, err)
	}
	r := recs[0]
	if r.ICPCode != "京ICP备12345678号-1" || r.UnitName != "示例公司" || r.Province != "京" {
		t.Fatalf("字段解析不对: %+v", r)
	}
	if r.Nature != "企业" {
		t.Fatalf("备案性质应翻译成中文，实际 %q", r.Nature)
	}
	if r.Source != "apihz" {
		t.Fatalf("来源标记不对: %s", r.Source)
	}
}

func TestAPIHZNotFoundCases(t *testing.T) {
	cases := []string{
		`{"code":400,"msg":"查询失败或没有备案。"}`,
		`{"code":200,"icp":"查询失败","unit":"查询失败","time":"查询失败"}`,
		`{"code":200,"icp":null}`,
	}
	for _, body := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		p := NewAPIHZ(Options{HTTP: mockClient()})
		p.URL = srv.URL
		if _, err := p.Lookup(context.Background(), "example.com"); !errors.Is(err, ErrNotFound) {
			t.Errorf("响应 %s 应判为无备案，实际 %v", body, err)
		}
		srv.Close()
	}
}

func TestAPIHZUpstreamErrors(t *testing.T) {
	// 非法 JSON
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>blocked</html>"))
	}))
	p := NewAPIHZ(Options{HTTP: mockClient()})
	p.URL = srv.URL
	if _, err := p.Lookup(context.Background(), "example.com"); !errors.Is(err, ErrUpstream) {
		t.Fatalf("非 JSON 应判为上游错误，实际 %v", err)
	}
	srv.Close()

	// 500 与不可达
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"code":500,"msg":"err"}`))
	}))
	p.URL = srv2.URL
	if _, err := p.Lookup(context.Background(), "example.com"); !errors.Is(err, ErrUpstream) {
		t.Fatalf("code 500 应判为上游错误，实际 %v", err)
	}
	srv2.Close()

	p.URL = "http://127.0.0.1:1/none"
	if _, err := p.Lookup(context.Background(), "example.com"); !errors.Is(err, ErrUpstream) {
		t.Fatalf("连接失败应判为上游错误，实际 %v", err)
	}
}

func TestMIITHappyPath(t *testing.T) {
	var authed bool
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("认证应为 POST，实际 %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("authKey") == "" || r.Form.Get("timeStr") == "" {
			t.Error("缺少 authKey/timeStr")
		}
		authed = true
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true, "params": map[string]string{"bussiness": "tok-123"},
		})
	})
	mux.HandleFunc("/query", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Token") != "tok-123" {
			t.Errorf("Token 头不对: %q", r.Header.Get("Token"))
		}
		if !authed {
			t.Error("应先认证再查询")
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["unitName"] != "example.com" {
			t.Errorf("查询参数不对: %v", req)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"params": map[string]any{
				"total": 2,
				"list": []map[string]any{
					{"domain": "example.com", "unitName": "示例公司", "unitType": "企业",
						"serviceName": "示例站", "serviceLicence": "京ICP备12345678号-1",
						"natureName": "1-1", "updateRecordTime": "2024-01-01"},
					// 干扰项：模糊匹配带出来的其他域名，必须被过滤掉
					{"domain": "not-example.com", "serviceLicence": "京ICP备99999999号-1"},
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := NewMIIT(Options{HTTP: mockClient()})
	m.authURL = srv.URL + "/auth"
	m.queryURL = srv.URL + "/query"

	recs, err := m.Lookup(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("应过滤掉非本域名的结果，实际 %d 条", len(recs))
	}
	if recs[0].ICPCode != "京ICP备12345678号-1" || recs[0].Nature != "企业" {
		t.Fatalf("字段不对: %+v", recs[0])
	}

	// token 应被缓存，第二次查询不再认证
	before := authed
	if _, err := m.Lookup(context.Background(), "example.com"); err != nil {
		t.Fatal(err)
	}
	if authed != before {
		t.Fatal("token 应被复用")
	}
}

func TestMIITAuthFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>WAF</html>"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := NewMIIT(Options{HTTP: mockClient()})
	m.authURL = srv.URL + "/auth"
	m.queryURL = srv.URL + "/query"
	_, err := m.Lookup(context.Background(), "example.com")
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("WAF 拦截 HTML 应判为上游错误，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "防火墙") {
		t.Fatalf("错误信息应说明可能被拦截: %v", err)
	}
}

func TestMIITQueryResponses(t *testing.T) {
	newSrv := func(queryBody string) *httptest.Server {
		mux := http.NewServeMux()
		mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true, "params": map[string]string{"bussiness": "t"},
			})
		})
		mux.HandleFunc("/query", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(queryBody))
		})
		return httptest.NewServer(mux)
	}

	// 空列表 -> 无备案
	s1 := newSrv(`{"success":true,"params":{"total":0,"list":[]}}`)
	m := NewMIIT(Options{HTTP: mockClient()})
	m.authURL, m.queryURL = s1.URL+"/auth", s1.URL+"/query"
	if _, err := m.Lookup(context.Background(), "example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("空列表应判为无备案，实际 %v", err)
	}
	s1.Close()

	// 明确的"没有"文案
	s2 := newSrv(`{"success":false,"msg":"没有查询到相关记录"}`)
	m2 := NewMIIT(Options{HTTP: mockClient()})
	m2.authURL, m2.queryURL = s2.URL+"/auth", s2.URL+"/query"
	if _, err := m2.Lookup(context.Background(), "example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("明确无记录应判为 ErrNotFound，实际 %v", err)
	}
	s2.Close()

	// 非 JSON
	s3 := newSrv(`<html></html>`)
	m3 := NewMIIT(Options{HTTP: mockClient()})
	m3.authURL, m3.queryURL = s3.URL+"/auth", s3.URL+"/query"
	if _, err := m3.Lookup(context.Background(), "example.com"); !errors.Is(err, ErrUpstream) {
		t.Errorf("非 JSON 应判为上游错误，实际 %v", err)
	}
	s3.Close()

	// 401 应清空 token
	s4 := newSrv(`{"success":true,"params":{"list":[]}}`)
	m4 := NewMIIT(Options{HTTP: mockClient()})
	m4.authURL, m4.queryURL = s4.URL+"/auth", s4.URL+"/query"
	s4.Close()

	// 认证成功但没给 token
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "params": map[string]string{}})
	})
	s5 := httptest.NewServer(mux)
	defer s5.Close()
	m5 := NewMIIT(Options{HTTP: mockClient()})
	m5.authURL, m5.queryURL = s5.URL+"/auth", s5.URL+"/query"
	if _, err := m5.Lookup(context.Background(), "example.com"); !errors.Is(err, ErrUpstream) {
		t.Errorf("没有 token 应判为上游错误，实际 %v", err)
	}
}

func TestMockProvider(t *testing.T) {
	m := NewMock(Options{})
	recs, err := m.Lookup(context.Background(), "demo.com")
	if err != nil || len(recs) != 1 {
		t.Fatalf("演示数据源应返回一条记录: %v %v", recs, err)
	}
	if recs[0].ICPCode == "" || recs[0].Source != "mock" {
		t.Fatalf("演示记录不完整: %+v", recs[0])
	}
	// 同域名必须稳定生成同样的数据
	again, _ := m.Lookup(context.Background(), "demo.com")
	if again[0].ICPCode != recs[0].ICPCode {
		t.Fatal("演示数据必须对同一域名保持稳定")
	}
	if _, err := m.Lookup(context.Background(), "x.test"); !errors.Is(err, ErrNotFound) {
		t.Fatal(".test 域名应返回无备案")
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	names := r.Names()
	if len(names) < 3 {
		t.Fatalf("应内置至少 3 个适配器，实际 %v", names)
	}
	if _, err := r.Build("nope", Options{}); err == nil {
		t.Fatal("未知适配器应报错")
	}
	for _, n := range names {
		p, err := r.Build(n, Options{Timeout: time.Second, HTTP: mockClient()})
		if err != nil || p.Name() == "" {
			t.Fatalf("构建 %s 失败: %v", n, err)
		}
	}
}

func TestNormalizeDomainEdgeCases(t *testing.T) {
	// 上游可能返回带协议的域名，必须归一化后才能和入参比较
	r := model.Record{Domain: "HTTPS://Example.COM/path"}
	r.Normalize()
	if r.Domain != "example.com" {
		t.Fatalf("域名归一化失败: %q", r.Domain)
	}
}

// 凭据必须真的带进请求：配置里写了自己的 id/key，请求里却还是公共值，
// 等于「配了也没用」，而错误只在被限流时才暴露。
func TestAPIHZCredentialFromOptions(t *testing.T) {
	var gotID, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = r.URL.Query().Get("id")
		gotKey = r.URL.Query().Get("key")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 400, "msg": "无数据"})
	}))
	defer srv.Close()

	reg := NewRegistry()
	inst, err := reg.Build("apihz", Options{HTTP: mockClient(), ID: "myid", Key: "mykey"})
	if err != nil {
		t.Fatal(err)
	}
	inst.(*APIHZ).URL = srv.URL
	_, _ = inst.Lookup(context.Background(), "example.com")
	if gotID != "myid" || gotKey != "mykey" {
		t.Fatalf("请求里的凭据不对: id=%q key=%q", gotID, gotKey)
	}
}

// 不填凭据时保持原来的公共测试值，老部署行为不变。
func TestAPIHZDefaultCredentialUnchanged(t *testing.T) {
	p := NewAPIHZ(Options{HTTP: mockClient()})
	if p.ID != "88888888" || p.Key != "88888888" {
		t.Fatalf("默认凭据不该变: %q %q", p.ID, p.Key)
	}
}
