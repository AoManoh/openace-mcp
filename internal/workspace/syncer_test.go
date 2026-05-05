package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateFileUsesOpenACECacheDir(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", cacheDir)

	path, err := stateFile(filepath.Join("tmp", "workspace"))
	if err != nil {
		t.Fatal(err)
	}

	wantPrefix := filepath.Join(cacheDir, "workspaces") + string(os.PathSeparator)
	if !strings.HasPrefix(path, wantPrefix) {
		t.Fatalf("state file %q does not use OPENACE_CACHE_DIR prefix %q", path, wantPrefix)
	}
	if filepath.Ext(path) != ".json" {
		t.Fatalf("state file should be json: %q", path)
	}
}
