package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/asoe/beian2api/internal/service"
)

// handleBadge 输出可直接嵌入 README 的 SVG 徽章：/badge/icp?domain=example.com
// 纯矢量、按内容自适应宽度、带缓存头，零外部依赖。
func (s *Server) handleBadge(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	label := r.URL.Query().Get("label")
	if label == "" {
		label = "ICP"
	}
	if domain == "" {
		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(badgeSVG(label, "missing domain", "#9ca3af")))
		return
	}
	res, err := s.svc.Query(r.Context(), domain, service.Options{})
	var value, color string
	switch {
	case err != nil:
		value, color = "error", "#ef4444"
	case res.Found && len(res.Records) > 0:
		value, color = res.Records[0].ICPCode, "#10b981"
		if value == "" {
			value, color = "已备案", "#10b981"
		}
	default:
		value, color = "未备案", "#f59e0b"
	}

	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=600")
	if res != nil && res.Cached {
		w.Header().Set("X-Cache", "HIT")
	}
	_, _ = w.Write([]byte(badgeSVG(label, value, color)))
}

// textWidth 按字符类型估算宽度：中文 11px，其余 6.6px（Verdana 11px 经验值）。
func textWidth(s string) int {
	w := 0.0
	for _, r := range s {
		if r > 0x2E80 {
			w += 11
		} else {
			w += 6.6
		}
	}
	return int(w + 0.5)
}

func badgeSVG(label, value, color string) string {
	lw := textWidth(label) + 12
	vw := textWidth(value) + 12
	total := lw + vw
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="%s: %s">
  <title>%s: %s</title>
  <defs>
    <linearGradient id="s" x2="0" y2="100%%">
      <stop offset="0" stop-color="#bbb" stop-opacity=".1"/>
      <stop offset="1" stop-opacity=".1"/>
    </linearGradient>
    <clipPath id="r"><rect width="%d" height="20" rx="4" fill="#fff"/></clipPath>
  </defs>
  <g clip-path="url(#r)">
    <rect width="%d" height="20" fill="#24292f"/>
    <rect x="%d" width="%d" height="20" fill="%s"/>
    <rect width="%d" height="20" fill="url(#s)"/>
  </g>
  <g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">
    <text x="%d" y="15" fill="#010101" fill-opacity=".3">%s</text>
    <text x="%d" y="14">%s</text>
    <text x="%d" y="15" fill="#010101" fill-opacity=".3">%s</text>
    <text x="%d" y="14">%s</text>
  </g>
</svg>`, total, esc(label), esc(value), esc(label), esc(value),
		total, lw, lw, vw, color, total,
		lw/2, esc(label), lw/2, esc(label),
		lw+vw/2, esc(value), lw+vw/2, esc(value))
}

func esc(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&#39;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// handleOpenAPI 输出 OpenAPI 3.0 描述，方便任何语言生成客户端。
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	base := s.cfg.BaseURL
	if base == "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	doc := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "Beian2API",
			"description": "轻量、高性能、可自托管的 ICP 备案查询 API。多源竞速 + 本地快照 + 变更巡检。",
			"version":     s.version,
		},
		"servers": []map[string]string{{"url": base}},
		"paths": map[string]any{
			"/api/v1/query": map[string]any{
				"get": map[string]any{
					"summary": "查询单个域名备案信息",
					"parameters": []map[string]any{
						{"name": "domain", "in": "query", "required": true, "schema": map[string]string{"type": "string"}, "example": "example.com"},
						{"name": "refresh", "in": "query", "schema": map[string]string{"type": "boolean"}},
					},
					"responses": map[string]any{"200": map[string]string{"description": "查询结果"}},
				},
			},
			"/api/v1/batch": map[string]any{
				"post": map[string]any{
					"summary": "批量查询（最多 50 个）",
					"requestBody": map[string]any{"content": map[string]any{"application/json": map[string]any{
						"schema": map[string]any{"type": "object", "properties": map[string]any{
							"domains": map[string]any{"type": "array", "items": map[string]string{"type": "string"}},
						}},
					}}},
					"responses": map[string]any{"200": map[string]string{"description": "批量结果"}},
				},
			},
			"/api/v1/history": map[string]any{"get": map[string]any{"summary": "备案历史快照与差异"}},
			"/api/v1/diff":    map[string]any{"get": map[string]any{"summary": "最近两次快照对比"}},
			"/api/v1/watch":   map[string]any{"post": map[string]any{"summary": "加入监控"}, "delete": map[string]any{"summary": "取消监控"}},
			"/badge/icp":      map[string]any{"get": map[string]any{"summary": "SVG 徽章"}},
			"/healthz":        map[string]any{"get": map[string]any{"summary": "健康检查"}},
		},
	}
	writeJSON(w, http.StatusOK, doc)
}
