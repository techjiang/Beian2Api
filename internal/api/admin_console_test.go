package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asoe/beian2api/internal/config"
	"github.com/asoe/beian2api/internal/model"
	"github.com/asoe/beian2api/internal/store"
)

// adminReq 发一个「看起来来自本机」的请求。
//
// httptest.NewRequest 默认把 RemoteAddr 设成 192.0.2.1:1234（TEST-NET-1），
// 而后台在未配 Key 时的准入判据正是「TCP 对端是不是回环」。
// 所以这里必须显式改写 RemoteAddr——不写的话每个用例都会拿到 403，
// 让人误以为功能坏了，实际上是测试没模拟对场景。
func adminReq(t *testing.T, h http.Handler, method, path, body string) (*http.Response, string) {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = "127.0.0.1:54321"
	// httptest 默认把 Host 设成 example.com，而「本机直连」的真实场景里
	// 地址栏写的是 127.0.0.1。后台的准入判据要同时看对端地址与 Host
	// （后者防 DNS 重绑定），所以这里必须一起模拟对。
	req.Host = "127.0.0.1:8080"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	resp := w.Result()
	b := make([]byte, 0, 1024)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			break
		}
	}
	_ = resp.Body.Close()
	return resp, string(b)
}

// 后台的破坏性接口在未配 Key 时只接受本机直连。
//
// 这是这套接口唯一不能出错的地方：它能删缓存、删历史、清流水。
// 一次「局域网里谁都能点」就等于把数据交给整栋楼。
func TestAdminRequiresLoopbackWhenNoKey(t *testing.T) {
	_, h := mkServer(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/changes/clear", strings.NewReader("{}"))
	// 故意不改 RemoteAddr：保持 httptest 默认的 192.0.2.1（非回环）。
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("非本机访问应 403，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "本机") {
		t.Fatalf("错误信息要说清原因：%s", w.Body.String())
	}
}

// 配了 Key 时，后台跟着走 withAuth：没有 Key 401，有 Key 才 200。
func TestAdminFollowsAPIKeyWhenConfigured(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) {
		c.Auth.Enabled = true
		c.Auth.Keys = []string{"k1"}
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/overview", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("开了鉴权却没带 Key，应 401，实际 %d", w.Code)
	}
	_, body := get(t, h, "/api/v1/admin/overview?key=k1", nil)
	if !strings.Contains(body, `"ok":true`) {
		t.Fatalf("带对的 Key 应放行：%s", body)
	}
}

// 后台页面本身公开，但里面没有任何数据。
//
// 拦页面只会让人打不开自己的后台、也看不到「需要 API Key」那句提示；
// 真正要守的是 /api/v1/admin/* 那层。
func TestAdminPageStaysPublicWithAuth(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) {
		c.Auth.Enabled = true
		c.Auth.Keys = []string{"k1"}
	})
	resp, body := get(t, h, "/admin", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("后台页面应 200，实际 %d", resp.StatusCode)
	}
	if !strings.Contains(body, "后台管理") {
		t.Fatal("页面内容不对")
	}
	// 页面里不能出现任何真实数据（版本号除外），否则「公开」就变成了泄露。
	for _, leak := range []string{"京ICP备", "unit_name"} {
		if strings.Contains(body, leak) {
			t.Fatalf("后台页面不该内联数据：%s", leak)
		}
	}
}

func TestAdminOverviewShape(t *testing.T) {
	_, h := mkServer(t, nil)
	resp, body := adminReq(t, h, http.MethodGet, "/api/v1/admin/overview", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("概览失败 %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Data struct {
			Counts   map[string]int  `json:"counts"`
			Requests map[string]any  `json:"requests"`
			Storage  map[string]any  `json:"storage"`
			Watch    map[string]any  `json:"watch"`
			Auth     map[string]bool `json:"auth"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	// 概览要么全有要么没有：前端靠这几个字段决定首屏渲染什么，
	// 缺一个就会在那里显示 undefined。
	for _, k := range []string{"cache", "history", "changes", "watch", "notify"} {
		if _, ok := out.Data.Counts[k]; !ok {
			t.Errorf("counts 缺少 %s：%s", k, body)
		}
	}
	for _, k := range []string{"hit_rate", "cache_hits"} {
		if _, ok := out.Data.Requests[k]; !ok {
			t.Errorf("requests 缺少 %s", k)
		}
	}
	for _, k := range []string{"size_bytes", "retain_history_total", "history_total"} {
		if _, ok := out.Data.Storage[k]; !ok {
			t.Errorf("storage 缺少 %s", k)
		}
	}
	if _, ok := out.Data.Watch["enabled"]; !ok {
		t.Error("watch 缺少 enabled：巡检关掉时前端要靠它把按钮变灰")
	}
	if _, ok := out.Data.Auth["enabled"]; !ok {
		t.Error("auth 缺少 enabled")
	}
}

// 缓存列表要能按关键词过滤，并且 total 是**过滤后**的集合。
//
// 如果 total 仍按全量算，前端的分页控件会显示「共 300 条」而实际只有 2 条，
// 翻到第二页就是空白。
func TestAdminCacheListAndFilter(t *testing.T) {
	s, h := mkServer(t, nil)
	// 先正常查一次，落一条真实缓存。
	get(t, h, "/api/v1/query?domain=example.com", nil)
	_ = s.svc.Store().PutCache(&store.Entry{
		Domain: "other.com", Found: false, Source: "stub",
		FetchedAt: 1, ExpireAt: 2,
	})

	_, body := adminReq(t, h, http.MethodGet, "/api/v1/admin/cache?limit=10", "")
	if !strings.Contains(body, "example.com") || !strings.Contains(body, "other.com") {
		t.Fatalf("缓存列表不全：%s", body)
	}
	var all struct {
		Data struct {
			Total int `json:"total"`
			Items []struct {
				Domain  string `json:"domain"`
				ICPCode string `json:"icp_code"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &all); err != nil {
		t.Fatal(err)
	}
	if all.Data.Total != 2 {
		t.Fatalf("应有 2 条，实际 %d", all.Data.Total)
	}
	// 备案号也要能搜到：排查时人记得的是「京ICP备…」而不是域名。
	_, body = adminReq(t, h, http.MethodGet, "/api/v1/admin/cache?q=example", "")
	if err := json.Unmarshal([]byte(body), &all); err != nil {
		t.Fatal(err)
	}
	if all.Data.Total != 1 || len(all.Data.Items) != 1 || all.Data.Items[0].Domain != "example.com" {
		t.Fatalf("过滤结果不对：%s", body)
	}
	if all.Data.Items[0].ICPCode == "" {
		t.Fatalf("列表行必须带备案号，否则后台看不出是哪个网站：%s", body)
	}
}

func TestAdminCacheDeleteAndClear(t *testing.T) {
	s, h := mkServer(t, nil)
	get(t, h, "/api/v1/query?domain=example.com", nil)
	// 用 www 版本删主域名的缓存：canonical 归一化必须生效。
	resp, body := adminReq(t, h, http.MethodDelete, "/api/v1/admin/cache?domain=www.example.com", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删除失败 %d: %s", resp.StatusCode, body)
	}
	if n, _ := s.svc.Store().Count(); n.Cache != 0 {
		t.Fatalf("应已删除，实际还剩 %d 条", n.Cache)
	}

	get(t, h, "/api/v1/query?domain=example.com", nil)
	// 空 body 的清理请求等于「只清过期」：不带参数不该造成不可逆后果。
	resp, body = adminReq(t, h, http.MethodPost, "/api/v1/admin/cache/clear", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("清理失败 %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"only_expired":true`) {
		t.Fatalf("空 body 应回落到只清过期：%s", body)
	}
	if n, _ := s.svc.Store().Count(); n.Cache != 1 {
		t.Fatalf("只清过期的请求不该删掉新鲜缓存，实际剩 %d 条", n.Cache)
	}
	adminReq(t, h, http.MethodPost, "/api/v1/admin/cache/clear", `{"only_expired":false}`)
	if n, _ := s.svc.Store().Count(); n.Cache != 0 {
		t.Fatalf("全清后应为 0，实际 %d", n.Cache)
	}
}

// 历史列表是「按域名聚合」的：一个域名几百条快照只占一行，
// 否则后台会把几万条快照铺到页面上，翻都翻不动。
func TestAdminHistoryAggregatesByDomain(t *testing.T) {
	s, h := mkServer(t, nil)
	get(t, h, "/api/v1/query?domain=example.com", nil)
	// 再塞两条指纹不同的快照。
	for _, ts := range []int64{100, 200} {
		r := stubRecord("example.com", ts, "京ICP备2号-1")
		if err := s.svc.Store().AppendHistory(r); err != nil {
			t.Fatal(err)
		}
	}
	_, body := adminReq(t, h, http.MethodGet, "/api/v1/admin/history", "")
	var out struct {
		Data struct {
			Total int `json:"total"`
			Items []struct {
				Domain string `json:"domain"`
				Count  int    `json:"count"`
				LastAt int64  `json:"last_at"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Total != 1 {
		t.Fatalf("一个域名应合成一行，实际 %d 行：%s", out.Data.Total, body)
	}
	if out.Data.Items[0].Count < 2 {
		t.Fatalf("聚合后的条数不对：%s", body)
	}

	resp, del := adminReq(t, h, http.MethodDelete, "/api/v1/admin/history?domain=example.com", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删除历史失败 %d: %s", resp.StatusCode, del)
	}
	if n := s.svc.Store().HistoryCount(); n != 0 {
		t.Fatalf("应全部删除，实际还剩 %d 条", n)
	}
}

func TestAdminChangesClearAndDeleteOne(t *testing.T) {
	s, h := mkServer(t, nil)
	// key 必须用和巡检完全一样的规则（%020d-域名），否则这个用例
	// 只证明了「测试自己造的 key 能被自己删掉」，什么都没验证。
	_ = s.svc.Store().AppendChange(fmt.Sprintf("%020d-%s", 100, "a.com"), map[string]any{"domain": "a.com", "at": 100})
	_ = s.svc.Store().AppendChange(fmt.Sprintf("%020d-%s", 200, "b.com"), map[string]any{"domain": "b.com", "at": 200})

	// 精确删一条：key 由 at + domain 重建，必须和巡检写进去的规则一致。
	resp, body := adminReq(t, h, http.MethodDelete, "/api/v1/admin/changes?at=200&domain=www.b.com", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("按条删除失败 %d: %s", resp.StatusCode, body)
	}
	_, list := adminReq(t, h, http.MethodGet, "/api/v1/changes", "")
	if strings.Contains(list, "b.com") {
		t.Fatalf("b.com 那条应已删除：%s", list)
	}
	if !strings.Contains(list, "a.com") {
		t.Fatalf("a.com 不该被误删：%s", list)
	}

	// 参数不全要明确拒绝：猜一个默认值去删数据是不可接受的。
	resp, _ = adminReq(t, h, http.MethodDelete, "/api/v1/admin/changes?domain=a.com", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 at 应 400，实际 %d", resp.StatusCode)
	}

	adminReq(t, h, http.MethodPost, "/api/v1/admin/changes/clear", `{}`)
	if n, _ := s.svc.Store().Count(); n.Changes != 0 {
		t.Fatalf("清空后应为 0，实际 %d", n.Changes)
	}
}

// 批量加监控：归一化、去重、非法值原样回报。
//
// 「失败的那几个原样列出来」很重要：用户贴进来的是从别处拷的清单，
// 只告诉他「失败 2 个」等于让他自己一行行找。
func TestAdminWatchBulk(t *testing.T) {
	s, h := mkServer(t, nil)
	body := `{"domains":["example.com","https://www.example.com/x","example.com","bad domain!",""],"action":"add"}`
	resp, out := adminReq(t, h, http.MethodPost, "/api/v1/admin/watch/bulk", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("批量加入失败 %d: %s", resp.StatusCode, out)
	}
	var res struct {
		Data struct {
			Added      int      `json:"added"`
			Duplicated int      `json:"duplicated"`
			Failed     int      `json:"failed"`
			Invalid    []string `json:"invalid"`
			OK         []string `json:"ok"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Data.Added != 1 {
		t.Fatalf("example.com 与 www 版本应合并成 1 个，实际 %d：%s", res.Data.Added, out)
	}
	// 三个输入归一化之后都是 example.com：第一个计入、后两个算重复。
	if res.Data.Duplicated != 2 {
		t.Fatalf("重复项应被计数，实际 %d：%s", res.Data.Duplicated, out)
	}
	if res.Data.Failed != 1 || len(res.Data.Invalid) != 1 || res.Data.Invalid[0] != "bad domain!" {
		t.Fatalf("非法域名要原样回报：%s", out)
	}
	if n, _ := s.svc.Store().CountWatch(); n != 1 {
		t.Fatalf("库里应有 1 个监控，实际 %d", n)
	}

	// 批量加的域名必须已经建立基线，否则第一轮巡检会把「全部字段」
	// 当成一次变更推出去。addWatch 里那句 Query 就是为这个。
	w, err := s.svc.Store().GetWatch("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if w.LastFP == "" {
		t.Fatal("批量加入的域名必须同时建立基线")
	}

	resp, out = adminReq(t, h, http.MethodPost, "/api/v1/admin/watch/bulk", `{"domains":["example.com"],"action":"remove"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("批量移出失败 %d: %s", resp.StatusCode, out)
	}
	if n, _ := s.svc.Store().CountWatch(); n != 0 {
		t.Fatalf("应已移出，实际剩 %d", n)
	}

	// action 写错不能当成 add 静默放过：这是「删」和「加」的区别。
	resp, _ = adminReq(t, h, http.MethodPost, "/api/v1/admin/watch/bulk", `{"domains":["a.com"],"action":"del"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知 action 应 400，实际 %d", resp.StatusCode)
	}
}

func TestAdminBackupsListAndPurge(t *testing.T) {
	s, h := mkServer(t, nil)
	dir := filepath.Join(s.cfg.DataDir, "backups")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	// 同目录放一个非 .db 文件：清空只能删自己的快照，不能误伤用户的东西。
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("keep me"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.db"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}

	_, body := adminReq(t, h, http.MethodGet, "/api/v1/admin/backups", "")
	if !strings.Contains(body, "a.db") || strings.Contains(body, "README.txt") {
		t.Fatalf("备份列表不对：%s", body)
	}

	resp, del := adminReq(t, h, http.MethodDelete, "/api/v1/admin/backups", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("清空备份失败 %d: %s", resp.StatusCode, del)
	}
	if !strings.Contains(del, `"removed":1`) {
		t.Fatalf("应删除 1 个：%s", del)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.txt")); err != nil {
		t.Fatal("非 .db 文件不该被删")
	}
}

// 后台接口也要守方法：一个 GET 不该清掉任何东西。
func TestAdminRejectsWrongMethods(t *testing.T) {
	_, h := mkServer(t, nil)
	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/cache/clear"},
		{http.MethodDelete, "/api/v1/admin/overview"},
		{http.MethodGet, "/api/v1/admin/watch/purge"},
		{http.MethodPost, "/api/v1/admin/backups"},
	}
	for _, c := range cases {
		resp, body := adminReq(t, h, c.method, c.path, "")
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s 应 405，实际 %d: %s", c.method, c.path, resp.StatusCode, body)
		}
	}
}

// 后台前端是手写 JS，同样没有类型检查——所以和 app.js 一样用静态检查兜底。
func TestAdminFrontendSane(t *testing.T) {
	src := string(mustAsset(t, "admin.js"))
	// 与查询台共用 storage key：两边填的 Key 必须是同一个，
	// 否则在后台填完再打开查询台又是 401。
	if !strings.Contains(src, "var KEY_STORE = 'beian2api.key'") {
		t.Fatal("admin.js 必须声明 KEY_STORE")
	}
	if !strings.Contains(src, "localStorage.getItem(KEY_STORE)") ||
		!strings.Contains(src, "localStorage.setItem(KEY_STORE") {
		t.Fatal("Key 的读与写必须落到同一个 storage key")
	}
	// 每个 tab 对应的面板 id 必须真的存在，否则点了侧栏是一片空白。
	html := string(mustAsset(t, "admin.html"))
	for _, id := range []string{"overview", "cache", "history", "changes", "watch", "backups"} {
		if !strings.Contains(html, `id="p-`+id+`"`) {
			t.Errorf("admin.html 缺少面板 #p-%s", id)
		}
		if !strings.Contains(html, `data-p="`+id+`"`) {
			t.Errorf("admin.html 缺少侧栏入口 %s", id)
		}
	}
	// 破坏性按钮必须有二次确认：后台一次手滑是不可逆的数据丢失。
	for _, id := range []string{"#c-clear-all", "#ch-clear", "#w-purge", "#b-purge"} {
		idx := strings.Index(html, id[1:])
		if idx < 0 {
			t.Errorf("admin.html 缺少按钮 %s", id)
			continue
		}
		// 对应的 click 处理里必须出现 confirm
		marker := "bind('" + id + "'"
		start := strings.Index(src, marker)
		if start < 0 {
			t.Errorf("admin.js 没有绑定 %s", id)
			continue
		}
		end := start + 700
		if end > len(src) {
			end = len(src)
		}
		if !strings.Contains(src[start:end], "confirm(") {
			t.Errorf("%s 必须二次确认，否则一次误点就是不可逆的数据丢失", id)
		}
	}
}

// 前端资源指纹必须覆盖后台的 css/js。
//
// 只算 style.css / app.js 的话，改后台样式不会让 ?v= 变，
// 浏览器吃 7 天 immutable 缓存，升级后看不到任何变化。
func TestAssetVersionCoversAdminAssets(t *testing.T) {
	// 直接比两个版本的算法覆盖范围：源码里必须把 admin 的两个文件算进去。
	src := string(mustReadSource(t, "static.go"))
	for _, name := range []string{"admin.css", "admin.js"} {
		if !strings.Contains(src, name) {
			t.Errorf("assetVersion 没把 %s 算进指纹", name)
		}
	}
}

// mustReadSource 读本包里的一个 .go 源文件（测试与源码同目录）。
func mustReadSource(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("读取 %s: %v", name, err)
	}
	return b
}

// stubRecord 造一条带指定抓取时间的记录，用来铺历史快照。
func stubRecord(domain string, ts int64, icp string) model.Record {
	r := model.Record{
		Domain: domain, UnitName: "示例公司", ICPCode: icp,
		Source: "stub", FetchedAt: ts,
	}
	r.Normalize()
	return r
}
