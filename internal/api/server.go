// Package api 暴露 HTTP 接口：JSON API、SVG 徽章、内置文档页。
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/asoe/beian2api/internal/config"
	"github.com/asoe/beian2api/internal/notify"
	"github.com/asoe/beian2api/internal/service"
	"github.com/asoe/beian2api/internal/watch"
)

// Server 承载路由与依赖。
type Server struct {
	cfg            *config.Config
	svc            *service.Service
	watch          *watch.Watcher
	notifier       *notify.Dispatcher
	baseURL        string
	log            *slog.Logger
	startAt        time.Time
	version        string
	limiter        *rateLimiter
	keys           []string
	origins        []string
	allowAnyOrigin bool
}

// Options 构造 Server 所需参数。
type Options struct {
	Config  *config.Config
	Service *service.Service
	Watch   *watch.Watcher
	Notify  *notify.Dispatcher
	BaseURL string
	Log     *slog.Logger
	Version string
}

// New 构造 Server。
func New(o Options) *Server {
	s := &Server{
		cfg: o.Config, svc: o.Service, watch: o.Watch, notifier: o.Notify, baseURL: o.BaseURL, log: o.Log,
		startAt: time.Now(), version: o.Version,
		keys: o.Config.Auth.Keys, origins: o.Config.CORS,
	}
	if len(s.keys) > 0 && !o.Config.Auth.Enabled {
		s.keys = nil
	}
	if o.Config.RateLimit.Enabled {
		s.limiter = newRateLimiter(o.Config.RateLimit.RPS, o.Config.RateLimit.Burst)
	}
	for _, o := range s.origins {
		if o == "*" {
			s.allowAnyOrigin = true
		}
	}
	return s
}

// Handler 返回完整路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 业务接口
	mux.HandleFunc("/api/v1/query", s.withRateLimit(s.withAuth(s.handleQuery)))
	mux.HandleFunc("/api/v1/batch", s.withRateLimit(s.withAuth(s.handleBatch)))
	mux.HandleFunc("/api/v1/history", s.withRateLimit(s.withAuth(s.handleHistory)))
	mux.HandleFunc("/api/v1/diff", s.withRateLimit(s.withAuth(s.handleDiff)))
	mux.HandleFunc("/api/v1/watch", s.withRateLimit(s.withAuth(s.handleWatch)))
	mux.HandleFunc("/api/v1/watch/run", s.withRateLimit(s.withAuth(s.handleWatchRun)))
	mux.HandleFunc("/api/v1/watch/list", s.withRateLimit(s.withAuth(s.handleWatchList)))
	mux.HandleFunc("/api/v1/watch/status", s.withRateLimit(s.withAuth(s.handleWatchStatus)))
	mux.HandleFunc("/api/v1/stats", s.withRateLimit(s.withAuth(s.handleStats)))
	mux.HandleFunc("/api/v1/providers", s.withRateLimit(s.withAuth(s.handleProviders)))
	mux.HandleFunc("/api/v1/changes", s.withRateLimit(s.withAuth(s.handleChanges)))
	// RSS 也要过鉴权：它列的是「你在监控哪些域名」，这本身就是一份情报。
	// 之前这里没包 withAuth，开了 API Key 的部署，变更流水照样对全网公开。
	// RSS 阅读器不方便带请求头，但 withAuth 认 ?key=，订阅地址里带上即可。
	mux.HandleFunc("/api/v1/changes.rss", s.withRateLimit(s.withAuth(s.handleRSS)))
	mux.HandleFunc("/api/v1/notify/status", s.withRateLimit(s.withAuth(s.handleNotifyStatus)))
	mux.HandleFunc("/api/v1/notify/test", s.withRateLimit(s.withAuth(s.handleNotifyTest)))
	mux.HandleFunc("/api/v1/verify", s.withRateLimit(s.withAuth(s.handleVerify)))
	mux.HandleFunc("/api/v1/admin/backup", s.withRateLimit(s.withAuth(s.handleAdminBackup)))
	mux.HandleFunc("/api/v1/openapi.json", s.handleOpenAPI)

	// 徽章与运维。
	//
	// 徽章必须公开（README 里的 <img> 带不了请求头），但正因为公开，
	// 它不能裸奔：每个请求都可能触发一次回源，等于给全网开了一个
	// 「填任意域名就能让服务器去查一次上游」的放大器，而它又不占鉴权、
	// 不限流——之前实测连刷 120 次全部 200，同时 /api/v1/query 已经 429。
	mux.HandleFunc("/badge/icp.svg", s.withRateLimit(s.handleBadge))
	mux.HandleFunc("/badge/icp", s.withRateLimit(s.handleBadge))
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)

	// 静态站点
	s.mountStatic(mux)

	return s.withCommon(s.logRequests(mux))
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		if s.cfg.LogLevel == "debug" || (s.cfg.LogLevel == "info" && strings.HasPrefix(r.URL.Path, "/api/")) {
			s.log.Debug("http",
				"method", r.Method, "path", r.URL.Path, "status", sw.code,
				"ms", float64(time.Since(start).Microseconds())/1000, "ip", s.clientIP(r))
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

// Unwrap 让 http.ResponseController 能穿透这一层包装拿到真实连接。
// 少了这个方法，SetWriteDeadline 会静默失效——长任务照样被 30 秒掐断。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) WriteHeader(c int) {
	w.code = c
	w.ResponseWriter.WriteHeader(c)
}

// envelope 是统一响应外壳。
type envelope struct {
	OK    bool    `json:"ok"`
	Data  any     `json:"data,omitempty"`
	Error *errObj `json:"error,omitempty"`
	Cost  float64 `json:"cost_ms,omitempty"`
}

type errObj struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeOK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, envelope{OK: true, Data: data})
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, envelope{OK: false, Error: &errObj{Code: code, Message: msg}})
}

// extendWriteDeadline 把一个长任务的响应写超时往后推。
//
// http.Server 的 WriteTimeout 是 30 秒，它从**读完请求头**就开始计时，
// 覆盖整个 handler 的执行。而「立即巡检」的窗口可以被估到几十分钟、
// 在线备份要拷整个数据文件、推送测试要等最多 30 秒——这些接口在
// 监控域名稍多一点时必然跑过 30 秒，响应直接写不回去，浏览器侧看到的是
// 连接被掐断，而服务端日志里一片成功。前端「巡检中…」永远转下去，
// 用户以为点到没反应，其实巡检跑完了、结果没人收到。
//
// 用 http.ResponseController 只调这一个请求的写超时，不动全局值。
// 返回的 cancel 在 handler 结束时把超时恢复，避免影响连接复用的后续请求。
func extendWriteDeadline(w http.ResponseWriter, d time.Duration) func() {
	rc := http.NewResponseController(w)
	prevErr := rc.SetWriteDeadline(time.Now().Add(d))
	if prevErr != nil {
		// 不支持 SetWriteDeadline（比如被中间件包了几层又没实现 Unwrap）
		// 时不算致命：照常执行，只是超时还是全局值。
		return func() {}
	}
	return func() { _ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second)) }
}
