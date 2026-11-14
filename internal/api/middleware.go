package api

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rateLimiter 是每 IP 的令牌桶，零依赖、无后台 goroutine（惰性补充）。
type rateLimiter struct {
	rps    float64
	burst  float64
	mu     sync.Mutex
	bucket map[string]*token
	lastGC int64
}

type token struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(rps, burst int) *rateLimiter {
	return &rateLimiter{rps: float64(rps), burst: float64(burst), bucket: make(map[string]*token), lastGC: time.Now().Unix()}
}

// allow 判断某 IP 此刻是否放行。
func (l *rateLimiter) allow(ip string) bool {
	if l == nil || l.rps <= 0 {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.bucket[ip]
	if !ok {
		t = &token{tokens: l.burst, last: now}
		l.bucket[ip] = t
	}
	t.tokens += now.Sub(t.last).Seconds() * l.rps
	if t.tokens > l.burst {
		t.tokens = l.burst
	}
	t.last = now
	if t.tokens < 1 {
		return false
	}
	t.tokens--
	// 每 5 分钟清理一次长期不活跃的桶，防止内存增长。
	if now.Unix()-l.lastGC > 300 {
		for k, v := range l.bucket {
			if now.Sub(v.last) > 10*time.Minute {
				delete(l.bucket, k)
			}
		}
		l.lastGC = now.Unix()
	}
	return true
}

// clientIP 取客户端 IP。
//
// 只有明确配置 trust_proxy 时才看转发头。这些头是客户端可以随便填的：
// 无条件采信就等于让攻击者自己选限流的 key，每个请求换一个头就换一桶令牌。
//
// 即使开了 trust_proxy，也要取 X-Forwarded-For 的**最后一段**，不能取第一段。
// 头是链式的：反向代理把「它看到的对端地址」**追加**到末尾
// （nginx 的 $proxy_add_x_forwarded_for 就是 append）。也就是说：
//
//	X-Forwarded-For: <客户端自己填的任意内容>, ..., <真实客户端 IP>
//
// 第一段是攻击者写的，最后一段才是最近一跳代理亲手写上去的。
// 之前取的是第一段，于是开了 trust_proxy 的部署里，随便填一个
// `X-Forwarded-For: 127.0.0.1` 就能：换一桶新的限流令牌（限流等于没有）、
// 并且让未配 API Key 的在线备份接口误判为「本机直连」而放行（能写文件）。
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			if ip := validHop(lastHop(v)); ip != "" {
				return ip
			}
		}
		if ip := validHop(r.Header.Get("X-Real-IP")); ip != "" {
			return ip
		}
	}
	// 不再单独采信 CF-Connecting-IP。
	//
	// 它看起来更权威，但只要 origin 还没限制成「只接受 Cloudflare 的 IP」，
	// 任何人直连端口随手写上这一个头就换到了新桶——那正是上面刚堵掉的口子，
	// 换个头名再开一遍没有意义。跑在 Cloudflare 后面的正确做法是在 nginx 里
	// `set_real_ip_from` + `real_ip_header CF-Connecting-IP`，让 $remote_addr
	// 变成真实客户端 IP，X-Forwarded-For 的末段自然就是它（deploy/nginx.conf 有注释）。
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// lastHop 取逗号分隔列表里最后一个非空项。
//
// 空项要跳过：`"1.2.3.4, "` 这种带尾随逗号的头在真实流量里很常见，
// 直接 TrimSpace 会得到空串，客户端就变成「没有 IP」——限流按空 key 分桶，
// 所有这类请求又挤回同一个桶里。
func lastHop(v string) string {
	parts := strings.Split(v, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		if ip := strings.TrimSpace(parts[i]); ip != "" {
			return ip
		}
	}
	return ""
}

// validHop 只接受「真的像个地址」的值，其余一律当没有。
//
// 取末段之后还剩一个口子：末段本身是字符串，不是 IP。`X-Real-IP` 更是完全
// 没校验过。于是每个请求带一个不同的乱码值，就是一桶新的限流令牌——限流又
// 白开了；更糟的是这些值会原样成为 limiter 那个 map 的 key，随请求量无界增长，
// 鼓包两百字节一个 key，发几十万个请求就把内存吃干净。它们还会进访问日志。
//
// 反向代理写进来的 `$remote_addr` 永远是 IP，所以「不是 IP 就不认」不会误伤
// 任何正常部署。带端口的写法（`1.2.3.4:5678`、`[::1]:80`）也接受。
func validHop(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 64 {
		return ""
	}
	if ip := net.ParseIP(v); ip != nil {
		return ip.String()
	}
	if host, _, err := net.SplitHostPort(v); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
	}
	return ""
}

// withRateLimit 包装限流。
func (s *Server) withRateLimit(next http.HandlerFunc) http.HandlerFunc {
	if s.limiter == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// 健康检查与静态资源不限流。
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/badge/") {
			next(w, r)
			return
		}
		ip := s.clientIP(r)
		if !s.limiter.allow(ip) {
			w.Header().Set("Retry-After", "1")
			writeErr(w, http.StatusTooManyRequests, "rate_limited", "请求过于频繁，请稍后再试")
			return
		}
		next(w, r)
	}
}

// withAuth 是可选的 API Key 校验；key 可通过 header 或 query 传递。
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	if len(s.keys) == 0 {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if isPublicPath(r.URL.Path) {
			next(w, r)
			return
		}
		key := r.Header.Get("X-API-Key")
		if key == "" {
			key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if key == "" {
			key = r.URL.Query().Get("key")
		}
		if !s.validKey(key) {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "缺少或无效的 API Key")
			return
		}
		next(w, r)
	}
}

func (s *Server) validKey(key string) bool {
	if key == "" {
		return false
	}
	for _, k := range s.keys {
		if subtle.ConstantTimeCompare([]byte(k), []byte(key)) == 1 {
			return true
		}
	}
	return false
}

func isPublicPath(p string) bool {
	switch p {
	case "/", "/healthz", "/readyz", "/api/v1/openapi.json", "/docs", "/favicon.ico", "/logo.svg", "/apple-touch-icon.png":
		return true
	}
	return strings.HasPrefix(p, "/assets/") || strings.HasPrefix(p, "/badge/") || strings.HasPrefix(p, "/docs/")
}

// withCommon 统一附加通用响应头：CORS、请求追踪、内容安全。
func (s *Server) withCommon(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		if s.allowAnyOrigin {
			h.Set("Access-Control-Allow-Origin", "*")
		} else if origin := r.Header.Get("Origin"); origin != "" && s.originAllowed(origin) {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Vary", "Origin")
		}
		h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, Authorization")
		h.Set("Access-Control-Max-Age", "86400")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Request-Id", strconv.FormatInt(time.Now().UnixNano(), 36))
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) originAllowed(origin string) bool {
	for _, o := range s.origins {
		if o == "*" || o == origin {
			return true
		}
	}
	return false
}
