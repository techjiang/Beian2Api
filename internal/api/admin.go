package api

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asoe/beian2api/internal/model"
)

// handleAdminBackup 在服务运行中生成一致性数据快照。
//
// 为什么必须由进程自己做：bbolt 用排他文件锁，外部进程在服务运行时
// 打不开数据库，只能靠持有连接的这个进程执行 Tx.WriteTo。
//
// 访问控制：配置了 API Key 时必须携带；未配置时只允许本机回环访问，
// 避免把「能写文件」的能力暴露给公网。
func (s *Server) handleAdminBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 GET / POST")
		return
	}
	if len(s.keys) == 0 && !s.isLoopback(r) {
		writeErr(w, http.StatusForbidden, "forbidden",
			"未配置 API Key 时，备份接口仅允许本机访问")
		return
	}

	dir := filepath.Join(s.cfg.DataDir, "backups")

	// 文件名精确到毫秒，且撞名时让路而不是覆盖。
	//
	// 之前是秒级时间戳 + O_TRUNC：同一秒里点两次「立即备份」（或者脚本里
	// 连发两个请求），第二次会静默盖掉第一次写出来的文件——两个请求都回
	// 200 和同一个路径，磁盘上只剩一份。备份是「最后一道保险」，
	// 它自己静默丢一份比不备份更让人安心得过分。
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	explicit := name != ""
	if name == "" {
		base := "beian2api-" + time.Now().Format("20060102-150405.000")
		name = base + ".db"
		for i := 2; ; i++ {
			if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
				break
			}
			name = fmt.Sprintf("%s-%d.db", base, i)
			if i > 100 {
				writeErr(w, http.StatusInternalServerError, "backup_failed", "备份目录里同名文件过多，请先清理")
				return
			}
		}
	}
	// 只接受纯文件名，防止路径穿越；而且必须以 .db 结尾。
	//
	// 后缀这一条不是洁癖：备份列表和「清空备份」都只认 `.db`（它们要保护
	// 目录里用户自己放的东西）。一个不以 .db 结尾的快照会写进磁盘、
	// 在后台看不见、删不掉、也不会被清理——纯占地方，而且没人知道它在。
	// `beian2api backup -server ... -o /tmp/snap` 正好会走到这条路上。
	if strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		writeErr(w, http.StatusBadRequest, "invalid_name", "name 只能是纯文件名")
		return
	}
	if !strings.HasSuffix(name, ".db") {
		writeErr(w, http.StatusBadRequest, "invalid_name",
			"name 必须以 .db 结尾，否则这份快照不会出现在备份列表里，也清理不掉")
		return
	}
	dst := filepath.Join(dir, name)
	// 显式指定的文件名撞上已有备份时明确拒绝。
	//
	// 用固定名字做「每天一份，覆盖昨天」是个合理用法，但它必须是**说出来的**
	// 决定，不能由「刚好同名」来替人决定——两次不同的备份变成一份，
	// 中间那次的内容就永久没了。要覆盖就先删再备，那是两步、看得见。
	if explicit {
		if _, err := os.Stat(dst); err == nil {
			writeErr(w, http.StatusConflict, "backup_exists",
				"backups 目录里已有同名文件 "+name+"，换一个名字或先删掉它")
			return
		}
	}

	// 在线备份要在一个读事务里把整个数据文件写到磁盘。数据文件几百 MB 时
	// 这一步必然超过 30 秒的 WriteTimeout，结果是备份成功、响应被掐断，
	// 调用方（含 CLI 的 `backup -server`）看到的是「连接失败」。
	defer extendWriteDeadline(w, 10*time.Minute)()
	if err := s.svc.Store().Snapshot(dst); err != nil {
		writeErr(w, http.StatusInternalServerError, "backup_failed", err.Error())
		return
	}
	info, statErr := os.Stat(dst)
	size := int64(0)
	if statErr == nil {
		size = info.Size()
	}
	writeOK(w, map[string]any{
		"path":       dst,
		"size":       size,
		"created_at": model.Now(),
	})
}

// isLoopback 判断这次请求是不是真的从本机发起的。
//
// 两条都要满足：TCP 对端是回环，且 Host 头指向本机。
//
//   - 不看任何转发头，包括开了 trust_proxy 之后。文档里承诺的是「只允许本机
//     直连」，而这个判断守护的是「能往磁盘写文件」。一旦采信转发头，任何
//     能直接连到端口的人写一行 `X-Forwarded-For: 127.0.0.1` 就拿到了备份能力。
//   - 还要看 Host：DNS 重绑定攻击下，页面从 evil.com 加载、再让 evil.com
//     解析到 127.0.0.1，此时 RemoteAddr 真的是回环，但 Host 是 evil.com。
//     只查对端地址的话，那把「回环才放行」的门等于开着的。
func (s *Server) isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	return hostIsLoopbackName(r.Host)
}
