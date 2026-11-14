// Command beian2api 是一个单二进制的 ICP 备案查询 API 服务。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/asoe/beian2api/internal/api"
	"github.com/asoe/beian2api/internal/config"
	"github.com/asoe/beian2api/internal/notify"
	"github.com/asoe/beian2api/internal/provider"
	"github.com/asoe/beian2api/internal/service"
	"github.com/asoe/beian2api/internal/store"
	"github.com/asoe/beian2api/internal/watch"
)

// version 由构建时通过 -ldflags 注入。
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return cmdServe(args)
	case "version":
		fmt.Println("beian2api", version)
		return nil
	case "config":
		return cmdConfig(args)
	case "backup":
		return cmdBackup(args)
	case "export":
		return cmdExport(args)
	case "import":
		return cmdImport(args)
	case "keygen":
		return cmdKeygen(args)
	case "doctor":
		return cmdDoctor(args)
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("未知命令 %q", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Beian2API — 轻量自托管的 ICP 备案查询 API

用法:
  beian2api [命令] [参数]

命令:
  serve      启动 HTTP 服务（默认）
  config     打印当前生效配置
  backup     生成一致性数据快照
               服务运行时： -server http://127.0.0.1:8080
               服务停止时： -o /path/to/backup.db
  export     导出全部缓存为 JSON（可跨机器迁移）
  import     从 JSON 导入缓存
  keygen     生成随机 API Key
  doctor     自检：配置、目录、上游连通性
  version    打印版本

通用参数:
  -c, --config  配置文件路径

示例:
  beian2api serve
  beian2api serve -c /etc/beian2api/config.yaml
  beian2api doctor
  beian2api backup -server http://127.0.0.1:8080
  BEIAN2API_PROVIDERS=mock:1 beian2api serve    # 离线演示
`)
}

// logger 按配置创建结构化日志。
func logger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}

// bootstrap 打开配置与存储，供多个命令复用。
func bootstrap(configPath string) (*config.Config, *store.Store, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, nil, err
	}
	st.WithRetention(cfg.Storage.RetainChanges, cfg.Storage.RetainHistory)
	return cfg, st, nil
}

func buildTasks(cfg *config.Config) ([]provider.Task, error) {
	reg := provider.NewRegistry()
	var tasks []provider.Task
	for _, p := range cfg.Providers {
		if !p.Enabled || p.Weight <= 0 {
			continue
		}
		timeout := time.Duration(p.Timeout) * time.Second
		inst, err := reg.Build(p.Name, provider.Options{Timeout: timeout, HTTP: provider.NewHTTPClient(timeout)})
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, provider.Task{Provider: inst, Weight: p.Weight, Timeout: timeout})
	}
	if len(tasks) == 0 {
		return nil, errors.New("没有启用任何数据源，请检查配置中的 providers")
	}
	return tasks, nil
}

// buildNotifier 按配置构造推送器，并把状态接上存储。
//
// 状态落盘不是为了好看：推送失败大多发生在无人值守的定时巡检里，
// 不落盘就没人会发现「昨晚所有推送都挂了」。
func buildNotifier(cfg *config.Config, st *store.Store, log *slog.Logger) *notify.Dispatcher {
	var chs []notify.Channel
	for _, c := range cfg.Channels() {
		chs = append(chs, notify.Channel{
			Name: c.Name, Kind: c.Kind, URL: c.URL,
			Secret: c.Secret, Events: c.Events, Disabled: c.Disabled,
		})
	}
	d := notify.New(chs, log).WithRetries(cfg.Notify.Retries, 0)
	if saved, err := st.NotifyStates(); err == nil && len(saved) > 0 {
		// 只保留仍然存在的通道，改过配置后别留幽灵状态。
		live := map[string]notify.State{}
		names := map[string]struct{}{}
		for _, ch := range d.Channels() {
			names[ch.Name] = struct{}{}
			if v, ok := saved[ch.Name]; ok {
				live[ch.Name] = v
			}
		}
		d.Seed(live)
		_ = st.PurgeNotifyState(names)
	}
	return d.WithRecorder(func(s notify.State) { _ = st.PutNotifyState(s) })
}

// baseURL 推断对外地址：优先配置，其次本地监听地址，用于推送里的详情链接。
func baseURL(cfg *config.Config) string {
	if cfg.BaseURL != "" {
		return cfg.BaseURL
	}
	addr := cfg.Listen
	if strings.HasPrefix(addr, ":") {
		return "http://127.0.0.1" + addr
	}
	return "http://" + addr
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	fs.StringVar(cfgPath, "c", "", "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, st, err := bootstrap(*cfgPath)
	if err != nil {
		return err
	}
	defer st.Close()

	log := logger(cfg.LogLevel)
	tasks, err := buildTasks(cfg)
	if err != nil {
		return err
	}
	svc := service.New(cfg, st, tasks, log)

	notifier := buildNotifier(cfg, st, log)
	var w *watch.Watcher
	if cfg.Watch.Enabled {
		w = watch.New(svc, st, cfg.WatchInterval(), cfg.Watch.Workers, notifier, log).
			WithBaseURL(baseURL(cfg)).
			WithFailThreshold(cfg.Watch.FailThreshold)
	}

	if notifier.Enabled() {
		log.Info("变更推送已启用", "channels", len(notifier.Channels()))
	}

	base := baseURL(cfg)
	srv := api.New(api.Options{Config: cfg, Service: svc, Watch: w, Notify: notifier, BaseURL: base, Log: log, Version: version})
	httpSrv := &httpServer{addr: cfg.Listen, handler: srv.Handler()}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 计数器攒在内存里、定期刷盘；顺带按保留上限裁剪只增数据。
	// 15 秒一次：丢 15 秒的命中率统计无所谓，而每次请求都 fsync 是真的伤性能。
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if r, err := st.Maintain(); err != nil {
					log.Warn("存储维护失败", "err", err)
				} else if r.ChangesTrimmed > 0 {
					log.Info("变更流水裁剪完成", "changes", r.ChangesTrimmed)
				}
			}
		}
	}()

	if w != nil {
		w.Start(ctx)
		log.Info("备案巡检已启动", "interval", cfg.WatchInterval().String(), "workers", cfg.Watch.Workers)
	}

	log.Info("Beian2API 启动",
		"version", version,
		"listen", cfg.Listen,
		"data", st.Path(),
		"providers", cfg.EnabledProviders(),
		"auth", len(cfg.Auth.Keys) > 0,
	)

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, errServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("收到退出信号，正在优雅关闭…")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("关闭服务时出错", "err", err)
	}
	if w != nil {
		w.Stop()
	}
	// 推送是后台协程：不等它们，最后一条「备案注销」可能就在关闭瞬间被丢掉。
	if notifier.Enabled() {
		done := make(chan struct{})
		go func() { notifier.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			log.Warn("仍有推送未完成，已超时退出")
		}
	}
	// 进程退出前把内存里还没刷盘的计数器落下去，否则最后的统计会归零。
	if _, err := st.Maintain(); err != nil {
		log.Warn("退出前存储维护失败", "err", err)
	}
	log.Info("已退出")
	return nil
}

func cmdConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	fs.StringVar(cfgPath, "c", "", "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	b, err := cfg.Dump()
	if err != nil {
		return err
	}
	fmt.Print(string(b))
	return nil
}
