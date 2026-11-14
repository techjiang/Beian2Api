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
// 只有明确配置 trust_proxy 时才看 X-Forwarded-For / X-Real-IP。
// 这几个头是客户端可以随便填的：无条件采信就等于让攻击者自己选限流的 key，
// 每个请求换一个头就换一桶令牌，限流形同虚设。
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if v := r.Header.Get("CF-Connecting-IP"); v != "" {
			return v
		}
		if v := r.Header.Get("X-Real-IP"); v != "" {
			return v
		}
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			if i := strings.IndexByte(v, ','); i > 0 {
				return strings.TrimSpace(v[:i])
			}
			return strings.TrimSpace(v)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
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
