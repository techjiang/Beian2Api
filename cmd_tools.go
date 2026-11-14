package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asoe/beian2api/internal/store"
)

// cmdBackup 生成一致性快照（bbolt 事务级拷贝，可以在运行时执行）。
func cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	out := fs.String("o", "", "输出文件路径（默认 data/backups/beian2api-<时间>.db）")
	server := fs.String("server", "", "通过运行中的服务做在线备份，如 http://127.0.0.1:8080")
	key := fs.String("key", "", "服务启用鉴权时的 API Key")
	if err := fs.Parse(args); err != nil {
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
	if err := st.Snapshot(dst); err != nil {
		return err
	}
	info, err := os.Stat(dst)
	if err == nil {
		fmt.Printf("✔ 备份完成: %s (%.1f KB)\n", dst, float64(info.Size())/1024)
	}
	return nil
}

// dumpItem 是导出文件的单条记录。
type dumpItem struct {
	Domain    string `json:"domain"`
	Found     bool   `json:"found"`
	ICPCode   string `json:"icp_code,omitempty"`
	UnitName  string `json:"unit_name,omitempty"`
	Records   any    `json:"records"`
	Source    string `json:"source"`
	FetchedAt int64  `json:"fetched_at"`
}

// cmdExport 导出全部缓存为 JSON，用于跨机迁移或版本升级。
func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	out := fs.String("o", "beian2api-export.json", "输出文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, st, err := bootstrap(*cfgPath)
	if err != nil {
		return err
	}
	defer st.Close()

	type wrapper struct {
		Version    string         `json:"version"`
		ExportedAt int64          `json:"exported_at"`
		Total      int            `json:"total"`
		Items      []*store.Entry `json:"items"`
	}
	w := wrapper{Version: version, ExportedAt: time.Now().Unix()}
	if err := st.AllCache(func(e *store.Entry) error {
		w.Items = append(w.Items, e)
		return nil
	}); err != nil {
		return err
	}
	w.Total = len(w.Items)
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(w); err != nil {
		return err
	}
	fmt.Printf("✔ 已导出 %d 条到 %s\n", w.Total, *out)
	return nil
}

// cmdImport 从 JSON 恢复缓存。
func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	in := fs.String("i", "beian2api-export.json", "输入文件路径")
	if err := fs.Parse(args); err != nil {
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
	var w struct {
		Items []*store.Entry `json:"items"`
	}
	if err := json.NewDecoder(f).Decode(&w); err != nil {
		return err
	}
	if err := st.Bulk(w.Items); err != nil {
		return err
	}
	fmt.Printf("✔ 已导入 %d 条\n", len(w.Items))
	return nil
}

// cmdKeygen 生成随机 API Key。
func cmdKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	n := fs.Int("n", 1, "生成数量")
	if err := fs.Parse(args); err != nil {
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
	u := strings.TrimRight(server, "/") + "/api/v1/admin/backup"
	if out != "" {
		u += "?name=" + urlQueryEscape(filepath.Base(out))
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
