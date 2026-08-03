package pathutil

import (
	"errors"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strings"
)

type WorkspacePathKind string

const (
	WorkspacePathNative   WorkspacePathKind = "native"
	WorkspacePathWSLMount WorkspacePathKind = "wsl_mount"
)

// WorkspaceRoot is the single boundary object for user supplied workspace
// roots. Business packages should key state/watch/sync by CanonicalPath, not by
// ad-hoc filepath.Abs calls.
type WorkspaceRoot struct {
	InputPath     string            `json:"input_path"`
	CanonicalPath string            `json:"canonical_path"`
	PathKind      WorkspacePathKind `json:"path_kind"`
	HostOS        string            `json:"host_os"`
}

// ExpandUser resolves user-friendly path forms that may appear in environment
// variables passed to openACE, including:
//
//   - leading "~" or "~/" / "~\\" tilde shorthand
//   - POSIX shell placeholders "$HOME" and "${HOME}"
//   - Windows shell placeholders "%USERPROFILE%" and "%HOME%"
//   - any remaining "$VAR" / "${VAR}" references resolvable from the process
//     environment
//
// AI IDEs typically launch MCP child processes directly (no shell), so values
// like "$HOME/.cache/openace-mcp" reach our process verbatim. Without this
// helper, filepath.Abs would treat them as a literal directory name and end up
// somewhere like "C:\\software\\Windsurf\\$HOME\\.cache\\openace-mcp", which
// triggers "Access is denied" on Windows.
func ExpandUser(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("empty path")
	}

	if path == "~" {
		return os.UserHomeDir()
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, path[2:]), nil
	}

	// Order matters: replace ${HOME} before $HOME so the shorter form does not
	// chew through the longer one.
	homeRefs := []string{"${HOME}", "$HOME", "%USERPROFILE%", "%HOME%"}
	if containsAny(path, homeRefs) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		for _, ref := range homeRefs {
			path = strings.ReplaceAll(path, ref, home)
		}
	}

	// Resolve remaining $VAR / ${VAR} references using the live environment.
	// os.ExpandEnv leaves unknown variables empty; that is acceptable here,
	// since the most common offender ($HOME) is already handled above.
	path = os.ExpandEnv(path)
	return path, nil
}

// ResolveWorkspaceRoot normalizes user-supplied workspace roots before they
// become state keys or filesystem scan roots.
func ResolveWorkspaceRoot(path string) (WorkspaceRoot, error) {
	return ResolveWorkspaceRootForOS(path, runtime.GOOS)
}

func ResolveWorkspaceRootForOS(input string, goos string) (WorkspaceRoot, error) {
	path := strings.TrimSpace(input)
	if path == "" {
		return WorkspaceRoot{}, errors.New("empty path")
	}
	hostOS := strings.TrimSpace(strings.ToLower(goos))
	root := WorkspaceRoot{
		InputPath: path,
		PathKind:  WorkspacePathNative,
		HostOS:    hostOS,
	}
	if hostOS == "windows" {
		if converted, ok := wslMountToWindowsPath(path); ok {
			root.CanonicalPath = converted
			root.PathKind = WorkspacePathWSLMount
			return root, nil
		}
		if converted, ok := normalizeWindowsDrivePath(path); ok {
			root.CanonicalPath = converted
			return root, nil
		}
		if hasWindowsDrivePrefix(path) {
			return WorkspaceRoot{}, errors.New("workspace path uses a drive-relative Windows path; use an absolute path like D:\\project")
		}
		if strings.HasPrefix(strings.ReplaceAll(path, `\`, "/"), "/") {
			return WorkspaceRoot{}, errors.New("workspace path uses POSIX syntax but the daemon is running on Windows; use a Windows path like D:\\project or run a WSL daemon")
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return WorkspaceRoot{}, err
	}
	root.CanonicalPath = canonicalizeExistingPath(abs, hostOS)
	return root, nil
}

// canonicalizeExistingPath 对当前主机上真实存在的路径解析符号链接
// （H2：根为 symlink 时 filepath.WalkDir 对根用 Lstat，会把它当
// "非常规文件"跳过，扫描成功返回空集——local-hybrid 发布空 manifest、
// legacy 路径把远端索引当作全量删除。EvalSymlinks 让 CanonicalPath
// 落到真实目录，顺带消解同一物理目录多重身份的 state key 分裂，L11）。
//
// 契约边界：
//   - 路径不存在（或解析失败）时原样返回 filepath.Abs 结果——
//     ResolveWorkspaceRoot 必须继续支持不存在路径的纯词法规范化
//     （现有测试以不存在的 /mnt/d/... 断言 lexical 结果；daemon 对
//     已删除 workspace 查询状态也依赖该行为）；
//   - hostOS 与运行主机不一致时（跨 OS 单测传入 goos 参数的场景）
//     不做文件系统探测，保持纯词法语义，避免测试结果依赖宿主机文件布局。
//
// 副作用（已在诊断报告 §4-H2 声明）：symlink 用户的 CanonicalPath 由
// 链接路径变为真实路径 → workspaceKey 变化 → 旧索引子树成为孤儿，
// 属一次性重建成本。
func canonicalizeExistingPath(abs string, hostOS string) string {
	if hostOS != runtime.GOOS {
		return abs
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs
	}
	return resolved
}

func wslMountToWindowsPath(input string) (string, bool) {
	cleaned := pathpkg.Clean(strings.ReplaceAll(strings.TrimSpace(input), `\`, "/"))
	parts := strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
	if len(parts) < 2 || !strings.EqualFold(parts[0], "mnt") || len(parts[1]) != 1 {
		return "", false
	}
	drive := parts[1][0]
	if !isASCIILetter(drive) {
		return "", false
	}
	prefix := strings.ToUpper(string(drive)) + `:\`
	if len(parts) == 2 {
		return prefix, true
	}
	return prefix + strings.Join(parts[2:], `\`), true
}

func normalizeWindowsDrivePath(input string) (string, bool) {
	path := strings.ReplaceAll(strings.TrimSpace(input), "/", `\`)
	if len(path) < 2 || path[1] != ':' || !isASCIILetter(path[0]) {
		return "", false
	}
	if len(path) == 2 || path[2] != '\\' {
		return "", false
	}
	drive := strings.ToUpper(string(path[0])) + `:\`
	rest := strings.TrimPrefix(path[2:], `\`)
	if rest == "" {
		return drive, true
	}
	cleaned := pathpkg.Clean(strings.ReplaceAll(rest, `\`, "/"))
	if cleaned == "." {
		return drive, true
	}
	return drive + strings.ReplaceAll(cleaned, "/", `\`), true
}

func hasWindowsDrivePrefix(input string) bool {
	path := strings.TrimSpace(input)
	return len(path) >= 2 && path[1] == ':' && isASCIILetter(path[0])
}

func isASCIILetter(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func containsAny(s string, refs []string) bool {
	for _, ref := range refs {
		if strings.Contains(s, ref) {
			return true
		}
	}
	return false
}
