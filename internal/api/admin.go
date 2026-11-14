package api

import (
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

	name := r.URL.Query().Get("name")
	if name == "" {
		name = "beian2api-" + time.Now().Format("20060102-150405") + ".db"
	}
	// 只接受纯文件名，防止路径穿越。
	if strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		writeErr(w, http.StatusBadRequest, "invalid_name", "name 只能是纯文件名")
		return
	}
	dir := filepath.Join(s.cfg.DataDir, "backups")
	dst := filepath.Join(dir, name)

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
// 只看 TCP 对端地址，不看任何转发头——包括开了 trust_proxy 之后。
// 理由：文档里承诺的是「只允许本机直连」，而这个判断守护的是「能往磁盘写文件」。
// 一旦采信转发头，任何能直接连到端口的人只要写一行 `X-Forwarded-For: 127.0.0.1`
// 就拿到了备份能力（哪怕开了 trust_proxy，那条头也是他自己填的）。
// 换句话说：这一条不需要「真实客户端 IP」，需要的是「你是不是从本机连过来的」，
// 而这两件事在反向代理后面天然不同——经过反代的请求本来就不是本机直连。
func (s *Server) isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
