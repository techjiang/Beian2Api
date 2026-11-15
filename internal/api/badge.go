package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/asoe/beian2api/internal/model"
	"github.com/asoe/beian2api/internal/service"
)

// handleBadge 输出可直接嵌入 README 的 SVG 徽章：/badge/icp?domain=example.com
// 纯矢量、按内容自适应宽度、带缓存头，零外部依赖。
func (s *Server) handleBadge(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	label := strings.TrimSpace(r.URL.Query().Get("label"))
	if label == "" {
		label = "ICP"
	}
	// 标签长度封顶。
	//
	// 徽章是公开接口（README 里的 <img> 带不了请求头），而 label 由 URL 决定。
	// 不封顶时 `?label=5000 个字符` 会生成一个三万多像素宽、20KB 的 SVG——
	// 一次请求就能让别人家的 README 页面横向滚到天边，而这张图还是从你的
	// 服务器上吐出去的。徽章本来只放得下一两个词（「ICP」「备案」），
	// 24 个字符足够任何真实用法，超出的部分截断并加省略号，
	// 而不是把宽度算到离谱。
	label = clip(label, 24)
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
		// 与巡检/历史用同一条代表记录：一个域名有多条备案时，
		// 徽章显示哪条、巡检比对哪条必须是同一个答案。
		if p, ok := model.Primary(res.Records); ok {
			value = p.ICPCode
		}
		color = "#10b981"
		if value == "" {
			value, color = "已备案", "#10b981"
		}
	default:
		value, color = "未备案", "#f59e0b"
	}

	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	// 失败态不进缓存。
	//
	// 之前无论结果如何都回 `public, max-age=600`：上游抖一下，徽章变成
	// 「ICP: error」，然后这个红图被浏览器和 CDN 钉死 10 分钟——上游早就
	// 恢复了，所有嵌着它的 README 还在红色报错，而且清不掉（不是你自己的
	// 浏览器缓存，是别人 CDN 上的）。徽章是给别人看的第一眼，
	// 它必须比后端更早知道「现在已经好了」。
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=600")
	}
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

// clip 按字符（不是字节）截断过长的文本，截断处补省略号。
//
// 按 rune 是为了不把「备案」这种多字节字符劈成半个——那会渲染成一个
// 替换字符，看着像字体坏了或者编码错了，而原因只是截断位置不对。
func clip(s string, max int) string {
	if max <= 0 {
		return ""
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i] + "…"
		}
		n++
	}
	return s
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
