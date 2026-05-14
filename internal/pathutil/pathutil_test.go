package pathutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestExpandUser_Empty(t *testing.T) {
	if _, err := ExpandUser(""); err == nil {
		t.Fatal("expected error for empty path")
	}
	if _, err := ExpandUser("   "); err == nil {
		t.Fatal("expected error for whitespace-only path")
	}
}

func TestExpandUser_TildeShorthand(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("UserHomeDir unavailable: %v", err)
	}

	got, err := ExpandUser("~")
	if err != nil {
		t.Fatalf("expand ~: %v", err)
	}
	if got != home {
		t.Fatalf("~ -> %q, want %q", got, home)
	}

	got, err = ExpandUser("~/.cache/openace-mcp")
	if err != nil {
		t.Fatalf("expand ~/...: %v", err)
	}
	want := filepath.Join(home, ".cache", "openace-mcp")
	if got != want {
		t.Fatalf("~/... -> %q, want %q", got, want)
	}
}

func TestExpandUser_DollarHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("UserHomeDir unavailable: %v", err)
	}

	cases := []string{"$HOME/.cache/openace-mcp", "${HOME}/.cache/openace-mcp"}
	for _, in := range cases {
		got, err := ExpandUser(in)
		if err != nil {
			t.Fatalf("expand %q: %v", in, err)
		}
		want := filepath.Join(home, ".cache", "openace-mcp")
		// Keep separator-agnostic comparison via filepath.Clean.
		if filepath.Clean(got) != filepath.Clean(want) {
			t.Fatalf("expand %q -> %q, want %q", in, got, want)
		}
	}
}

func TestExpandUser_DollarHomeWhenEnvUnset(t *testing.T) {
	// Simulate Windows IDEs where HOME is not present in the environment but
	// USERPROFILE / UserHomeDir() still resolve to the user's profile.
	t.Setenv("HOME", "")

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("UserHomeDir unavailable: %v", err)
	}

	got, err := ExpandUser("$HOME/.cache/openace-mcp")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	want := filepath.Join(home, ".cache", "openace-mcp")
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("expand -> %q, want %q", got, want)
	}
}

func TestExpandUser_WindowsPlaceholders(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("UserHomeDir unavailable: %v", err)
	}

	cases := []string{
		`%USERPROFILE%/.cache/openace-mcp`,
		`%HOME%/.cache/openace-mcp`,
	}
	for _, in := range cases {
		got, err := ExpandUser(in)
		if err != nil {
			t.Fatalf("expand %q: %v", in, err)
		}
		want := filepath.Join(home, ".cache", "openace-mcp")
		if filepath.Clean(got) != filepath.Clean(want) {
			t.Fatalf("expand %q -> %q, want %q", in, got, want)
		}
	}
}

func TestExpandUser_PassthroughForLiteralPath(t *testing.T) {
	in := filepath.Join(t.TempDir(), "literal", "subdir")
	got, err := ExpandUser(in)
	if err != nil {
		t.Fatalf("expand literal: %v", err)
	}
	if got != in {
		t.Fatalf("literal -> %q, want %q", got, in)
	}
}

func TestExpandUser_GenericEnvVar(t *testing.T) {
	if runtime.GOOS == "windows" {
		// $VAR style is uncommon on Windows; the helper still supports it via
		// os.ExpandEnv, but we only assert the cross-platform behaviour.
	}
	t.Setenv("OPENACE_TEST_DIR", "/tmp/openace-test")
	got, err := ExpandUser("$OPENACE_TEST_DIR/cache")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	want := filepath.FromSlash("/tmp/openace-test/cache")
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("expand -> %q, want %q", got, want)
	}
}

func TestResolveWorkspaceRootForLinuxKeepsWSLMountPath(t *testing.T) {
	root, err := ResolveWorkspaceRootForOS("/mnt/d/Go-Project/GoZero-AI", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if root.CanonicalPath != "/mnt/d/Go-Project/GoZero-AI" {
		t.Fatalf("canonical path = %q", root.CanonicalPath)
	}
	if root.PathKind != WorkspacePathNative || root.HostOS != "linux" {
		t.Fatalf("unexpected root metadata: %+v", root)
	}
}

func TestResolveWorkspaceRootForWindowsTranslatesWSLMountPath(t *testing.T) {
	root, err := ResolveWorkspaceRootForOS("/mnt/d/Go-Project/GoZero-AI", "windows")
	if err != nil {
		t.Fatal(err)
	}
	if root.CanonicalPath != `D:\Go-Project\GoZero-AI` {
		t.Fatalf("canonical path = %q", root.CanonicalPath)
	}
	if root.PathKind != WorkspacePathWSLMount || root.HostOS != "windows" {
		t.Fatalf("unexpected root metadata: %+v", root)
	}
}

func TestResolveWorkspaceRootForWindowsNormalizesDrivePath(t *testing.T) {
	root, err := ResolveWorkspaceRootForOS(`d:/Go-Project/../GoZero-AI`, "Windows")
	if err != nil {
		t.Fatal(err)
	}
	if root.CanonicalPath != `D:\GoZero-AI` {
		t.Fatalf("canonical path = %q", root.CanonicalPath)
	}
	if root.PathKind != WorkspacePathNative {
		t.Fatalf("unexpected path kind: %+v", root)
	}
}

func TestResolveWorkspaceRootForWindowsRejectsOtherPOSIXPath(t *testing.T) {
	_, err := ResolveWorkspaceRootForOS("/home/aomanoh/project", "windows")
	if err == nil {
		t.Fatal("expected Windows daemon to reject non-WSL POSIX path")
	}
}

func TestResolveWorkspaceRootForWindowsRejectsDriveRelativePath(t *testing.T) {
	_, err := ResolveWorkspaceRootForOS(`D:GoZero-AI`, "windows")
	if err == nil {
		t.Fatal("expected Windows daemon to reject drive-relative workspace path")
	}
}
