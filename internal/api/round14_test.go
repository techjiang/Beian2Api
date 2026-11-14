package api

import (
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

// 这轮的问题都是「在真跑起来的实例上手动打一遍」撞出来的，不是在编辑器里
// 想出来的。五条里有三条属于同一类：**接口做了一件比它承诺的更大的事**。

// DELETE /api/v1/admin/backups?name=x.db 只该删 x.db。
//
// 之前这个 name 参数根本没人读：路径、OpenAPI、后台按钮上都写着「删这一个」，
// 实际执行的是「清空整个备份目录」。备份是出事时最后一份数据，
// 一次手滑全没了，而响应只回一句 removed: 3，看着还挺正常。
func TestDeleteOneBackupDoesNotPurgeAll(t *testing.T) {
	s, h := mkServer(t, nil)
	dir := filepath.Join(s.cfg.DataDir, "backups")
	for _, n := range []string{"keep-a.db", "keep-b.db", "victim.db"} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	resp, body := adminReq(t, h, http.MethodDelete, "/api/v1/admin/backups?name=victim.db", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删单个备份应 200，实际 %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"removed":1`) || !strings.Contains(body, `"deleted":true`) {
		t.Fatalf("应如实回删掉 1 个：%s", body)
	}
	for _, n := range []string{"keep-a.db", "keep-b.db"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("删一个备份把 %s 也删了：%v", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "victim.db")); !os.IsNotExist(err) {
		t.Fatal("目标备份应已被删除")
	}

	// 删一个不存在的名字：回 removed:0 / deleted:false，而不是假成功。
	_, body = adminReq(t, h, http.MethodDelete, "/api/v1/admin/backups?name=nope.db", "")
	if !strings.Contains(body, `"deleted":false`) {
		t.Fatalf("删不存在的备份要如实说没删到：%s", body)
	}

	// name 只能是目录下的 .db 文件名。
	for _, bad := range []string{"../evil.db", "sub/evil.db", ".hidden.db", "note.txt"} {
		resp, body := adminReq(t, h, http.MethodDelete,
			"/api/v1/admin/backups?name="+bad, "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("name=%q 应 400，实际 %d: %s", bad, resp.StatusCode, body)
		}
	}
}

// 不带 name 的 DELETE 才是「清空」，且仍然只认 .db。
func TestPurgeBackupsStillWorks(t *testing.T) {
	s, h := mkServer(t, nil)
	dir := filepath.Join(s.cfg.DataDir, "backups")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.db", "b.db", "README.md"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	_, body := adminReq(t, h, http.MethodDelete, "/api/v1/admin/backups", "")
	if !strings.Contains(body, `"removed":2`) {
		t.Fatalf("应删掉 2 个 .db：%s", body)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Fatal("同目录里非 .db 的文件不该被误伤")
	}
}

// HEAD /api/v1/admin/backup 不该真的写出一份备份文件。
//
// allowMethods 会顺手放行 HEAD（对只读接口是对的），但这个接口读一下就会
// 往磁盘写文件。而 HEAD 恰好是探针、健康检查、curl -I 最常用的方法：
// 一个「我只想问你活着没」的请求往磁盘里留一份全量快照，反复探活就能把
// 磁盘写满。
func TestBackupHeadDoesNotWriteFile(t *testing.T) {
	s, h := mkServer(t, nil)
	dir := filepath.Join(s.cfg.DataDir, "backups")

	req := httptest.NewRequest(http.MethodHead, "/api/v1/admin/backup", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:8080"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD 备份应 405，实际 %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Allow"); !strings.Contains(got, "POST") {
		t.Fatalf("405 要带 Allow 头，实际 %q", got)
	}
	if ents, err := os.ReadDir(dir); err == nil && len(ents) > 0 {
		t.Fatalf("HEAD 不该写出备份文件，实际 %d 个", len(ents))
	}
	// GET 仍然会写（它是明确的「给我一份备份」），只是 HEAD 不再顺带写。
	if _, body := adminReq(t, h, http.MethodPost, "/api/v1/admin/backup", ""); !strings.Contains(body, `"ok":true`) {
		t.Fatalf("POST 备份应仍然可用：%s", body)
	}
}

// 显式指定的备份名必须以 .db 结尾。
//
// 备份列表和「清空备份」都只认 .db（它们要保护目录里用户自己放的东西）。
// 一个不带后缀的快照会写进磁盘、后台看不见、删不掉、也不会被清理：
// 纯占地方，而且没人知道它在。`backup -server ... -o /tmp/snap` 正好走这条路。
func TestBackupNameMustEndWithDB(t *testing.T) {
	s, h := mkServer(t, nil)
	resp, body := adminReq(t, h, http.MethodPost, "/api/v1/admin/backup?name=snapshot", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("不带 .db 后缀应 400，实际 %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, ".db") {
		t.Fatalf("错误信息要说明后缀要求：%s", body)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "backups", "snapshot")); !os.IsNotExist(err) {
		t.Fatal("被拒绝的备份名不该留下文件")
	}
}

// 移出一个从来没加过的域名，不能回「移出成功」。
//
// bbolt 的 Delete 对不存在的 key 返回 nil，把 nil 当成成功，后台就会弹
// 「移出 2 个」——而那 2 个域名根本没在列表里。用户会以为自己看错了，
// 或者怀疑列表没刷新。这个坑在删缓存、删变更流水上已经踩过两次。
func TestBulkRemoveReportsMissing(t *testing.T) {
	s, h := mkServer(t, nil)
	if err := s.svc.Store().PutWatch(&store.WatchItem{Domain: "real.com", AddedAt: model.Now()}); err != nil {
		t.Fatal(err)
	}
	_, body := adminReq(t, h, http.MethodPost, "/api/v1/admin/watch/bulk",
		`{"domains":["real.com","ghost1.com","ghost2.com"],"action":"remove"}`)
	if !strings.Contains(body, `"added":1`) {
		t.Fatalf("只有 real.com 真被移出，added 应为 1：%s", body)
	}
	if !strings.Contains(body, `"missing":["ghost1.com","ghost2.com"]`) {
		t.Fatalf("没在列表里的域名要单独列出来：%s", body)
	}
	if n, _ := s.svc.Store().CountWatch(); n != 0 {
		t.Fatalf("real.com 应已被移出，剩下 %d 条", n)
	}
}

// 请求体里出现第二个 JSON 值必须报错，不能静默只取第一个。
//
// json.Decoder 只解第一个值就收工，剩下的原样留在流里。于是
// `{"domain":"a.com"}{"domain":"b.com"}` 会成功返回 a.com 的结果，
// 而发请求的脚本以为自己提交了两个。批量接口更糟：
// `{"domains":[...]}{"action":"remove"}` 会按默认 add 执行——
// 收到的意图被悄悄改写了一半，还回 200。
func TestDecodeJSONRejectsTrailingValue(t *testing.T) {
	s, h := mkServer(t, nil)
	_, body := adminReq(t, h, http.MethodPost, "/api/v1/watch",
		`{"domain":"a.com"}{"domain":"b.com"}`)
	if !strings.Contains(body, `"ok":false`) || !strings.Contains(body, "一个 JSON") {
		t.Fatalf("尾随第二个 JSON 应被拒：%s", body)
	}
	if n, _ := s.svc.Store().CountWatch(); n != 0 {
		t.Fatalf("被拒的请求不该写进监控，实际 %d 条", n)
	}

	// 后面跟一串垃圾同样要报错：那说明请求体本身拼错了。
	if _, body := adminReq(t, h, http.MethodPost, "/api/v1/watch",
		`{"domain":"a.com"} garbage`); !strings.Contains(body, `"ok":false`) {
		t.Fatalf("尾随垃圾应被拒：%s", body)
	}
	// 只有一个 JSON 值时照常工作。
	if _, body := adminReq(t, h, http.MethodPost, "/api/v1/watch",
		`{"domain":"ok.com"}`); !strings.Contains(body, `"watching":true`) {
		t.Fatalf("正常请求不该受影响：%s", body)
	}
}

// counts.keys 要如实反映「这个实例有几把 Key」。
//
// 它原来读的是 apikeys bucket——而那个 bucket 从来没有人写过：Key 只在
// 配置文件和环境变量里，服务启动时读进内存，不落库。于是字段恒为 0，
// 界面上就是「一把 Key 都没有」，而实例可能正用着两把 Key 拦着全网。
func TestStatsCountsKeys(t *testing.T) {
	_, h := mkServer(t, func(c *config.Config) {
		c.Auth.Enabled = true
		c.Auth.Keys = []string{"k1", "k2"}
	})
	resp, body := get(t, h, "/api/v1/stats", map[string]string{"X-API-Key": "k1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats 应 200，实际 %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"keys":2`) {
		t.Fatalf("配了 2 把 Key，counts.keys 应为 2：%s", body)
	}
}
