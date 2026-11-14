package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asoe/beian2api/internal/store"
)

// cmdBackup 生成一致性快照（bbolt 事务级拷贝，可以在运行时执行）。
func cmdBackup(args []string) error {
	fs := newFlags("backup")
	cfgPath := fs.String("config", "", "配置文件路径")
	fs.StringVar(cfgPath, "c", "", "配置文件路径")
	out := fs.String("o", "", "输出文件路径（默认 data/backups/beian2api-<时间>.db）")
	server := fs.String("server", "", "通过运行中的服务做在线备份，如 http://127.0.0.1:8080")
	key := fs.String("key", "", "服务启用鉴权时的 API Key")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *server != "" {
		return backupOnline(*server, *key, *out)
	}
	cfg, st, err := bootstrap(*cfgPath)
	if err != nil {
		// 服务在运行时数据库被锁住，这时该走服务内的在线备份接口。
		if strings.Contains(err.Error(), "正被其他进程占用") {
			fmt.Fprintln(os.Stderr, "提示：服务正在运行，请改用在线备份接口：")
			fmt.Fprintln(os.Stderr, `  curl -X POST "http://127.0.0.1:8080/api/v1/admin/backup"`)
			fmt.Fprintln(os.Stderr, "或者停掉服务后再执行本命令。")
		}
		return err
	}
	defer st.Close()

	dst := *out
	if dst == "" {
		dst = filepath.Join(cfg.DataDir, "backups", "beian2api-"+time.Now().Format("20060102-150405")+".db")
	}
	// 本地备份也要能进后台的备份列表，否则它会成为一个谁也不知道的孤儿文件。
	// 只在「指向默认的 backups 目录」时强制，随手 -o 到别处（拿去归档、拷到
	// 另一台机器）不受限制——那份文件本来就该由使用者自己管。
	if !strings.HasSuffix(dst, ".db") && strings.Contains(dst, string(filepath.Separator)+"backups"+string(filepath.Separator)) {
		return fmt.Errorf("备份文件名 %q 必须以 .db 结尾，否则后台的备份列表里看不到它，也删不掉", filepath.Base(dst))
	}
	if err := st.Snapshot(dst); err != nil {
		return err
	}
	info, err := os.Stat(dst)
	if err == nil {
		fmt.Printf("✔ 备份完成: %s (%.1f KB)\n", dst, float64(info.Size())/1024)
	}
	return nil
}

// cmdExport 导出全部数据为 JSON，用于跨机器迁移或版本升级。
//
// 以前只导缓存，于是 README 里那句「跨机器迁移用 JSON」有一半是假的：
// 迁完监控列表、历史快照、变更流水、推送状态全没了。现在导的是完整快照，
// 并且在一个读事务里取，导出来的是某一时刻的一致状态。
func cmdExport(args []string) error {
	fs := newFlags("export")
	cfgPath := fs.String("config", "", "配置文件路径")
	fs.StringVar(cfgPath, "c", "", "配置文件路径")
	out := fs.String("o", "beian2api-export.json", "输出文件路径")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	_, st, err := bootstrap(*cfgPath)
	if err != nil {
		return err
	}
	defer st.Close()

	d, err := st.Dump()
	if err != nil {
		return err
	}
	d.ExportedAt = time.Now().Unix()
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(d); err != nil {
		return err
	}
	fmt.Printf("✔ 已导出到 %s\n", *out)
	fmt.Printf("  缓存 %d 条 · 历史 %d 条 · 监控 %d 个 · 变更 %d 条 · 推送通道 %d 个\n",
		len(d.Cache), len(d.History), len(d.Watch), len(d.Changes), len(d.Notify))
	return nil
}

// cmdImport 从 JSON 恢复全部数据。
func cmdImport(args []string) error {
	fs := newFlags("import")
	cfgPath := fs.String("config", "", "配置文件路径")
	fs.StringVar(cfgPath, "c", "", "配置文件路径")
	in := fs.String("i", "beian2api-export.json", "输入文件路径")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	_, st, err := bootstrap(*cfgPath)
	if err != nil {
		return err
	}
	defer st.Close()

	f, err := os.Open(*in)
	if err != nil {
		return err
	}
	defer f.Close()
	d, err := decodeDump(f)
	if err != nil {
		return err
	}
	if err := st.Restore(d); err != nil {
		return err
	}
	fmt.Printf("✔ 已导入\n")
	fmt.Printf("  缓存 %d 条 · 历史 %d 条 · 监控 %d 个 · 变更 %d 条 · 推送通道 %d 个\n",
		len(d.Cache), len(d.History), len(d.Watch), len(d.Changes), len(d.Notify))
	return nil
}

// decodeDump 读导出文件，同时兼容旧版本（只有 items 字段）的格式。
//
// 兼容不是客套：老版本导出的文件现在还在别人机器上，读不了就等于
// 「升级之后，上次的备份恢复不进去」——这是最容易在出事那天才发现的坑。
func decodeDump(r io.Reader) (*store.Dump, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 512<<20))
	if err != nil {
		return nil, err
	}
	var d store.Dump
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("解析导出文件: %w", err)
	}
	if d.Version == 0 {
		var legacy struct {
			Items []*store.Entry `json:"items"`
		}
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return nil, fmt.Errorf("解析导出文件: %w", err)
		}
		if len(legacy.Items) == 0 {
			return nil, errors.New("无法识别的导出文件：既不是当前格式，也没有可用的 items")
		}
		d.Version, d.Cache = 1, legacy.Items
	}
	return &d, nil
}

// cmdKeygen 生成随机 API Key。
func cmdKeygen(args []string) error {
	fs := newFlags("keygen")
	n := fs.Int("n", 1, "生成数量")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	for i := 0; i < *n; i++ {
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		fmt.Println("bk_" + base64.RawURLEncoding.EncodeToString(b))
	}
	return nil
}

// backupOnline 调用运行中服务的备份接口，服务不需要停机。
func backupOnline(server, key, out string) error {
	// 本地先校验后缀，而不是把 -o /tmp/snap 原样发出去让服务端回 400。
	//
	// 服务端要求 .db 是有理由的（备份列表和「清空备份」只认这个后缀，
	// 不带后缀的快照会占着磁盘、在后台看不见、也删不掉）。但那条规则从
	// 命令行的角度看只是一句「你的 -o 少了个后缀」——在这里说清楚，
	// 比让人对着一句服务端错误去猜要好。
	name := ""
	if out != "" {
		name = filepath.Base(out)
		if !strings.HasSuffix(name, ".db") {
			return fmt.Errorf("-o %q 必须以 .db 结尾：在线备份的文件名会出现在后台的备份列表里，"+
				"而那里只认 .db（例如 -o /backup/%s.db）", out, name)
		}
	}
	u := strings.TrimRight(server, "/") + "/api/v1/admin/backup"
	if name != "" {
		u += "?name=" + urlQueryEscape(name)
	}
	req, err := http.NewRequest(http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("连接服务失败: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		OK   bool `json:"ok"`
		Data struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("响应解析失败: %w", err)
	}
	if !body.OK {
		msg := "备份失败"
		if body.Error != nil {
			msg = body.Error.Message
		}
		return errors.New(msg)
	}
	fmt.Printf("✔ 在线备份完成: %s (%.1f KB)\n", body.Data.Path, float64(body.Data.Size)/1024)
	return nil
}

func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
