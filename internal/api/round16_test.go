package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asoe/beian2api/internal/model"
)

// 这一组同样来自「把服务跑起来照着文档打」：五个问题都属于同一种形状——
// 接口回 200、形状看着对，只是它做的事和说的事不一样。前几轮也在这条线上，
// 说明这类问题不会自己用完，只能一段一段地实测。

// RSS 完全忽略 kind：`?kind=revoked` 返回 200 和整条流水。
//
// /api/v1/changes 从上一轮起就认 kind，写错回 400；同一个词在 RSS 上
// 既不筛也不报错。订阅「只看注销」的人以为自己盯上了注销，实际收到全部——
// 而 RSS 的消费方不会核对内容，它只会照单全收。
func TestRSSHonoursKind(t *testing.T) {
	s, h := mkServer(t, nil)
	add := func(at int64, domain, kind string) {
		ch := map[string]any{"domain": domain, "at": at, "kind": kind, "icp_code": "京ICP备1000001号-1"}
		if err := s.svc.Store().AppendChange(fmt.Sprintf("%020d-%s", at, domain), ch); err != nil {
			t.Fatal(err)
		}
	}
	add(1000, "changed.com", "changed")
	add(1001, "revoked.com", "revoked")
	add(1002, "registered.com", "registered")

	_, all := get(t, h, "/api/v1/changes.rss", nil)
	if n := strings.Count(all, "<item>"); n != 3 {
		t.Fatalf("不带 kind 应返回全部 3 条，实际 %d：%s", n, all)
	}
	resp, only := get(t, h, "/api/v1/changes.rss?kind=revoked", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("kind=revoked 应 200，实际 %d", resp.StatusCode)
	}
	if n := strings.Count(only, "<item>"); n != 1 {
		t.Fatalf("kind=revoked 应只返回 1 条，实际 %d：%s", n, only)
	}
	if !strings.Contains(only, "revoked.com") || strings.Contains(only, "changed.com") {
		t.Fatalf("筛选结果串了：%s", only)
	}
	// 写错取值要和 /api/v1/changes 一样报错，不能静默当成「不过滤」。
	resp, body := get(t, h, "/api/v1/changes.rss?kind=bogus", nil)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "invalid_kind") {
		t.Fatalf("kind 写错应 400 invalid_kind，实际 %d：%s", resp.StatusCode, body)
	}
}

// 带 limit 取历史时，返回的第一条永远没有 diff。
//
// 服务端按 limit 取最近的 N 条，再在结果里相邻相减——于是这一页的头一条
// 是「这一页里最旧的那条」，它其实前面还有一次变更，只是被 limit 挡在外面。
// 那条记录看起来像「从来没变过」，而它恰恰是变过的那一条。
// 分页不该改变单条记录的含义。
func TestHistoryFirstItemKeepsDiffWithLimit(t *testing.T) {
	s, h := mkServer(t, nil)
	dir := t.TempDir()
	_ = dir
	base := int64(1794000000)
	for i, name := range []string{"A公司", "B公司", "C公司"} {
		rec := model.Record{
			Domain: "multi.com", UnitName: name, UnitType: "企业", SiteName: "站点",
			ICPCode: "京ICP备100000" + string(rune('1'+i)) + "号-1",
			Nature:  "企业", Source: "mock", FetchedAt: base + int64(i)*3600,
		}
		if err := s.svc.Store().AppendHistory(rec); err != nil {
			t.Fatal(err)
		}
	}

	// limit=2 拿到 B、C 两条。B 前面还有 A，所以 B 必须带 diff。
	_, body := get(t, h, "/api/v1/history?domain=multi.com&limit=2", nil)
	var out struct {
		Data struct {
			Total   int `json:"total"`
			History []struct {
				Record model.Record `json:"record"`
				Diff   []struct {
					Field string `json:"field"`
				} `json:"diff"`
			} `json:"history"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Total != 2 {
		t.Fatalf("limit=2 应回 2 条，实际 %d：%s", out.Data.Total, body)
	}
	if len(out.Data.History) != 2 {
		t.Fatalf("应有 2 条 history：%s", body)
	}
	first := out.Data.History[0]
	if first.Record.UnitName != "B公司" {
		t.Fatalf("第一条应是 B公司，实际 %s", first.Record.UnitName)
	}
	if len(first.Diff) == 0 {
		t.Fatalf("第一条（B公司）带着对 A公司 的变化，diff 不能为空：%s", body)
	}
	found := false
	for _, d := range first.Diff {
		if d.Field == "主办单位" {
			found = true
		}
	}
	if !found {
		t.Fatalf("B 相对 A 的主办单位变了，diff 里应有「主办单位」：%s", body)
	}
}

// 移出一个不在列表里的域名，回 {"watching":false}。
//
// 批量移出早就如实报 missing 了，单接口还在敷衍：值不合法（`<script>`）
// 也一样回 200 + watching:false。一个「成功」的形状，背后什么都没做——
// 而人看着这句 200，会以为自己已经把它移出去了。
func TestWatchDeleteReportsMissing(t *testing.T) {
	_, h := mkServer(t, nil)

	// 从没加过：404 not_watching，而不是 200。
	resp, body := delWatch(t, h, "never-watched.com")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "not_watching") {
		t.Fatalf("移出未监控的域名应 404 not_watching，实际 %d：%s", resp.StatusCode, body)
	}

	// 格式不合法：400，而不是悄悄当成功。
	resp, body = delWatch(t, h, "<script>")
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "invalid_domain") {
		t.Fatalf("非法域名应 400 invalid_domain，实际 %d：%s", resp.StatusCode, body)
	}

	// 真的在列表里：200 且带 removed。
	add := httptest.NewRequest(http.MethodPost, "/api/v1/watch", strings.NewReader(`{"domain":"keep.com"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, add)
	if w.Code != http.StatusOK {
		t.Fatalf("加入监控失败 %d: %s", w.Code, w.Body.String())
	}
	resp, body = delWatch(t, h, "keep.com")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"removed":true`) {
		t.Fatalf("移出在列表里的域名应 200 + removed，实际 %d：%s", resp.StatusCode, body)
	}
	// 移两次：第二次同样不能假装成功。
	resp, body = delWatch(t, h, "keep.com")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("重复移出应 404，实际 %d：%s", resp.StatusCode, body)
	}
}

func delWatch(t *testing.T, h http.Handler, domain string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/watch?domain="+domain, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Result(), w.Body.String()
}

// 批量移出全部「本来就不在列表里」时回 502「请检查上游连通性」。
//
// 移出不碰上游——它只删本地记录。让用户去查数据源，等于给了一个错的
// 排查方向；而正确的答案就在手上：这些域名本来就没在监控里。
func TestBulkRemoveMissingIsNotUpstreamError(t *testing.T) {
	_, h := mkServer(t, nil)
	resp, body := adminReq(t, h, http.MethodPost, "/api/v1/admin/watch/bulk",
		`{"domains":["ghost1.com","ghost2.com"],"action":"remove"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("全部不在列表里不该是 502，实际 %d：%s", resp.StatusCode, body)
	}
	var out struct {
		Data struct {
			Removed int      `json:"added"`
			Missing []string `json:"missing"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Removed != 0 || len(out.Data.Missing) != 2 {
		t.Fatalf("应回 added:0 + missing 两项，实际 %s", body)
	}
	if strings.Contains(body, "上游") {
		t.Fatalf("移出不涉及上游，不该提上游：%s", body)
	}
}

// 徽章 label 没有长度上限。
//
// 徽章是公开接口，label 由 URL 决定：不封顶时一个 5000 字符的 label
// 能生成三万像素宽、20KB 的 SVG，一次请求就能让别人家的 README 横向滚到
// 天边，而这张图还是从这台服务器上吐出去的。
func TestBadgeLabelIsClipped(t *testing.T) {
	_, h := mkServer(t, nil)
	long := strings.Repeat("L", 5000)
	_, body := get(t, h, "/badge/icp.svg?domain=example.com&label="+long, nil)
	if strings.Contains(body, long) {
		t.Fatal("超长 label 应被截断，实际原样输出")
	}
	if w := svgWidth(t, body); w > 800 {
		t.Fatalf("截断后宽度仍达 %d，标签没被真正限制", w)
	}
	// 正常长度不能被截断。
	_, body = get(t, h, "/badge/icp.svg?domain=example.com&label=备案", nil)
	if !strings.Contains(body, "备案:") {
		t.Fatalf("正常 label 被改坏了：%s", body)
	}
	// 按字符截断：多字节字符不能被劈成半个。
	_, body = get(t, h, "/badge/icp.svg?domain=example.com&label="+strings.Repeat("备", 40), nil)
	if strings.Contains(body, "\ufffd") {
		t.Fatal("截断把中文劈成了替换字符")
	}
}

func svgWidth(t *testing.T, svg string) int {
	t.Helper()
	const marker = `width="`
	i := strings.Index(svg, marker)
	if i < 0 {
		t.Fatalf("SVG 缺少 width：%.120s", svg)
	}
	rest := svg[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("SVG width 未闭合")
	}
	n := 0
	for _, c := range rest[:j] {
		if c < '0' || c > '9' {
			t.Fatalf("SVG width 不是数字：%q", rest[:j])
		}
		n = n*10 + int(c-'0')
	}
	return n
}
