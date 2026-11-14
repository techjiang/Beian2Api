package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/asoe/beian2api/internal/store"
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
	// since 写错不能当成 0 静默放过：`since=abc` 被当成「从头拉」，
	// 调用方用它做增量同步时会以为「这些全是新的」，把早先的变更重复处理一遍。
	// limit 超出上限都返回 400 了，since 解析失败更没理由装作无事。
	since, sinceErr := parseSince(r.URL.Query().Get("since"))
	if sinceErr != nil {
		writeErr(w, http.StatusBadRequest, "invalid_since", sinceErr.Error())
		return
	}

	offset := 0
	if v := strings.TrimSpace(r.URL.Query().Get("offset")); v != "" {
		n, convErr := strconv.Atoi(v)
		if convErr != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "invalid_offset", fmt.Sprintf("offset 必须是非负整数，收到 %q", v))
			return
		}
		offset = n
	}
	// kind 让调用方按类型取（只看注销、只看上游异常）。
	// 之前只能把流水全拉下来自己过滤，而流水本身是有截断的：
	// 「只看注销」在末尾几条里找不到时，界面上就是一片空白，
	// 用户没法知道是「真的没有」还是「没翻到」。
	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind")))
	if kind != "" && !knownKind(kind) {
		writeErr(w, http.StatusBadRequest, "invalid_kind",
			fmt.Sprintf("kind 只能是 changed / revoked / registered / upstream_error / recovered，收到 %q", kind))
		return
	}

	// 多要一条用来判断「后面还有没有」：这样不必为了算总数把整条流水
	// 反序列化一遍（那是每 60 秒一次的轮询接口）。
	raws, err := s.svc.Store().PageChanges(limit+1, offset, func(raw []byte) (bool, error) {
		var c watch.Change
		if json.Unmarshal(raw, &c) != nil {
			return false, nil
		}
		if since > 0 && c.At < since {
			// 变更流水的 key 是「时间戳-域名」，遍历本身是新的在前：
			// 读到第一条早于游标的记录，后面全是更旧的，没必要再扫。
			// 增量同步（最常见的用法）本来就是「拉最近几条」，
			// 没有早停的话每次都要把整个流水反序列化一遍。
			return false, store.ErrStop
		}
		if kind != "" && c.Kind != kind {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	hasMore := len(raws) > limit
	if hasMore {
		raws = raws[:limit]
	}
	out := make([]watch.Change, 0, len(raws))
	for _, raw := range raws {
		var c watch.Change
		if json.Unmarshal(raw, &c) == nil {
			out = append(out, c)
		}
	}
	if out == nil {
		out = []watch.Change{}
	}
	// total 仍然是「本次返回的条数」，保持老调用方不用改；
	// 翻页信息放在 offset / limit / has_more 三个字段里。
	writeOK(w, map[string]any{
		"total": len(out), "items": out,
		"offset": offset, "limit": limit, "has_more": hasMore,
	})
}

// knownKind 是变更事件的合法类型，与 watch / notify 的常量保持一致。
func knownKind(k string) bool {
	switch k {
	case "changed", "revoked", "registered", "upstream_error", "recovered":
		return true
	}
	return false
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
	//
	// 空 feed 同理，但上一版只修了「有事件」那一支：没有事件时仍然回落到
	// time.Now()，于是「一条记录都没有」的这个 feed 反而每次请求都在变——
	// 比修之前更容易被反复拉。空 feed 干脆不输出 lastBuildDate：
	// RFC 822 里它是可选项，缺失的正确含义就是「没有内容更新过」。
	if len(items) > 0 {
		fmt.Fprintf(&b, "<lastBuildDate>%s</lastBuildDate>\n", time.Unix(items[0].At, 0).Format(time.RFC1123Z))
	}
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

// parseSince 解析增量游标（Unix 秒），空值表示不限制。
func parseSince(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("since 必须是 Unix 时间戳（秒），收到 %q", raw)
	}
	if n < 0 {
		return 0, fmt.Errorf("since 不能为负数")
	}
	return n, nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
