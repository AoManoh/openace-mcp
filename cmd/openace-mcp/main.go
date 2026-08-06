package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/AoManoh/openace-mcp/internal/buildinfo"
	"github.com/AoManoh/openace-mcp/internal/daemon"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/localengine"
	"github.com/AoManoh/openace-mcp/internal/managed"
	"github.com/AoManoh/openace-mcp/internal/mcp"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "daemon":
			runDaemon()
			return
		case "version", "-version", "--version", "-v":
			// 灰度反馈一号 2.2-2:核对版本此前只能裸跑二进制,而裸跑
			// 进 MCP 模式会在 auto 档隐式拉起 daemon(且带着当时可能
			// 残缺的 provider env,产出错误 engine profile 的野 daemon)。
			// version 子命令零副作用:不连、不拉起任何进程。
			fmt.Println(versionString())
			return
		}
	}

	ctx := context.Background()

	service, err := buildService(ctx)
	if err != nil {
		// 灰度反馈一号 P1-3:启动失败不再 exit(1) 了事——多数 MCP 客户端
		// 不展示 stderr,调用方只见裸 "Failed to connect"。降级形态保持
		// 会话,把 build/profile mismatch 等可行动文案经工具错误透传;
		// 并按 TTL 惰性重探测(灰度反馈三 C.4),daemon 修好后本会话自愈。
		fmt.Fprintf(os.Stderr, "openace-mcp: %v\n", err)
		reconnect := func() (engine.Service, error) { return buildService(ctx) }
		if runErr := mcp.NewUnavailableServer(err, reconnect).Run(ctx, os.Stdin, os.Stdout); runErr != nil {
			fmt.Fprintf(os.Stderr, "openace-mcp: %v\n", runErr)
		}
		os.Exit(1)
	}
	server := mcp.NewServer(service)

	runErr := server.Run(ctx, os.Stdin, os.Stdout)
	// direct 模式下引擎持有本地句柄，退出前有序释放（review S7，
	// 与 daemon Shutdown 路径对称）。
	if lifecycle, ok := service.(engine.Lifecycle); ok {
		if err := lifecycle.Close(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "openace-mcp: close engine: %v\n", err)
		}
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "openace-mcp: %v\n", runErr)
		os.Exit(1)
	}
}

// versionString 输出构建身份(与 daemon 启动日志、daemon_status 同源)。
func versionString() string {
	build := buildinfo.Current()
	return fmt.Sprintf("openace-mcp %s (%s)", build.Version, build.VCSRevision)
}

func buildService(ctx context.Context) (engine.Service, error) {
	switch openaceMode() {
	case "direct":
		return buildLocalService(ctx)
	case "manual-daemon":
		return daemon.NewClient(daemonAddr()), nil
	case "auto":
		return managed.Connect(ctx)
	default:
		return nil, fmt.Errorf("invalid OPENACE_MODE %q; use auto, direct, or manual-daemon", os.Getenv("OPENACE_MODE"))
	}
}

func openaceMode() string {
	mode := strings.TrimSpace(strings.ToLower(os.Getenv("OPENACE_MODE")))
	switch mode {
	case "", "auto", "managed", "managed-daemon":
		return "auto"
	case "direct":
		return "direct"
	case "manual", "daemon", "manual-daemon":
		return "manual-daemon"
	default:
		return mode
	}
}

func daemonAddr() string {
	if addr := strings.TrimSpace(os.Getenv("OPENACE_DAEMON_ADDR")); addr != "" {
		return addr
	}
	if addr := strings.TrimSpace(os.Getenv("OPENACE_DAEMON_LISTEN_ADDR")); addr != "" {
		return addr
	}
	return daemon.DefaultAddr
}

func buildLocalService(ctx context.Context) (engine.Service, error) {
	_ = ctx
	// Stage 7:legacy ACE 引擎已删除,唯一引擎 = local-hybrid;
	// NormalizeEngineID 对 "ace" 给出可行动错误。零凭据可启动
	// (缺 key = semantic off,词法照常,阶段计划 D1)。
	if _, err := engine.NormalizeEngineID(os.Getenv("OPENACE_ENGINE")); err != nil {
		return nil, err
	}
	opts, err := localengine.OptionsFromEnv()
	if err != nil {
		return nil, err
	}
	return localengine.New(opts)
}

func runDaemon() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := os.Getenv("OPENACE_DAEMON_LISTEN_ADDR")
	if addr == "" {
		addr = os.Getenv("OPENACE_DAEMON_ADDR")
	}
	if addr == "" {
		addr = daemon.DefaultAddr
	}

	service, err := buildLocalService(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "openace-daemon: %v\n", err)
		os.Exit(1)
	}
	server := daemon.NewServer(service)

	fmt.Fprintf(os.Stderr, "openace-daemon: listening on %s\n", addr)
	if err := server.ListenAndServe(ctx, addr); err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "openace-daemon: %v\n", err)
		os.Exit(1)
	}
}
