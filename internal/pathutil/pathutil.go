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
	root.CanonicalPath = abs
	return root, nil
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
