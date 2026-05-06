package managed

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AoManoh/openace-mcp/internal/daemon"
)

const defaultStartupTimeout = 10 * time.Second

func Connect(ctx context.Context) (*daemon.Client, error) {
	addr := daemonAddrFromEnv()
	client := daemon.NewClient(addr)
	if healthy(ctx, client) {
		return client, nil
	}
	managedAddr := managedDaemonAddr(addr)
	managedClient := daemon.NewClient(managedAddr)
	if managedAddr != addr && healthy(ctx, managedClient) {
		return managedClient, nil
	}
	logPath, err := startDaemon(managedAddr)
	if err != nil {
		return nil, err
	}
	if err := waitReady(ctx, managedClient, startupTimeout()); err != nil {
		return nil, withDaemonLog(err, logPath)
	}
	return managedClient, nil
}

func daemonAddrFromEnv() string {
	if addr := strings.TrimSpace(os.Getenv("OPENACE_DAEMON_ADDR")); addr != "" {
		return addr
	}
	if addr := strings.TrimSpace(os.Getenv("OPENACE_DAEMON_LISTEN_ADDR")); addr != "" {
		return addr
	}
	return daemon.DefaultAddr
}

func listenAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return daemon.DefaultAddr
	}
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		parsed, err := url.Parse(addr)
		if err == nil && parsed.Host != "" {
			return parsed.Host
		}
	}
	return strings.TrimRight(addr, "/")
}

func managedDaemonAddr(addr string) string {
	return listenAddr(addr)
}

func startupTimeout() time.Duration {
	value := strings.TrimSpace(os.Getenv("OPENACE_DAEMON_START_TIMEOUT"))
	if value == "" {
		return defaultStartupTimeout
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout <= 0 {
		return defaultStartupTimeout
	}
	return timeout
}

func healthy(ctx context.Context, client *daemon.Client) bool {
	healthCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	return client.Health(healthCtx) == nil
}

func waitReady(ctx context.Context, client *daemon.Client, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		healthCtx, healthCancel := context.WithTimeout(waitCtx, 500*time.Millisecond)
		err := client.Health(healthCtx)
		healthCancel()
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("managed daemon did not become ready within %s: %w", timeout, lastErr)
		case <-ticker.C:
		}
	}
}

func startDaemon(addr string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve current executable: %w", err)
	}
	cmd := exec.Command(exe, "daemon")
	cmd.Env = upsertEnv(os.Environ(), "OPENACE_DAEMON_LISTEN_ADDR", listenAddr(addr))
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	logFile, logPath := daemonLogFile()
	if logFile != nil {
		defer logFile.Close()
		cmd.Stderr = logFile
	} else {
		cmd.Stderr = io.Discard
	}
	if err := cmd.Start(); err != nil {
		return logPath, fmt.Errorf("start managed daemon: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		return logPath, fmt.Errorf("release managed daemon: %w", err)
	}
	return logPath, nil
}

func daemonLogFile() (*os.File, string) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, ""
	}
	dir := filepath.Join(cache, "openace-mcp", "daemon-logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, ""
	}
	file, err := os.CreateTemp(dir, "managed-daemon-*.log")
	if err != nil {
		return nil, ""
	}
	return file, file.Name()
}

func withDaemonLog(err error, path string) error {
	tail := daemonLogTail(path, 4096)
	if tail == "" {
		return err
	}
	return fmt.Errorf("%w; managed daemon stderr: %s", err, tail)
}

func daemonLogTail(path string, max int) string {
	if path == "" || max <= 0 {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) > max {
		data = data[len(data)-max:]
	}
	return strings.TrimSpace(string(data))
}

func upsertEnv(env []string, key string, value string) []string {
	prefix := key + "="
	for i, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
