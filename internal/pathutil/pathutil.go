package pathutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

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

func containsAny(s string, refs []string) bool {
	for _, ref := range refs {
		if strings.Contains(s, ref) {
			return true
		}
	}
	return false
}
