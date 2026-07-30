package localengine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/index"
)

// storeRootDir 定位工作区的 store 根目录（staging/segments/manifests/journal 的父目录）。
func storeRootDir(t *testing.T, cacheDir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(cacheDir, "*", "engines", "local-hybrid", "*", "*", "manifests"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("定位 store 根: %v matches=%v", err, matches)
	}
	return filepath.Dir(matches[0])
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// TestCrashRecoveryKillPoints 是 G4 的进程内注入矩阵：在 delta 链上模拟
// 各持久化窗口的中断残留，断言重启后自动恢复、无孤儿、下次 sync 收敛。
func TestCrashRecoveryKillPoints(t *testing.T) {
	points := []struct {
		name   string
		mutate func(t *testing.T, root string, storeRoot string)
	}{
		{
			// staging 残留（构建中 kill，K16 的 delta 链版本）。
			name: "staging-leftover",
			mutate: func(t *testing.T, _ string, storeRoot string) {
				dir := filepath.Join(storeRoot, "staging", "crash-leftover")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "chunks.jsonl"), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// segment 已 rename、manifest 未写（S5 的共享链版本）。
			name: "orphan-segment",
			mutate: func(t *testing.T, _ string, storeRoot string) {
				dir := filepath.Join(storeRoot, "segments", "orphan-crash")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "chunks.jsonl"), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// manifest 已写、active 指针未更新（发布窗口后半段）。
			name: "manifest-without-active",
			mutate: func(t *testing.T, _ string, storeRoot string) {
				manifests := filepath.Join(storeRoot, "manifests")
				names := listDir(t, manifests)
				if len(names) == 0 {
					t.Fatal("场景需要既有 manifest")
				}
				raw, err := os.ReadFile(filepath.Join(manifests, names[0]))
				if err != nil {
					t.Fatal(err)
				}
				var manifest index.Manifest
				if err := json.Unmarshal(raw, &manifest); err != nil {
					t.Fatal(err)
				}
				manifest.Revision = "rev-crash-unpointed"
				out, err := json.Marshal(&manifest)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(manifests, "rev-crash-unpointed.json"), out, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// GC 中断：manifest 已删、共享 segment 残留（K42 兜底窗口）。
			name: "gc-interrupted-orphan",
			mutate: func(t *testing.T, _ string, storeRoot string) {
				dir := filepath.Join(storeRoot, "segments", "gc-orphan")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// journal 坏尾（K40 的引擎级视角）。
			name: "journal-torn-tail",
			mutate: func(t *testing.T, _ string, storeRoot string) {
				path := filepath.Join(storeRoot, "journal", "vectors.journal")
				file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.Write([]byte{200, 0, 0, 0, 1, 2, 3}); err != nil {
					t.Fatal(err)
				}
				_ = file.Close()
			},
		},
	}

	for _, point := range points {
		t.Run(point.name, func(t *testing.T) {
			const dim = 8
			server := newEmbedServer(t, dim)
			cacheDir := t.TempDir()
			t.Setenv("OPENACE_CACHE_DIR", cacheDir)
			t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
			root := newFixtureWorkspace(t)
			opts := embedOptions(server.ts.URL, dim, 16, "fake-model")

			first, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := first.Sync(context.Background(), syncRequest(root)); err != nil {
				t.Fatal(err)
			}
			// 建一段 delta 链再注入（多段共享形态下的恢复）。
			writeFixture(t, root, "util.py", "def parse_config(path):\n    return 'crash-fixture'\n")
			if _, err := first.Sync(context.Background(), syncRequest(root)); err != nil {
				t.Fatal(err)
			}
			_ = first.Close(context.Background())
			storeRoot := storeRootDir(t, cacheDir)
			point.mutate(t, root, storeRoot)

			// "重启"：新引擎实例（NewStore 执行启动清理）。
			second, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = second.Close(context.Background()) })
			// 编辑触发下一次 sync：必须收敛到一致状态。
			writeFixture(t, root, "util.py", "def parse_config(path):\n    return 'recovered'\n")
			if _, err := second.Sync(context.Background(), syncRequest(root)); err != nil {
				t.Fatalf("恢复 sync: %v", err)
			}
			result, err := second.Search(context.Background(), searchRequest(root, "parse_config"))
			if err != nil || !strings.Contains(result.Text, "recovered") {
				t.Fatalf("恢复后检索: %v %q", err, result.Text)
			}
			// 无孤儿：staging 清空；segments 全部被 manifest 引用。
			if leftovers := listDir(t, filepath.Join(storeRoot, "staging")); len(leftovers) != 0 {
				t.Fatalf("staging 应清空: %v", leftovers)
			}
			store, err := index.NewStore(cacheDir, "test", "probe", "probe")
			_ = store
			if err != nil {
				t.Fatal(err)
			}
			referenced := map[string]bool{}
			for _, name := range listDir(t, filepath.Join(storeRoot, "manifests")) {
				raw, err := os.ReadFile(filepath.Join(storeRoot, "manifests", name))
				if err != nil {
					t.Fatal(err)
				}
				var manifest index.Manifest
				if err := json.Unmarshal(raw, &manifest); err != nil {
					t.Fatal(err)
				}
				manifest.Normalize()
				for _, segment := range manifest.Segments {
					referenced[segment.ID] = true
				}
			}
			for _, segment := range listDir(t, filepath.Join(storeRoot, "segments")) {
				if !referenced[segment] {
					t.Fatalf("孤儿 segment 残留（G4）: %s", segment)
				}
			}
		})
	}
}

// TestCrashRecoverySubprocessKill 是 G4/G2 的真实 kill -9 证据：子进程
// 构建中途被 SIGKILL，父进程在同一 cache 上恢复——已付费批次经 journal
// 复活零重付，最终覆盖完整、无孤儿残留（WSL /mnt/d 实测由测试环境决定）。
func TestCrashRecoverySubprocessKill(t *testing.T) {
	if os.Getenv("OPENACE_CRASH_HELPER") == "1" {
		crashHelperMain()
		return
	}
	const dim = 8
	var mu sync.Mutex
	texts := []string{}
	requestCount := 0
	proceed := make(chan struct{}, 64)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		requestCount++
		mu.Unlock()
		// 每个批次都等父进程放行：kill 窗口完全确定。
		<-proceed
		mu.Lock()
		texts = append(texts, req.Input...)
		mu.Unlock()
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		items := make([]item, 0, len(req.Input))
		for i, text := range req.Input {
			items = append(items, item{Embedding: fakeVector(dim, text), Index: i})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	defer ts.Close()

	cacheDir := t.TempDir()
	root := newFixtureWorkspace(t)

	helper := exec.Command(os.Args[0], "-test.run", "TestCrashRecoverySubprocessKill")
	helper.Env = append(os.Environ(),
		"OPENACE_CRASH_HELPER=1",
		"OPENACE_CRASH_URL="+ts.URL,
		"OPENACE_CRASH_ROOT="+root,
		"OPENACE_CACHE_DIR="+cacheDir,
		"OPENACE_CACHE_NAMESPACE=test",
	)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	// 放行 2 个批次（落 journal），第 3 批挂起时 SIGKILL。
	proceed <- struct{}{}
	proceed <- struct{}{}
	deadline := time.After(20 * time.Second)
	for {
		mu.Lock()
		count := requestCount
		mu.Unlock()
		if count >= 3 {
			break
		}
		select {
		case <-deadline:
			_ = helper.Process.Kill()
			t.Fatal("等待第 3 批超时")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	// 快照 kill 时刻已完成（已回包、helper 已 journal）的批次文本；
	// kill 时在飞的批次未获响应、不可能入 journal，恢复期重发是
	// 唯一正确语义，不计入重付。
	mu.Lock()
	completedAtKill := append([]string{}, texts...)
	paidBefore := len(completedAtKill)
	mu.Unlock()
	if paidBefore < 2 {
		t.Fatalf("kill 前应已有完成批次: %d", paidBefore)
	}
	// 放行后续全部批次（含被 kill 时挂起的在飞批次）。
	close(proceed)

	t.Setenv("OPENACE_CACHE_DIR", cacheDir)
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	recovered, err := New(embedOptions(ts.URL, dim, 1, "fake-model"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close(context.Background()) })
	if _, err := recovered.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatalf("kill 后恢复 sync: %v", err)
	}
	manifest, _ := loadActiveManifest(t, recovered, root)
	if !manifest.SemanticComplete() {
		t.Fatalf("恢复后覆盖应完整: %d/%d", manifest.VectorCount, manifest.Counts.Chunks)
	}
	// 零重复付费（G2/G4）：kill 时已完成的批次绝不重发。
	mu.Lock()
	defer mu.Unlock()
	completedSet := make(map[string]bool, len(completedAtKill))
	for _, text := range completedAtKill {
		completedSet[text] = true
	}
	for _, text := range texts[len(completedAtKill):] {
		if completedSet[text] {
			t.Fatalf("kill 前已付费内容被重发（违反 G2/G4）: %q", text[:min(40, len(text))])
		}
	}
	// 无孤儿残留。
	storeRoot := storeRootDir(t, cacheDir)
	if leftovers := listDir(t, filepath.Join(storeRoot, "staging")); len(leftovers) != 0 {
		t.Fatalf("staging 应清空: %v", leftovers)
	}
}

// TestCrossEngineWriteLockExclusive 是 G6 的跨实例互斥（D6/S15）：
// 两个引擎实例共享同一 cache 子树时，第二个实例的构建被显式拒绝而非
// 互踩；第一个实例关闭后所有权立即可转移。
func TestCrossEngineWriteLockExclusive(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", cacheDir)
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	root := newFixtureWorkspace(t)

	first, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}

	second, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	writeFixture(t, root, "util.py", "def parse_config(path):\n    return 'second-writer'\n")
	if _, err := second.Sync(context.Background(), syncRequest(root)); err == nil || !strings.Contains(err.Error(), "写锁") {
		t.Fatalf("第二实例构建应被写锁拒绝: %v", err)
	}
	// 只读检索不需要锁：第二实例仍可服务旧 revision。
	if result, err := second.Search(context.Background(), searchRequest(root, "HandleLogin")); err != nil || !strings.Contains(result.Text, "HandleLogin") {
		t.Fatalf("只读路径不应被锁拦截: %v", err)
	}

	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatalf("持有者退出后所有权应可转移: %v", err)
	}
	result, err := second.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil || !strings.Contains(result.Text, "second-writer") {
		t.Fatalf("接管后应可发布新内容: %v %q", err, result.Text)
	}
}

// crashHelperMain 是子进程入口：在指定 cache 上执行一次语义 sync，
// 直至被父进程 SIGKILL。
func crashHelperMain() {
	e, err := New(embedOptions(os.Getenv("OPENACE_CRASH_URL"), 8, 1, "fake-model"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := e.Sync(context.Background(), syncRequest(os.Getenv("OPENACE_CRASH_ROOT"))); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
