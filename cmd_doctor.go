package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asoe/beian2api/internal/notify"
	"github.com/asoe/beian2api/internal/provider"
)

// cmdDoctor 做部署自检：配置是否可读、目录是否可写、各上游是否可达。
func cmdDoctor(args []string) error {
	fs := newFlags("doctor")
	cfgPath := fs.String("config", "", "配置文件路径")
	fs.StringVar(cfgPath, "c", "", "配置文件路径")
	probe := fs.String("domain", "example.com", "用于探测上游的域名")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, st, err := bootstrap(*cfgPath)
	if err != nil {
		return fmt.Errorf("配置/存储初始化失败: %w", err)
	}
	defer st.Close()

	fmt.Println("Beian2API 自检")
	fmt.Printf("  版本      %s\n", version)
	fmt.Printf("  监听      %s\n", cfg.Listen)
	fmt.Printf("  数据目录  %s\n", absPath(cfg.DataDir))
	fmt.Printf("  数据文件  %s\n", st.Path())

	// 目录可写性
	testFile := filepath.Join(cfg.DataDir, ".write-test")
	if err := os.WriteFile(testFile, []byte("ok"), 0o600); err != nil {
		fmt.Printf("  ✘ 数据目录不可写: %v\n", err)
	} else {
		_ = os.Remove(testFile)
		fmt.Println("  ✔ 数据目录可写")
	}

	c, err := st.Count()
	if err != nil {
		fmt.Printf("  ✘ 存储读取失败: %v\n", err)
	} else {
		fmt.Printf("  ✔ 存储正常：缓存 %d / 历史 %d / 监控 %d\n", c.Cache, c.History, c.Watch)
	}

	tasks, err := buildTasks(cfg)
	if err != nil {
		fmt.Printf("  ✘ 数据源配置错误: %v\n", err)
		return nil
	}
	fmt.Println("上游连通性:")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, t := range tasks {
		start := time.Now()
		recs, err := t.Provider.Lookup(ctx, *probe)
		cost := float64(time.Since(start).Microseconds()) / 1000
		switch {
		case err == nil:
			fmt.Printf("  ✔ %-8s %6.0fms  返回 %d 条\n", t.Provider.Name(), cost, len(recs))
		case errors.Is(err, provider.ErrNotFound):
			fmt.Printf("  ✔ %-8s %6.0fms  连通（该域名无备案）\n", t.Provider.Name(), cost)
		default:
			fmt.Printf("  ! %-8s %6.0fms  %v\n", t.Provider.Name(), cost, err)
		}
	}

	// 凭据是否已换：apihz 用公共测试凭据时会被限流，而界面上看不出任何异常，
	// 只能表现为「偶尔查不到」。这一点必须在自检里说清楚。
	for _, p := range cfg.Providers {
		if strings.EqualFold(p.Name, "apihz") && p.Enabled && p.Weight > 0 && !p.HasCredential {
			fmt.Println("  ! apihz   仍在使用公共测试凭据，被限流时只会表现为「查不到」")
			fmt.Println("            建议在 config.yaml 里填 providers[].id / key，或设 BEIAN2API_APIHZ_ID/KEY")
		}
	}

	// 推送通道只报「配了几个、什么类型」，不回显地址——机器人 URL 本身就是凭据。
	chs := cfg.Channels()
	if len(chs) == 0 {
		fmt.Println("推送通道:  未配置（变更只会写进首页/RSS，不会推送到群）")
	} else {
		fmt.Println("推送通道:")
		for _, c := range chs {
			kind := notify.NormalizeKind(c.Kind, c.URL)
			name := c.Name
			if name == "" {
				name = kind
			}
			fmt.Printf("  ✔ %-10s %-9s 需在 IM 里实际收到一条才算配通\n", name, kind)
		}
	}

	// 顺带校验响应结构，防止上游改名导致字段错位。
	b, _ := json.Marshal(map[string]any{"ok": true})
	fmt.Printf("  ✔ JSON 编码正常（%d 字节）\n", len(b))
	fmt.Println("\n提示：上游被 WAF 拦截是常见情况，系统会自动降级到其他源或本地快照。")
	return nil
}

func absPath(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}
