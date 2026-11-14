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

	// 业务接口。只读接口显式声明只接受 GET/HEAD：
	// 之前没有任何方法检查，`DELETE /api/v1/stats` 会返回 200 和完整统计，
	// 客户端会以为自己真的删掉了什么。allowMethods 把这件事变成 405 + Allow 头。
	read := func(h http.HandlerFunc) http.HandlerFunc {
		return s.withRateLimit(s.withAuth(allowMethods(h, http.MethodGet)))
	}
	readWrite := func(h http.HandlerFunc) http.HandlerFunc {
		return s.withRateLimit(s.withAuth(allowMethods(h, http.MethodGet, http.MethodPost)))
	}
	mux.HandleFunc("/api/v1/query", readWrite(s.handleQuery))
	mux.HandleFunc("/api/v1/batch", readWrite(s.handleBatch))
	mux.HandleFunc("/api/v1/history", read(s.handleHistory))
	mux.HandleFunc("/api/v1/diff", read(s.handleDiff))
	// watch 同时接受 POST（加入）与 DELETE（移出）。
	mux.HandleFunc("/api/v1/watch", s.withRateLimit(s.withAuth(allowMethods(s.handleWatch, http.MethodPost, http.MethodDelete))))
	mux.HandleFunc("/api/v1/watch/run", s.withRateLimit(s.withAuth(allowMethods(s.handleWatchRun, http.MethodPost))))
	mux.HandleFunc("/api/v1/watch/list", read(s.handleWatchList))
	mux.HandleFunc("/api/v1/watch/status", read(s.handleWatchStatus))
	mux.HandleFunc("/api/v1/stats", read(s.handleStats))
	mux.HandleFunc("/api/v1/providers", read(s.handleProviders))
	mux.HandleFunc("/api/v1/changes", read(s.handleChanges))
	// RSS 也要过鉴权：它列的是「你在监控哪些域名」，这本身就是一份情报。
	// 之前这里没包 withAuth，开了 API Key 的部署，变更流水照样对全网公开。
	// RSS 阅读器不方便带请求头，但 withAuth 认 ?key=，订阅地址里带上即可。
	mux.HandleFunc("/api/v1/changes.rss", read(s.handleRSS))
	mux.HandleFunc("/api/v1/notify/status", read(s.handleNotifyStatus))
	mux.HandleFunc("/api/v1/notify/test", s.withRateLimit(s.withAuth(allowMethods(s.handleNotifyTest, http.MethodPost))))
	mux.HandleFunc("/api/v1/verify", read(s.handleVerify))
	mux.HandleFunc("/api/v1/admin/backup", s.withRateLimit(s.withAuth(
		allowMethods(s.handleAdminBackup, http.MethodGet, http.MethodPost))))
	mux.HandleFunc("/api/v1/openapi.json", allowMethods(s.handleOpenAPI, http.MethodGet))

	// 后台管理。方法与常规读接口一样显式声明，破坏性操作额外过 withAdmin：
	// 配了 Key 就必须带 Key，没配 Key 时只接受真正的本机直连。
	//
	// 这一层不能用 read/readWrite 包装：它们只做 withAuth，而 withAuth 在
	// 未配置 Key 时是**直接放行**的。后台能删缓存、删历史、清流水，
	// 一个公开可调的管理接口等于把「数据」交给全网。
	admin := func(methods ...string) func(http.HandlerFunc) http.HandlerFunc {
		return func(h http.HandlerFunc) http.HandlerFunc {
			return s.withRateLimit(s.withAuth(s.withAdmin(allowMethods(h, methods...))))
		}
	}
	mux.HandleFunc("/api/v1/admin/overview", admin(http.MethodGet)(s.handleAdminOverview))
	mux.HandleFunc("/api/v1/admin/cache", admin(http.MethodGet, http.MethodDelete)(s.handleAdminCache))
	mux.HandleFunc("/api/v1/admin/cache/clear", admin(http.MethodPost)(s.handleAdminCacheClear))
	mux.HandleFunc("/api/v1/admin/history", admin(http.MethodGet, http.MethodDelete)(s.handleAdminHistory))
	mux.HandleFunc("/api/v1/admin/changes/clear", admin(http.MethodPost)(s.handleAdminChangesClear))
	mux.HandleFunc("/api/v1/admin/changes", admin(http.MethodDelete)(s.handleAdminChangeDelete))
	mux.HandleFunc("/api/v1/admin/watch/bulk", admin(http.MethodPost)(s.handleAdminWatchBulk))
	mux.HandleFunc("/api/v1/admin/watch/purge", admin(http.MethodPost)(s.handleAdminWatchPurge))
	mux.HandleFunc("/api/v1/admin/backups", admin(http.MethodGet, http.MethodDelete)(s.handleAdminBackups))

	// 徽章与运维。
	//
	// 徽章必须公开（README 里的 <img> 带不了请求头），但正因为公开，
	// 它不能裸奔：每个请求都可能触发一次回源，等于给全网开了一个
	// 「填任意域名就能让服务器去查一次上游」的放大器，而它又不占鉴权、
	// 不限流——之前实测连刷 120 次全部 200，同时 /api/v1/query 已经 429。
	mux.HandleFunc("/badge/icp.svg", s.withRateLimit(allowMethods(s.handleBadge, http.MethodGet)))
	mux.HandleFunc("/badge/icp", s.withRateLimit(allowMethods(s.handleBadge, http.MethodGet)))
	mux.HandleFunc("/healthz", allowMethods(s.handleHealth, http.MethodGet))
	mux.HandleFunc("/readyz", allowMethods(s.handleReady, http.MethodGet))

	// 静态站点
	s.mountStatic(mux)

	// 后台的跨站防护要包在最外层：它判的是「这次请求从哪个站点发起」，
	// 与「是谁发的」是两件事，必须在进入任何 handler 之前就拦下。
	return s.withCommon(s.logRequests(s.adminRequestGuard(mux)))
}

// logRequests 记录访问日志。
//
// 这里有个藏了很久的哑巴：条件判断写着「debug 级别，或 info 级别下的 /api 请求」，
// 但真正输出用的是 `s.log.Debug`，而 log_level=info 时 Debug 记录会被 handler
// 直接丢掉——于是默认配置下**一条访问日志都不会出现**。条件里那半个分支
// 从来没生效过，看起来「API 请求会被记录」，实际上什么都没有。
//
// 现在按级别分开：API 请求在 info 下用 Info 记录，其余（静态资源、探针）
// 仍然只在 debug 下可见。默认级别下能看到「谁在打哪个接口、多快、返回什么」，
// 这是排障时最先要看的东西。
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		ms := float64(time.Since(start).Microseconds()) / 1000
		apiPath := strings.HasPrefix(r.URL.Path, "/api/")
		switch {
		case s.cfg.LogLevel == "debug":
			s.log.Debug("http", "method", r.Method, "path", r.URL.Path,
				"status", sw.code, "ms", ms, "ip", s.clientIP(r))
		case s.cfg.LogLevel == "info" && apiPath:
			s.log.Info("http", "method", r.Method, "path", r.URL.Path,
				"status", sw.code, "ms", ms, "ip", s.clientIP(r))
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
