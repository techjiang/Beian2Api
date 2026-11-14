package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/asoe/beian2api/internal/watch"
)

// handleChanges 返回最近记录的备案变更事件。
func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	// limit 超上限时不能静默改成默认值：调用方写 limit=500 收到 50 条，
	// 分页逻辑就会以为「只有 50 条」，剩下的永远拉不到。
	limit, err := parseLimit(r.URL.Query().Get("limit"), 50, 200)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)

	var out []watch.Change
	err = s.svc.Store().ListChanges(limit, func(raw []byte) (bool, error) {
		var c watch.Change
		if json.Unmarshal(raw, &c) != nil {
			return false, nil
		}
		if since > 0 && c.At < since {
			return false, nil
		}
		out = append(out, c)
		return true, nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if out == nil {
		out = []watch.Change{}
	}
	writeOK(w, map[string]any{"total": len(out), "items": out})
}

// handleRSS 把变更事件输出成 RSS 2.0，可直接丢进 RSS 阅读器或 IM 的订阅机器人。
func (s *Server) handleRSS(w http.ResponseWriter, r *http.Request) {
	base := s.cfg.BaseURL
	if base == "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}

	var items []watch.Change
	_ = s.svc.Store().ListChanges(100, func(raw []byte) (bool, error) {
		var c watch.Change
		if json.Unmarshal(raw, &c) != nil {
			return false, nil
		}
		items = append(items, c)
		return true, nil
	})

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<rss version="2.0"><channel>` + "\n")
	fmt.Fprintf(&b, "<title>Beian2API 备案变更</title>\n<link>%s</link>\n<description>被监控域名的备案信息变化</description>\n", esc(base))
	// lastBuildDate 用最后一条事件的时间，不是 time.Now()。
	// 每次请求都把「构建时间」刷新成当前时刻，等于告诉阅读器「内容刚变过」，
	// 于是一个永远不会变的 feed 也会被反复拉取。
	built := time.Now()
	if len(items) > 0 {
		built = time.Unix(items[0].At, 0)
	}
	fmt.Fprintf(&b, "<lastBuildDate>%s</lastBuildDate>\n", built.Format(time.RFC1123Z))
	for _, c := range items {
		fmt.Fprintf(&b, "<item><title>%s 备案%s</title>\n", esc(c.Domain), esc(kindWord(c.Kind)))
		fmt.Fprintf(&b, "<link>%s/api/v1/history?domain=%s</link>\n", esc(base), esc(c.Domain))
		fmt.Fprintf(&b, "<guid isPermaLink=\"false\">%s-%d</guid>\n", esc(c.Domain), c.At)
		fmt.Fprintf(&b, "<pubDate>%s</pubDate>\n", time.Unix(c.At, 0).Format(time.RFC1123Z))
		fmt.Fprintf(&b, "<description>%s</description></item>\n", esc(diffText(c)))
	}
	b.WriteString("</channel></rss>\n")

	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write([]byte(b.String()))
}

func diffText(c watch.Change) string {
	if len(c.Diff) == 0 {
		return "备案号：" + c.ICPCode
	}
	parts := make([]string, 0, len(c.Diff))
	for _, d := range c.Diff {
		parts = append(parts, fmt.Sprintf("%s：%s → %s", d.Field, dash(d.From), dash(d.To)))
	}
	return strings.Join(parts, "；")
}

func kindWord(kind string) string {
	switch kind {
	case "revoked":
		return "注销"
	case "registered":
		return "新增"
	case "upstream_error":
		return "上游异常"
	case "recovered":
		return "已恢复"
	default:
		return "变更"
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
