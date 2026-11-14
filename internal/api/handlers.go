package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/asoe/beian2api/internal/model"
	"github.com/asoe/beian2api/internal/service"
	"github.com/asoe/beian2api/internal/store"
)

// handleQuery 处理单域名查询。
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 GET / POST")
		return
	}
	var domain string
	var refresh bool
	if r.Method == http.MethodGet {
		domain = r.URL.Query().Get("domain")
		refresh = truthy(r.URL.Query().Get("refresh"))
	} else {
		var body struct {
			Domain  string `json:"domain"`
			Refresh bool   `json:"refresh"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
			return
		}
		domain, refresh = body.Domain, body.Refresh
	}
	if strings.TrimSpace(domain) == "" {
		writeErr(w, http.StatusBadRequest, "missing_domain", "缺少 domain 参数")
		return
	}
	res, err := s.svc.Query(r.Context(), domain, service.Options{Refresh: refresh})
	if err != nil {
		s.writeServiceErr(w, err)
		return
	}
	writeOK(w, res)
}

// handleBatch 处理批量查询，支持 GET 逗号分隔与 POST JSON 数组。
func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	var domains []string
	var refresh bool
	if r.Method == http.MethodGet {
		domains = strings.Split(r.URL.Query().Get("domains"), ",")
		refresh = truthy(r.URL.Query().Get("refresh"))
	} else if r.Method == http.MethodPost {
		var body struct {
			Domains []string `json:"domains"`
			Refresh bool     `json:"refresh"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
			return
		}
		domains, refresh = body.Domains, body.Refresh
	} else {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 GET / POST")
		return
	}

	clean := make([]string, 0, len(domains))
	seen := map[string]struct{}{}
	for _, d := range domains {
		n := model.NormalizeDomain(d)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		clean = append(clean, n)
	}
	if len(clean) == 0 {
		writeErr(w, http.StatusBadRequest, "missing_domain", "缺少 domains 参数")
		return
	}
	if len(clean) > 50 {
		writeErr(w, http.StatusBadRequest, "too_many_domains", "单次最多 50 个域名")
		return
	}
	items := s.svc.Batch(r.Context(), clean, service.Options{Refresh: refresh}, 5)
	writeOK(w, map[string]any{"total": len(items), "items": items})
}

// handleHistory 返回某域名的历史快照与变化差异。
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	recs, err := s.svc.History(domain, limit)
	if err != nil {
		s.writeServiceErr(w, err)
		return
	}
	type node struct {
		Record model.Record          `json:"record"`
		Diff   []service.FieldChange `json:"diff,omitempty"`
	}
	out := make([]node, 0, len(recs))
	for i := range recs {
		n := node{Record: recs[i]}
		if i > 0 {
			n.Diff = service.DiffRecords(&recs[i-1], &recs[i])
		}
		out = append(out, n)
	}
	writeOK(w, map[string]any{
		"domain":  model.NormalizeDomain(domain),
		"total":   len(out),
		"history": out,
	})
}

// handleDiff 对比同一域名的两条历史快照。
func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	recs, err := s.svc.History(domain, 0)
	if err != nil {
		s.writeServiceErr(w, err)
		return
	}
	if len(recs) < 2 {
		writeOK(w, map[string]any{
			"domain":  model.NormalizeDomain(domain),
			"changed": false,
			"message": "历史快照不足两条，暂无可对比的变化",
		})
		return
	}
	a := recs[len(recs)-2]
	b := recs[len(recs)-1]
	writeOK(w, map[string]any{
		"domain":   model.NormalizeDomain(domain),
		"changed":  len(service.DiffRecords(&a, &b)) > 0,
		"previous": a,
		"current":  b,
		"diff":     service.DiffRecords(&a, &b),
	})
}

// handleWatch 新增或删除监控域名。
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var body struct {
			Domain string `json:"domain"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
			return
		}
		d := model.NormalizeDomain(body.Domain)
		if !model.IsValidDomain(d) {
			writeErr(w, http.StatusBadRequest, "invalid_domain", "域名格式不正确")
			return
		}
		// 首次加入时抓取一次建立基线，否则第一次巡检会把"全部字段"误报成变更。
		_, _ = s.svc.Query(r.Context(), d, service.Options{})

		now := model.Now()
		item := &store.WatchItem{Domain: d, AddedAt: now, CheckedAt: now}
		// 重复添加时保留已有的基线、计数与加入时间，只刷新抓取时间，
		// 否则用户手滑点两次就把变更计数清零了。
		if old, err := s.svc.Store().GetWatch(d); err == nil {
			item.AddedAt = old.AddedAt
			item.Changes = old.Changes
			item.LastFP = old.LastFP
			item.LastICP = old.LastICP
		}
		if item.LastFP == "" {
			if hist, _ := s.svc.History(d, 1); len(hist) > 0 {
				item.LastFP = hist[len(hist)-1].Fingerprint()
				item.LastICP = hist[len(hist)-1].ICPCode
			}
		}
		if err := s.svc.Store().PutWatch(item); err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		writeOK(w, map[string]any{"domain": d, "watching": true, "baseline_icp": item.LastICP})
	case http.MethodDelete:
		d := model.NormalizeDomain(r.URL.Query().Get("domain"))
		if d == "" {
			writeErr(w, http.StatusBadRequest, "missing_domain", "缺少 domain 参数")
			return
		}
		if err := s.svc.Store().DeleteWatch(d); err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		writeOK(w, map[string]any{"domain": d, "watching": false})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST / DELETE")
	}
}

// handleWatchList 列出监控项。
func (s *Server) handleWatchList(w http.ResponseWriter, r *http.Request) {
	items, err := s.svc.Store().ListWatch()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if items == nil {
		items = nil
	}
	writeOK(w, map[string]any{"total": len(items), "items": items, "interval": s.cfg.Watch.Interval})
}

// handleWatchRun 立即触发一轮巡检。
func (s *Server) handleWatchRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST")
		return
	}
	if s.watch == nil {
		writeErr(w, http.StatusServiceUnavailable, "watch_disabled", "巡检功能未启用")
		return
	}
	// 不能直接把请求的 ctx 交给巡检：浏览器关掉页面、代理超时都会取消它，
	// 一轮巡检就停在中途——域名各自写了一半状态，基线也可能停在半新半旧。
	// 巡检该跑完，请求方断连只影响它自己拿不到结果。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	changes := s.watch.RunOnce(ctx)
	writeOK(w, map[string]any{"changes": changes, "count": len(changes)})
}

// handleWatchStatus 返回巡检概况：是否在跑、上一轮何时、下一轮何时。
func (s *Server) handleWatchStatus(w http.ResponseWriter, r *http.Request) {
	if s.watch == nil {
		writeOK(w, map[string]any{
			"enabled": false, "total": 0, "interval": 0,
			"running": false, "last_run_at": 0, "last_count": 0, "next_run_at": 0,
			"items": []any{},
		})
		return
	}
	writeOK(w, s.watch.Status(s.cfg.Watch.Enabled))
}

// handleStats 返回运行统计。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.Store().Count()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	hits := s.svc.Store().Stat("hits")
	misses := s.svc.Store().Stat("misses")
	stale := s.svc.Store().Stat("stale")
	total := hits + misses
	rate := 0.0
	if total > 0 {
		rate = float64(hits) * 100 / float64(total)
	}
	writeOK(w, map[string]any{
		"version":    s.version,
		"uptime_sec": int(time.Since(s.startAt).Seconds()),
		"providers":  s.cfg.EnabledProviders(),
		"counts":     c,
		"requests": map[string]any{
			"cache_hits":      hits,
			"cache_misses":    misses,
			"stale_fallbacks": stale,
			"hit_rate":        fmt.Sprintf("%.1f%%", rate),
		},
		"data_file": s.svc.Store().Path(),
	})
}

// handleProviders 返回各数据源状态。
func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
		Weight  int    `json:"weight"`
		Timeout int    `json:"timeout"`
	}
	out := make([]item, 0, len(s.cfg.Providers))
	for _, p := range s.cfg.Providers {
		out = append(out, item{Name: p.Name, Enabled: p.Enabled && p.Weight > 0, Weight: p.Weight, Timeout: p.Timeout})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Weight > out[j].Weight })
	writeOK(w, map[string]any{"providers": out})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"status":  "healthy",
		"version": s.version,
		"uptime":  int(time.Since(s.startAt).Seconds()),
		"time":    time.Now().Format(time.RFC3339),
	})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if _, err := s.svc.Store().Count(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "status": "storage unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "ready"})
}

func (s *Server) writeServiceErr(w http.ResponseWriter, err error) {
	switch {
	case strings.Contains(err.Error(), service.ErrInvalidDomain.Error()):
		writeErr(w, http.StatusBadRequest, "invalid_domain", "域名格式不正确，示例：example.com")
	default:
		writeErr(w, http.StatusBadGateway, "upstream_unavailable", err.Error())
	}
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(v)
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "y", "on":
		return true
	}
	return false
}
