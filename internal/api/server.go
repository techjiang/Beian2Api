// Package api 暴露 HTTP 接口：JSON API、SVG 徽章、内置文档页。
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/asoe/beian2api/internal/config"
	"github.com/asoe/beian2api/internal/service"
	"github.com/asoe/beian2api/internal/watch"
)

// Server 承载路由与依赖。
type Server struct {
	cfg            *config.Config
	svc            *service.Service
	watch          *watch.Watcher
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
	Log     *slog.Logger
	Version string
}

// New 构造 Server。
func New(o Options) *Server {
	s := &Server{
		cfg: o.Config, svc: o.Service, watch: o.Watch, log: o.Log,
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
	mux.HandleFunc("/api/v1/changes.rss", s.handleRSS)
	mux.HandleFunc("/api/v1/verify", s.withRateLimit(s.withAuth(s.handleVerify)))
	mux.HandleFunc("/api/v1/admin/backup", s.withRateLimit(s.withAuth(s.handleAdminBackup)))
	mux.HandleFunc("/api/v1/openapi.json", s.handleOpenAPI)

	// 徽章与运维
	mux.HandleFunc("/badge/icp.svg", s.handleBadge)
	mux.HandleFunc("/badge/icp", s.handleBadge)
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
				"ms", float64(time.Since(start).Microseconds())/1000, "ip", clientIP(r))
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

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
