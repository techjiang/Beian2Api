package api

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
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

// allowMethods 限定接口接受的 HTTP 方法，其余一律 405 并给出 Allow 头。
//
// 这不是洁癖。之前只读接口（stats / providers / changes / verify / watch/*
// 等）根本没检查方法——一个 DELETE /api/v1/stats 会返回 200 和一份完整统计。
// 后果有两层：
//   - 客户端/网关看到「删成功」会认为自己真的删了，实际上什么都没发生；
//   - 有些人靠「改一下方法看会不会变」来判断接口性质，这里给出的答案是错的。
//
// 接口本身没变，变的是对外承诺：不属于它的方法就该明确拒绝。
//
// HEAD 跟着 GET 一起放行：不少监控探针和 CDN 预热只用 HEAD，
// 而 Go 的 net/http 对 HEAD 会自动丢掉响应体，handler 什么都不用改。
// 之前 /api/v1/query 收到 HEAD 直接 405，`curl -I` 探活永远是红叉。
func allowMethods(next http.HandlerFunc, methods ...string) http.HandlerFunc {
	allow := make(map[string]struct{}, len(methods)+1)
	list := make([]string, 0, len(methods)+1)
	for _, m := range methods {
		m = strings.ToUpper(m)
		if _, ok := allow[m]; ok {
			continue
		}
		allow[m] = struct{}{}
		list = append(list, m)
		if m == http.MethodGet {
			allow[http.MethodHead] = struct{}{}
			list = append(list, http.MethodHead)
		}
	}
	header := strings.Join(list, ", ")
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := allow[r.Method]; ok {
			// HEAD 以 GET 的身份进入业务处理函数。
			//
			// 各 handler 内部还各自判断「仅支持 GET / POST」，直接把 HEAD 放进去
			// 会被它们再拦一次 405——那等于没放行。复制一份请求、把方法改成 GET
			// 是最省事的一致做法：net/http 对真实连接上的 HEAD 会自动丢掉响应体，
			// 而 handler 里「算完再写」的逻辑照常跑一遍，这正是 HEAD 的语义。
			if r.Method == http.MethodHead {
				hr := r.Clone(r.Context())
				hr.Method = http.MethodGet
				next(w, hr)
				return
			}
			next(w, r)
			return
		}
		w.Header().Set("Allow", header)
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 "+header)
	}
}

// allowExactMethods 与 allowMethods 相同，但**不**顺手放行 HEAD。
//
// HEAD 在 Go 里会被通用地当成「GET 但不写响应体」处理（allowMethods 正是
// 这么做的），对只读接口这完全正确。但对会**产生副作用**的接口它是个陷阱：
// 在线备份每收到一次 HEAD 就真的在磁盘上写出一份快照文件——
// 而探针、负载均衡健康检查、`curl -I`、CDN 预热随手就在发 HEAD。
// 一个「我只是问一下它活着没」的请求不该往磁盘里留东西。
//
// 这类接口没有「HEAD 语义」可言（HEAD 的承诺是「与 GET 相同、只是不带响应体」，
// 而它连 GET 都会写文件），所以干脆拒掉并给出 Allow，比装作支持诚实。
func allowExactMethods(next http.HandlerFunc, methods ...string) http.HandlerFunc {
	allow := make(map[string]struct{}, len(methods))
	list := make([]string, 0, len(methods))
	for _, m := range methods {
		m = strings.ToUpper(m)
		if _, ok := allow[m]; ok {
			continue
		}
		allow[m] = struct{}{}
		list = append(list, m)
	}
	header := strings.Join(list, ", ")
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := allow[r.Method]; ok {
			next(w, r)
			return
		}
		w.Header().Set("Allow", header)
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 "+header)
	}
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
	case "/", "/healthz", "/readyz", "/api/v1/openapi.json", "/docs", "/admin", "/favicon.ico", "/logo.svg", "/apple-touch-icon.png":
		return true
	}
	return strings.HasPrefix(p, "/assets/") || strings.HasPrefix(p, "/badge/") || strings.HasPrefix(p, "/docs/")
}

// withCommon 统一附加通用响应头：CORS、请求追踪、内容安全。
func (s *Server) withCommon(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// 「这台机器上有什么」不能靠通配 CORS 公开。
		//
		// `cors: ["*"]` 的默认值是给只读查询台用的（别人家的前端、README 徽章）：
		// 查一个域名的备案号本来就是公开信息。但同一份配置顺带把
		// 监控域名清单、变更流水、运行统计也开放给了任意站点——
		// 而这些回答的是「这台机器在盯着哪些域名」，那是一份情报，
		// 不是公开信息。默认配置下攻击者只要让受害者打开一个页面，
		// 就能把清单整份读走。
		//
		// 所以分成两类：公开查询类接口保持通配，实例状态类接口只认
		// cors 里明确列出的域名（通配符不算）。
		if s.sensitivePath(r.URL.Path) {
			if origin := r.Header.Get("Origin"); origin != "" && s.adminOriginAllowed(origin, r.Host) {
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Vary", "Origin")
			}
		} else if s.allowAnyOrigin {
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

// 后台接口的「谁在动手」判定。
//
// 上面那一层 withAdmin 管的是「这是不是管理员」——配了 Key 就验 Key，
// 没配 Key 就要求 TCP 回环。但它回答不了一个更基础的问题：**这次请求
// 真的是那个人自己发起的吗？**
//
// 浏览器里有两个洞，两个都不需要攻击者拿到 Key：
//
//  1. **跨站简单请求**。`text/plain` 的 POST 不触发预检，也不带凭据，
//     于是任何一个网页（含广告位 iframe）都能往 127.0.0.1:8080 发一句
//     `fetch('http://127.0.0.1:8080/api/v1/admin/cache/clear', {...})`。
//     甚至不需要 JS：一个 method=POST 的 <form> 就够。而我们的 JSON 解析
//     不看 Content-Type，`-H 'Content-Type: text/plain' -d '{"only_expired":false}'`
//     会被照常当成 JSON 解出来——清空全部缓存、清空监控、生成备份文件，
//     全都是这样能打通的。攻击者读不到响应，但他不需要读响应。
//
//  2. **DNS 重绑定**。攻击者让 evil.com 先解析到自己的服务器、把页面发下来，
//     再把 TTL 归零改成 127.0.0.1。之后页面里的 fetch 发的就是
//     `http://evil.com:8080/...`——它对页面来说是**同源**的，Origin 检查
//     完全放行；而服务端看到的是 RemoteAddr=127.0.0.1，回环判据也放行。
//     唯一露馅的地方是 Host 头：它写的是 evil.com。
//
// 所以这里补两层判据：
//   - 请求必须不是跨站发起的（Sec-Fetch-Site / Origin）；
//   - 未配 Key 时，Host 必须是回环名字（挡住重绑定）。
//
// 用 curl、脚本、CI 调后台的人不受影响：它们不带这些头，也就没有「站点」身份可言。
func isAdminPath(p string) bool {
	return strings.HasPrefix(p, "/api/v1/admin/")
}

// mutatingPath 判断这个路径是不是「会改变实例状态」的接口。
//
// 上一版只护了 /api/v1/admin/*，因为「破坏性操作都在后台」这个判断在当时是对的。
// 实际不是：/api/v1/watch 的 POST/DELETE 同样能改状态——把任意域名塞进监控
// （于是「这台机器在盯什么」被人写进去），或者把别人的监控移出（巡检悄悄停掉，
// 却没有任何人收到通知）。而它根本不需要 Key，一个跨站 `<form>` 就够。
// /api/v1/watch/run 更直接：每调一次就是一次全量回源，等于用别人的机器打上游。
//
// 判据不看 HTTP 方法：GET /api/v1/watch 本来就会被 allowMethods 拒成 405，
// 拦在守卫这层反而少一次无意义的业务处理。
func mutatingPath(p string) bool {
	if isAdminPath(p) {
		return true
	}
	switch p {
	case "/api/v1/watch", "/api/v1/watch/run", "/api/v1/notify/test":
		return true
	}
	return false
}

// sensitivePath 判断这个路径返回的是不是「这台机器自己的状态」。
//
// 它决定两件事：跨站能否发起（配合 mutatingPath），以及能否走通配 CORS。
// 备案查询的结果（query / batch / history / verify / badge）是公开信息，
// 别人家的前端来调本来就是设计目标；而下面这些回答的是
// 「你在监控哪些域名、什么变了、数据文件在哪、上游配了什么」——
// 那是一份情报，不该因为 cors 默认值写着 "*" 就交给任意站点。
func sensitivePath(p string) bool {
	if isAdminPath(p) {
		return true
	}
	switch p {
	case "/api/v1/stats", "/api/v1/providers",
		"/api/v1/watch/list", "/api/v1/watch/status",
		"/api/v1/changes", "/api/v1/changes.rss",
		"/api/v1/notify/status":
		return true
	}
	return false
}

// (s *Server) sensitivePath 让 withCommon 里能直接用方法。
func (s *Server) sensitivePath(p string) bool { return sensitivePath(p) }

// crossSiteGuard 拒绝跨站发起的状态变更请求。
func (s *Server) crossSiteGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !mutatingPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if !s.crossSiteAllowed(r) {
			writeErr(w, http.StatusForbidden, "cross_site_blocked",
				"跨站请求不能改这台实例的状态；请在它自己的页面里操作，或用带 Key 的脚本调用")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// crossSiteAllowed 判断这次请求是否可以放行。
//
// 判据是 Origin：浏览器在所有「可能改变状态」的跨站请求上都会自动带上它，
// 页面脚本改不掉。有 Origin 就必须是本站或 cors 里明确列出的域名。
//
// 没有 Origin 的请求（curl、脚本、地址栏直接打开）没有「来自哪个站点」可言，
// 交给 withAdmin 那层判「是不是管理员」就够了。
//
// Sec-Fetch-Site 只作为补充：它能在 Origin 缺失时仍标出「这是跨站来的」，
// 而 cross-site / same-site 都该拒——evil.example.com 与 example.com 是
// same-site，但它显然不是同一个管理台。
func (s *Server) crossSiteAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin != "" {
		return s.adminOriginAllowed(origin, r.Host)
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site":
		return false
	}
	return true
}

// adminOriginAllowed 判断某个 Origin 是否被允许操作这台实例。
//
// 关键一点：**不认 `cors: ["*"]`**。通配符的意思是「我的只读查询接口欢迎
// 任何前端来调」，它绝不该被理解成「任何网站都可以删我的数据」。
// 要让别的域名改状态，就在 cors 里把那个域名一字不差地写上。
func (s *Server) adminOriginAllowed(origin, host string) bool {
	if origin == "null" {
		// 沙箱 iframe / file:// 页面发出来的 Origin 就是字符串 "null"，
		// 它比任何具体域名都更不该拿到后台。
		return false
	}
	if u, err := url.Parse(origin); err == nil && u.Host != "" {
		// 同源：后台页面自己发出的请求都走这一支。
		if strings.EqualFold(u.Host, host) {
			return true
		}
	}
	for _, o := range s.origins {
		if o != "*" && strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

// hostIsLoopbackName 判断 Host 头里的主机名是不是「本机」。
//
// 只看主机名，不看端口。它单独存在的理由是 DNS 重绑定：那种攻击下
// RemoteAddr 确实是 127.0.0.1（判据是真的），但 Host 说的是攻击者的域名。
// 只接受回环地址与 localhost，是因为后台在没有 Key 时的承诺本来就是
// 「只接受本机直连」——用局域网 IP 打开自己部署的服务，本来就在这条线之外。
func hostIsLoopbackName(host string) bool {
	if host == "" {
		return false
	}
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
