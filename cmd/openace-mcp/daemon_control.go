package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/AoManoh/openace-mcp/internal/managed"
)

// runDaemonControl 处理 `openace-mcp daemon stop|resume`，返回进程退出码。
// 不带子命令的 `openace-mcp daemon` 仍是运行 daemon（managed wrapper 拉起
// daemon 用的就是这个形式），由 main 分流。
//   - stop [--reason 文本]：写停用标记并停掉 managed 地址上的 daemon；标记存在
//     期间各会话的 wrapper 不再自动拉起 daemon，检索返回写明停用原因与解除
//     命令的错误。
//   - resume：删除停用标记；下一次 openACE 调用按原流程拉起 daemon。
func runDaemonControl(args []string) int {
	return daemonControl(args, os.Stdout, os.Stderr)
}

func daemonControl(args []string, stdout io.Writer, stderr io.Writer) int {
	switch args[0] {
	case "stop":
		fs := flag.NewFlagSet("openace-mcp daemon stop", flag.ContinueOnError)
		fs.SetOutput(stderr)
		reason := fs.String("reason", "", "why the daemon is stopped (shown in the error wrappers return)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		result, err := managed.StopDaemon(ctx, *reason)
		if result.HoldPath != "" {
			fmt.Fprintf(stdout, "stop marker written: %s\n", result.HoldPath)
		}
		if err != nil {
			fmt.Fprintf(stderr, "openace-mcp daemon stop: %v\n", err)
			return 1
		}
		if result.StoppedPID > 0 {
			fmt.Fprintf(stdout, "stopped openACE daemon pid %d at %s\n", result.StoppedPID, result.Addr)
		} else {
			fmt.Fprintf(stdout, "no openACE daemon was running at %s\n", result.Addr)
		}
		fmt.Fprintln(stdout, "wrappers will not start the daemon until `openace-mcp daemon resume` is run")
		return 0
	case "resume":
		if len(args) > 1 {
			fmt.Fprintf(stderr, "openace-mcp daemon resume: unexpected arguments %q\n", args[1:])
			return 2
		}
		path, existed, err := managed.ResumeDaemon()
		if err != nil {
			fmt.Fprintf(stderr, "openace-mcp daemon resume: %v\n", err)
			return 1
		}
		if existed {
			fmt.Fprintf(stdout, "stop marker removed: %s\n", path)
		} else {
			fmt.Fprintf(stdout, "no stop marker at %s\n", path)
		}
		fmt.Fprintln(stdout, "the next openACE call starts the daemon as usual")
		return 0
	default:
		fmt.Fprintf(stderr, "openace-mcp daemon: unknown subcommand %q; use `openace-mcp daemon` to run the daemon, or `stop` / `resume`\n", args[0])
		return 2
	}
}
