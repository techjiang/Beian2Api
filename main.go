// Command beian2api 是一个单二进制的 ICP 备案查询 API 服务。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
		// -h / --help 不是错误。
		//
		// flag 包在遇到 -h 时返回 flag.ErrHelp，而这里把它当普通错误处理，
		// 于是 `beian2api serve -h` 会先打一遍用法、再打一行「错误: flag: help requested」
		// 并以退出码 1 结束。对脚本和习惯用 `cmd -h | less` 的人来说，
		// 「求帮助」变成「失败」——这不该算错。
		if errors.Is(err, flag.ErrHelp) || errors.Is(err, errHelpShown) {
			return
		}
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
		usage(os.Stdout)
		return nil
	default:
		// 未知命令时用法去 stderr：它跟着一条错误一起出现，不该混进 stdout。
		usage(os.Stderr)
		return fmt.Errorf("未知命令 %q", cmd)
	}
}

// usage 输出总帮助。
//
// 分开 stdout / stderr 是因为 `beian2api help | less` 是一种正常用法：
// 把帮助一律打到 stderr，管道里就只剩下一片空白，用户会以为命令坏了。
func usage(w io.Writer) {
	fmt.Fprint(w, `Beian2API — 轻量自托管的 ICP 备案查询 API

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

// newFlags 构造子命令的参数集合，并把 -h / --help 的用法打到 stdout。
//
// flag 包默认把「用法」和「参数写错」都写到 stderr。于是：
//   - `beian2api serve -h | less` 里一片空白（帮助是正常输出，该走 stdout）；
//   - `beian2api doctor -badflag` 的用法跑到 stdout，和错误信息分了家。
//
// 两者的区别只有一处：结局是「求帮助」还是「出错了」。所以这里把 flag 包的
// 全部输出先攒进 buffer，等 Parse 返回后再按结局分别投递——一个入口，
// 两条路各归各位，子命令里只需要写 `if err := parseFlags(fs, args); err != nil`。
func newFlags(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ContinueOnError)
}

// errHelpShown 表示「帮助已经打完了，别再往下跑」。
//
// 不能简单 return nil：`serve -h` 之后会继续加载配置、开监听——
// 一个只想看用法的人会得到一个在前台跑着的服务，还以为命令卡住了。
var errHelpShown = errors.New("help shown")

// parseFlags 解析参数，并把帮助 / 错误分别投递到 stdout 与 stderr。
func parseFlags(fs *flag.FlagSet, args []string) error {
	var buf strings.Builder
	fs.SetOutput(&buf)
	fs.Usage = func() {
		fmt.Fprintf(&buf, "用法: beian2api %s [参数]\n", fs.Name())
		fs.PrintDefaults()
	}
	err := fs.Parse(args)
	if buf.Len() == 0 {
		return err
	}
	if errors.Is(err, flag.ErrHelp) {
		// 求帮助不是失败：用法去 stdout，再用「已处理」信号让调用方停下。
		fmt.Fprint(os.Stdout, buf.String())
		return errHelpShown
	}
	// 参数写错：错误信息与用法一起落到 stderr，别混进管道里的数据。
	fmt.Fprint(os.Stderr, buf.String())
	return err
}

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
	st.WithRetention(cfg.Storage.RetainChanges, cfg.Storage.RetainHistory).
		WithHistoryTotal(cfg.Storage.RetainHistoryTotal).
		WithStaleWindow(cfg.Cache.StaleMax)
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
		inst, err := reg.Build(p.Name, provider.Options{
			Timeout: timeout,
			HTTP:    provider.NewHTTPClient(timeout),
			ID:      p.ID,
			Key:     p.Key,
		})
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
	fs := newFlags("serve")
	cfgPath := fs.String("config", "", "配置文件路径")
	fs.StringVar(cfgPath, "c", "", "配置文件路径")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	cfg, st, err := bootstrap(*cfgPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// 按配置整理一次历史和缓存。放在这里而不是 store.Open：
	// 打开文件时配置还没加载，只能拿默认上限去裁，等于把
	// 「retain_history: 0（不裁剪）」的部署每次重启都砍回 200 条。
	if err := st.Prepare(); err != nil {
		return err
	}

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
				} else {
					if r.ChangesTrimmed > 0 {
						log.Info("变更流水裁剪完成", "changes", r.ChangesTrimmed)
					}
					if r.CachePurged > 0 {
						log.Info("过期缓存清理完成", "purged", r.CachePurged)
					}
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
		"auth", cfg.Auth.Enabled,
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
	fs := newFlags("config")
	cfgPath := fs.String("config", "", "配置文件路径")
	fs.StringVar(cfgPath, "c", "", "配置文件路径")
	if err := parseFlags(fs, args); err != nil {
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
