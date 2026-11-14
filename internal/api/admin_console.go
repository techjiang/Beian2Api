package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asoe/beian2api/internal/model"
	"github.com/asoe/beian2api/internal/store"
)

// 这个文件是后台管理的接口层。
//
// 设计上只有一条原则：**写操作必须能被证明是管理员发起的**。
// 查询台是公开的（徽章、演示、别人家 README），而这里能删缓存、删历史、
// 清流水、清监控列表——一次误调就是不可逆的数据损失。所以：
//
//   - 配了 API Key：沿用 withAuth，必须带 Key；
//   - 没配 Key：只允许真正的 TCP 回环直连（与在线备份同一套判据），
//     「局域网里谁都能点」和「公网谁都能删」之间必须有一条线。
//
// 注意这不是「再加一层鉴权」：withAuth 管的是身份，这里管的是
// 「这个身份是否被允许做破坏性操作」。两个问题分开问，才不会出现
// 「某人拿到了只读 Key 就能清空别人的历史」这种事。

// withAdmin 给管理接口加一层「本机或持 Key」的门。
func (s *Server) withAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 配了 Key 时 withAuth 已经把身份验完了，走到这里就是合法的。
		if len(s.keys) > 0 {
			next(w, r)
			return
		}
		if !s.isLoopback(r) {
			writeErr(w, http.StatusForbidden, "forbidden",
				"后台只接受本机直连；要远程管理请配置 API Key 后带上")
			return
		}
		next(w, r)
	}
}

// handleAdminOverview 返回后台首屏需要的全部概况。
//
// 合成一个接口而不是让前端打五六个：概览页一进来就要同时显示
// 「有多少数据、上游配了什么、巡检还活着吗、上次推送成功没」。
// 分开请求的话，任何一个慢/失败都会让首屏卡在半成品状态，而这几个
// 数字本来就在同一个进程里，合成一次几乎是免费的。
func (s *Server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.Store().Count()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	hits, misses, stale := s.svc.Store().Stat("hits"), s.svc.Store().Stat("misses"), s.svc.Store().Stat("stale")
	total := hits + misses
	rate := 0.0
	if total > 0 {
		rate = float64(hits) * 100 / float64(total)
	}
	retainChanges, retainHistory := s.svc.Store().Retention()

	// 巡检节奏：直接取 watcher 自己的状态，别在前端重算一遍间隔。
	watch := map[string]any{"enabled": false, "total": 0, "running": false}
	if s.watch != nil {
		st := s.watch.StatusPage(s.cfg.Watch.Enabled, 0, 0)
		watch = map[string]any{
			"enabled": st.Enabled, "interval": st.Interval, "total": st.Total,
			"running": st.Running, "last_run_at": st.LastRunAt,
			"last_count": st.LastCount, "next_run_at": st.NextRunAt,
		}
	} else {
		// 巡检关闭时监控项仍然存在（可能是从开着巡检的部署改过来的），
		// 概览页必须照实报出「有 N 个域名在监控，但不会自动巡检」。
		watch["total"], _ = s.svc.Store().CountWatch()
	}

	type chRow struct {
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		LastAt    int64  `json:"last_at"`
		LastOK    bool   `json:"last_ok"`
		LastError string `json:"last_error,omitempty"`
	}
	channels := make([]chRow, 0, 4)
	if s.notifier != nil {
		states := s.notifier.States()
		for _, ch := range s.notifier.Channels() {
			row := chRow{Name: ch.Name, Kind: ch.Kind}
			if st, ok := states[ch.Name]; ok {
				row.LastAt, row.LastOK, row.LastError = st.LastAt, st.LastOK, st.LastError
			}
			channels = append(channels, row)
		}
	}

	backups := s.backupsRows()

	// 趋势：最近 24 小时的小时序列。概览页顶上那张折线图就取这一份。
	//
	// 合成在 overview 里而不是单独开接口：首屏本来就要一次请求拿全部，
	// 再拆一个「图表接口」只会让首屏从「等一次」变成「等最慢的那次」，
	// 而 24 行的数据在这里几乎不要钱。
	series, err := s.svc.Store().Series(24)
	if err != nil {
		series = nil
	}

	writeOK(w, map[string]any{
		"series":     series,
		"version":    s.version,
		"uptime_sec": int(time.Since(s.startAt).Seconds()),
		"now":        time.Now().Unix(),
		"counts":     c,
		"requests": map[string]any{
			"cache_hits": hits, "cache_misses": misses, "stale_fallbacks": stale,
			"hit_rate": float64(int(rate*10+0.5)) / 10,
		},
		"storage": map[string]any{
			"size_bytes":           s.svc.Store().Size(),
			"path":                 s.svc.Store().Path(),
			"retain_changes":       retainChanges,
			"retain_history":       retainHistory,
			"retain_history_total": s.svc.Store().RetentionTotal(),
			"stale_window":         s.svc.Store().StaleWindow(),
			"history_total":        s.svc.Store().HistoryCount(),
		},
		"providers": s.providerRows(),
		"watch":     watch,
		"notify":    channels,
		"backups":   backups,
		"auth":      map[string]any{"enabled": len(s.keys) > 0},
	})
}

// providerRow 是数据源在后台里的一行。
type providerRow struct {
	Name             string `json:"name"`
	Enabled          bool   `json:"enabled"`
	Weight           int    `json:"weight"`
	Timeout          int    `json:"timeout"`
	CustomCredential bool   `json:"custom_credential"`
}

func (s *Server) providerRows() []providerRow {
	out := make([]providerRow, 0, len(s.cfg.Providers))
	for _, p := range s.cfg.Providers {
		out = append(out, providerRow{
			Name: p.Name, Enabled: p.Enabled && p.Weight > 0, Weight: p.Weight,
			Timeout: p.Timeout, CustomCredential: p.HasCredential,
		})
	}
	return out
}

func (s *Server) backupDir() string { return filepath.Join(s.cfg.DataDir, "backups") }

// backupsRows 把快照文件列表转成对外 JSON 形状（概览与备份页共用）。
func (s *Server) backupsRows() []map[string]any {
	infos, err := store.ListBackups(s.backupDir())
	if err != nil {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(infos))
	for _, b := range infos {
		out = append(out, map[string]any{"name": b.Name, "size": b.Size, "created_at": b.CreatedAt})
	}
	return out
}

// handleAdminCache 列出或清理缓存。
//
// DELETE 带 domain 删一条；POST /clear 清一批。两个动作分路径，
// 是因为「删这一个域名」和「全清」在界面上本来就是两个按钮，
// 而 HTTP 方法没法表达「删一个」和「删全部」的区别。
func (s *Server) handleAdminCache(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch r.Method {
	case http.MethodGet:
		limit, err := parseLimit(q.Get("limit"), 30, 200)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_limit", err.Error())
			return
		}
		offset, err := parseOffset(q.Get("offset"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_offset", err.Error())
			return
		}
		rows, total, err := s.svc.Store().ListCachePage(limit, offset, q.Get("q"))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		writeOK(w, map[string]any{
			"total": total, "offset": offset, "limit": limit,
			"has_more": offset+len(rows) < total, "items": rows,
			"now": time.Now().Unix(),
		})
	case http.MethodDelete:
		d := model.CanonicalDomain(q.Get("domain"))
		if d == "" {
			writeErr(w, http.StatusBadRequest, "missing_domain", "缺少 domain 参数")
			return
		}
		deleted, err := s.svc.Store().DeleteCache(d)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		// 没删到任何东西时如实说没删到。
		// bbolt 的 Delete 对不存在的 key 返回 nil，照旧写 deleted:true 的话，
		// 前端会弹「已删除 nope.com」——而那个域名从来没被缓存过，
		// 用户只会以为自己记错了域名，或者怀疑删除按钮没生效。
		writeOK(w, map[string]any{"domain": d, "deleted": deleted})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 GET / DELETE")
	}
}

// handleAdminCacheClear 清理缓存：只清过期，或全清。
func (s *Server) handleAdminCacheClear(w http.ResponseWriter, r *http.Request) {
	// 默认值必须是「只清过期」。
	// 一个不带参数的 POST 不该造成不可逆的后果，所以这里不是
	// 「默认 false」——那是全清。默认值本身就是安全策略的一部分。
	body := struct {
		OnlyExpired bool `json:"only_expired"`
	}{OnlyExpired: true}
	// 有请求体就必须读它。
	//
	// 判据原来只有 `r.ContentLength > 0`，而 chunked 编码的请求 ContentLength 是 -1
	// （未知）。于是 `curl -H 'Transfer-Encoding: chunked' -d '{"only_expired":false}'`
	// 发过来的意图被整个丢掉，默默按「只清过期」执行了——请求方要求的是全清，
	// 收到的却是一句「已清理 0 条过期缓存」，看起来还像是成功了。
	// 对一个不可逆操作来说，「静默换个语义执行」比直接报错危险得多。
	if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
		if err := decodeJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	n, err := s.svc.Store().ClearCache(body.OnlyExpired, time.Now().Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeOK(w, map[string]any{"removed": n, "only_expired": body.OnlyExpired})
}

// handleAdminHistory 列出（按域名聚合）或删除历史快照。
func (s *Server) handleAdminHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch r.Method {
	case http.MethodGet:
		limit, err := parseLimit(q.Get("limit"), 30, 200)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_limit", err.Error())
			return
		}
		offset, err := parseOffset(q.Get("offset"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_offset", err.Error())
			return
		}
		rows, total, err := s.svc.Store().ListHistoryDomains(limit, offset, q.Get("q"))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		writeOK(w, map[string]any{
			"total": total, "offset": offset, "limit": limit,
			"has_more": offset+len(rows) < total, "items": rows,
		})
	case http.MethodDelete:
		d := model.CanonicalDomain(q.Get("domain"))
		if d == "" {
			writeErr(w, http.StatusBadRequest, "missing_domain", "缺少 domain 参数")
			return
		}
		n, err := s.svc.Store().DeleteHistoryDomain(d)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		writeOK(w, map[string]any{"domain": d, "removed": n})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 GET / DELETE")
	}
}

// handleAdminChangesClear 清空变更流水。
func (s *Server) handleAdminChangesClear(w http.ResponseWriter, r *http.Request) {
	n, err := s.svc.Store().ClearChanges()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeOK(w, map[string]any{"removed": n})
}

// handleAdminChangeDelete 删一条变更流水。
//
// 为什么要有：一条错的告警（比如测试时随手加的域名产生的变更）会永远
// 挂在首页「最近变更」和 RSS 里，而流水只按保留上限滚动——2000 条之后
// 才会自然消失。给人一个精确删掉它的手段，比让他把整条流水清空合理得多。
// key 由「时间戳-域名」重建，和 watch.appendChange 的规则必须一致。
func (s *Server) handleAdminChangeDelete(w http.ResponseWriter, r *http.Request) {
	atRaw := strings.TrimSpace(r.URL.Query().Get("at"))
	d := model.CanonicalDomain(r.URL.Query().Get("domain"))
	if atRaw == "" || d == "" {
		writeErr(w, http.StatusBadRequest, "missing_param", "需要 at（Unix 秒）与 domain 两个参数")
		return
	}
	at, err := strconv.ParseInt(atRaw, 10, 64)
	if err != nil || at <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid_at", "at 必须是正整数时间戳")
		return
	}
	// 域名字符串直接进 key，所以这里必须挡住 \x00：
	// 它正是历史与监控 key 的分隔符，混进去能构造出指向别的记录 key。
	if strings.ContainsAny(d, "\x00") {
		writeErr(w, http.StatusBadRequest, "invalid_domain", "域名不合法")
		return
	}
	id := fmt.Sprintf("%020d-%s", at, d)
	deleted, err := s.svc.Store().DeleteChange(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeOK(w, map[string]any{"at": at, "domain": d, "deleted": deleted})
}

// handleAdminWatchBulk 批量加/移监控。
//
// 单个域名走 /api/v1/watch 就够了，但后台典型场景是「把导出的域名清单
// 一次性贴进来」。逐个发请求既慢又会在中途失败时留下半截状态，
// 这里一次事务式地做完，并如实报告每种结果的数量。
func (s *Server) handleAdminWatchBulk(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Domains []string `json:"domains"`
		Action  string   `json:"action"` // add | remove（留空按 add）
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	action := strings.ToLower(strings.TrimSpace(body.Action))
	if action == "" {
		action = "add"
	}
	if action != "add" && action != "remove" {
		writeErr(w, http.StatusBadRequest, "invalid_action", "action 只能是 add 或 remove")
		return
	}
	if len(body.Domains) == 0 {
		writeErr(w, http.StatusBadRequest, "missing_domain", "domains 不能为空")
		return
	}
	if len(body.Domains) > 500 {
		writeErr(w, http.StatusBadRequest, "too_many_domains", "单次最多 500 个域名")
		return
	}

	// 有效域名先去重归一化，非法项原样留着回报——用户要按原文找那一行改。
	var clean []string
	var bad []string
	dup := 0
	seen := map[string]struct{}{}
	for _, raw := range body.Domains {
		d := model.CanonicalDomain(raw)
		if !model.IsValidDomain(d) {
			if trimmed := strings.TrimSpace(raw); trimmed != "" {
				bad = append(bad, trimmed)
			}
			continue
		}
		if _, existed := seen[d]; existed {
			dup++
			continue
		}
		seen[d] = struct{}{}
		clean = append(clean, d)
	}

	// 写超时跟着域名数走。
	//
	// 每个域名要抓一次上游建基线，单域名超时 8 秒；500 个域名串行跑必然
	// 越过 http.Server 的 30 秒 WriteTimeout——实测 400 个域名跑了 39 秒，
	// 结果是「库里已经写进去 521 个，浏览器收到的是连接被重置」。
	// 用户看到失败、重试、于是又加一遍。这比慢更糟：它给了个错的结论。
	// 一个合法域名都没有：这是调用方把清单贴错了，不是「成功处理了 0 个」。
	// 回 200 + ok:null 会让脚本以为这批域名已经加进去了。
	if len(clean) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_domain",
			"域名清单里没有一个合法域名（示例：example.com）")
		return
	}

	const workers = 5
	if n := len(clean); n > 0 {
		defer extendWriteDeadline(w, bulkWindow(n, workers))()
	}

	// 并发处理，和批量查询同一个并发度上限（5）。
	//
	// 串行不只是慢：一批 500 个域名跑十几分钟，中途关掉页面就留下一半
	// 已加、一半没加的列表，而响应里什么都不说。
	var mu sync.Mutex
	var ok []string
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, d := range clean {
		wg.Add(1)
		go func(d string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if action == "add" {
				if err := s.addWatch(r, d); err != nil {
					mu.Lock()
					bad = append(bad, d)
					mu.Unlock()
					return
				}
			} else if err := s.svc.Store().DeleteWatch(d); err != nil {
				mu.Lock()
				bad = append(bad, d)
				mu.Unlock()
				return
			}
			mu.Lock()
			ok = append(ok, d)
			mu.Unlock()
		}(d)
	}
	wg.Wait()
	sort.Strings(ok)

	// 全失败时别回 200 + 空列表：那看起来像「成功了，只是没加进去」。
	if len(clean) > 0 && len(ok) == 0 {
		writeErr(w, http.StatusBadGateway, "bulk_failed",
			"全部域名都没处理成功，请检查上游连通性后重试")
		return
	}
	writeOK(w, map[string]any{
		"action": action, "added": len(ok), "failed": len(bad),
		"duplicated": dup, "ok": ok, "invalid": bad,
	})
}

// bulkWindow 估算批量加/移监控需要多久。
//
// 加监控要抓一次上游建基线（否则第一轮巡检会把全部字段误报成变更），
// 所以耗时按「域名数 / 并发度 × 单域名超时」算，再留一段余量。
// 上限 30 分钟：再长的话反代和浏览器早就先超时了，给一个跑不完的窗口
// 只是让请求方一直等。
func bulkWindow(n, workers int) time.Duration {
	const perDomain = 8 * time.Second // 上游默认超时
	if workers <= 0 {
		workers = 1
	}
	rounds := (n + workers - 1) / workers
	if rounds < 1 {
		rounds = 1
	}
	d := time.Duration(rounds)*perDomain + 30*time.Second
	if d < time.Minute {
		d = time.Minute
	}
	if d > 30*time.Minute {
		d = 30 * time.Minute
	}
	return d
}

// handleAdminWatchPurge 清空监控列表。
func (s *Server) handleAdminWatchPurge(w http.ResponseWriter, r *http.Request) {
	n, err := s.svc.Store().PurgeAllWatch()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeOK(w, map[string]any{"removed": n})
}

// handleAdminBackups 列出或清空 backups 目录里的快照文件。
func (s *Server) handleAdminBackups(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.purgeBackups(w, r)
		return
	}
	items := s.backupsRows()
	// 目录大小：备份是唯一会无限增长又没人清理的东西，
	// 后台得能一眼看出「这里占了 3 GB」。
	var total int64
	for _, it := range items {
		if v, ok := it["size"].(int64); ok {
			total += v
		}
	}
	writeOK(w, map[string]any{"dir": s.backupDir(), "total": len(items), "size_bytes": total, "items": items})
}

// purgeBackups 清空 backups 目录（只删 .db）。
func (s *Server) purgeBackups(w http.ResponseWriter, r *http.Request) {
	dir := s.backupDir()
	items, err := store.ListBackups(dir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	removed := 0
	for _, it := range items {
		// 只按 listBackups 的过滤结果删：它只认 .db，不会误伤同目录下
		// 用户自己放的东西（说明文件、脚本等）。
		if err := os.Remove(filepath.Join(dir, it.Name)); err == nil {
			removed++
		}
	}
	writeOK(w, map[string]any{"removed": removed})
}

// parseOffset 解析 offset，非法值明确报错。
func parseOffset(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, &offsetErr{raw: raw}
	}
	return n, nil
}

type offsetErr struct{ raw string }

func (e *offsetErr) Error() string {
	return "offset 必须是非负整数，收到 " + strconv.Quote(e.raw)
}
