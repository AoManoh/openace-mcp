package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// M5(诊断报告 2026-08-03,批准选项 A):daemon HTTP 面默认凭据。
// 多用户 Linux 上 127.0.0.1 为全体本地用户共享,零认证意味着他人可读取
// 已索引源码、提交任务。默认档自动生成随机 token 写 0600 状态文件,
// wrapper/client 自动读取——零配置用户无感;OPENACE_DAEMON_TOKEN=off
// 显式关闭(自担风险);显式 token 行为与历史一致。

// tokenModeOff 是显式关闭认证的哨兵值。
const tokenModeOff = "off"

// maxRequestBodyBytes 是单请求体上限(本地 DoS 面收敛;检索请求为短
// JSON,8MiB 远超正常需要)。
const maxRequestBodyBytes int64 = 8 << 20

// TokenFilePath 返回给定监听地址的 token 状态文件路径(与 managed 启动
// 锁同一命名口径,按地址隔离)。
func TokenFilePath(addr string) string {
	cache, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, strings.TrimSpace(addr))
	name = strings.Trim(name, ".-")
	if name == "" {
		name = "default"
	}
	return filepath.Join(cache, "openace-mcp", "daemon-token-"+name)
}

// LoadOrCreateToken 读取或原子创建 token 文件(0600)。返回路径与 token。
func LoadOrCreateToken(addr string) (string, string, error) {
	path := TokenFilePath(addr)
	if path == "" {
		return "", "", fmt.Errorf("无法定位用户缓存目录以存放 daemon token")
	}
	if data, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(data))
		if token != "" {
			return path, token, nil
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("生成 daemon token: %w", err)
	}
	token := hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", "", fmt.Errorf("创建 token 目录: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token+"\n"), 0o600); err != nil {
		return "", "", fmt.Errorf("写入 daemon token: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", "", fmt.Errorf("落盘 daemon token: %w", err)
	}
	// rename 竞态(并发首启)以文件现值为准:重读一次保证双方一致。
	if data, err := os.ReadFile(path); err == nil {
		if existing := strings.TrimSpace(string(data)); existing != "" {
			token = existing
		}
	}
	return path, token, nil
}

// resolveAuthToken 解析服务端认证档位:
//   - env=off → 零认证(显式选择);
//   - env 非空 → 显式 token(历史行为);
//   - env 未设 → 默认档:load-or-create 状态文件。文件不可用时 fail-closed
//     (返回错误让 daemon 拒绝启动,而非静默退回零认证)。
func resolveAuthToken() (string, error) {
	env := strings.TrimSpace(os.Getenv("OPENACE_DAEMON_TOKEN"))
	if strings.EqualFold(env, tokenModeOff) {
		return "", nil
	}
	if env != "" {
		return env, nil
	}
	_, token, err := LoadOrCreateToken(daemonAddrForToken())
	if err != nil {
		return "", fmt.Errorf("默认 token 档不可用(可显式设 OPENACE_DAEMON_TOKEN 或 =off 关闭): %w", err)
	}
	return token, nil
}

// daemonAddrForToken 取 token 文件命名用的监听地址(与 client 侧推导
// 一致:显式 env 优先,否则默认地址)。
func daemonAddrForToken() string {
	if addr := strings.TrimSpace(os.Getenv("OPENACE_DAEMON_LISTEN_ADDR")); addr != "" {
		return addr
	}
	if addr := strings.TrimSpace(os.Getenv("OPENACE_DAEMON_ADDR")); addr != "" {
		return addr
	}
	return DefaultAddr
}
