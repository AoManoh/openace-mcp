package index

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// deadPID 返回一个刚退出的子进程的进程号，用来模拟持有者已死。
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	return cmd.Process.Pid
}

// TestAcquireLockAtNote（T4.1）：锁文件在身份行之后多写一行说明，供状态显示持有者；
// 互斥、复验、接管与释放只看身份行。
func TestAcquireLockAtNote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slot-0.lock")
	lock, err := AcquireLockAt(path, "workspace=/srv/a build=full\nignored")
	if err != nil {
		t.Fatal(err)
	}
	holder, err := ReadLockHolder(path)
	if err != nil || holder.PID != os.Getpid() || holder.Note != "workspace=/srv/a build=full ignored" || !strings.HasPrefix(holder.Identity, "pid=") {
		t.Fatalf("锁文件应是身份行加一行说明（换行折成空格）: %+v err=%v", holder, err)
	}
	if err := lock.Verify(); err != nil {
		t.Fatalf("带说明行的锁应复验通过: %v", err)
	}
	if _, err := AcquireLockAt(path, "workspace=/srv/b"); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("第二个获取者应被拒绝: %v", err)
	}
	lock.Release()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("释放后锁文件应删除: %v", err)
	}

	// 持有者进程已死：立即接管，说明行换成新持有者的。
	dead := "pid=" + strconv.Itoa(deadPID(t)) + " rand=dead\nworkspace=/srv/old\n"
	if err := os.WriteFile(path, []byte(dead), filePerm); err != nil {
		t.Fatal(err)
	}
	taken, err := AcquireLockAt(path, "workspace=/srv/new")
	if err != nil {
		t.Fatalf("持有者已死时应立即接管: %v", err)
	}
	defer taken.Release()
	if holder, err := ReadLockHolder(path); err != nil || holder.PID != os.Getpid() || holder.Note != "workspace=/srv/new" {
		t.Fatalf("接管后应是新持有者的身份与说明: %+v err=%v", holder, err)
	}
	if err := taken.Verify(); err != nil {
		t.Fatalf("接管后应复验通过: %v", err)
	}

	// 不带说明的锁（AcquireLock）文件内容仍只有身份行。
	store := newV2TestStore(t)
	plain, err := AcquireLock(store)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Release()
	raw, err := os.ReadFile(store.LockPath())
	if err != nil || strings.Count(string(raw), "\n") != 1 {
		t.Fatalf("不带说明的锁文件应只有一行: %q err=%v", raw, err)
	}
}

func newTestSlots(t *testing.T, count int) *HeavyBuildSlots {
	t.Helper()
	return NewHeavyBuildSlots(filepath.Join(t.TempDir(), "heavy"), count)
}

// TestHeavySlotExclusive（T4.2）：两个不同身份争 1 个名额，第二个等待，第一个释放后获得。
func TestHeavySlotExclusive(t *testing.T) {
	slots := newTestSlots(t, 1)
	release, err := slots.TryAcquire("workspace=/srv/a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := slots.TryAcquire("workspace=/srv/b"); !errors.Is(err, ErrHeavySlotsBusy) {
		t.Fatalf("名额被占时应返回 ErrHeavySlotsBusy: %v", err)
	}
	got := make(chan error, 1)
	go func() {
		second, err := slots.Acquire(context.Background(), "workspace=/srv/b")
		if err == nil {
			second()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("名额释放前不应获得: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("释放后等待者应获得名额: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("释放后等待者没有在 5 秒内获得名额")
	}
	if holders := slots.Holders(); len(holders) != 0 {
		t.Fatalf("全部释放后不应有持有者: %+v", holders)
	}
}

// TestHeavySlotDeadHolder：持有者进程已死时立即接管，不等心跳超时。
func TestHeavySlotDeadHolder(t *testing.T) {
	slots := newTestSlots(t, 1)
	if err := os.MkdirAll(slots.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	dead := "pid=" + strconv.Itoa(deadPID(t)) + " rand=dead\nworkspace=/srv/old\n"
	if err := os.WriteFile(filepath.Join(slots.Dir(), "slot-0.lock"), []byte(dead), filePerm); err != nil {
		t.Fatal(err)
	}
	release, err := slots.TryAcquire("workspace=/srv/new")
	if err != nil {
		t.Fatalf("持有者已死时应立即接管: %v", err)
	}
	release()
}

// TestHeavySlotCancel：等待期间 ctx 取消时返回 context.Canceled，不占名额。
func TestHeavySlotCancel(t *testing.T) {
	slots := newTestSlots(t, 1)
	release, err := slots.TryAcquire("workspace=/srv/a")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		second, err := slots.Acquire(ctx, "workspace=/srv/b")
		if err == nil {
			second()
		}
		got <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-got:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("取消后应返回 context.Canceled: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后等待者没有在 5 秒内返回")
	}
	if holders := slots.Holders(); len(holders) != 1 || holders[0].Note != "workspace=/srv/a" {
		t.Fatalf("取消的等待者不应占名额: %+v", holders)
	}
}

// TestHeavySlotHolders：Holders 列出每个被占名额的进程号与说明；名额数大于 1 时可同时持有。
func TestHeavySlotHolders(t *testing.T) {
	slots := newTestSlots(t, 2)
	first, err := slots.TryAcquire("workspace=/srv/a build=full")
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	second, err := slots.TryAcquire("workspace=/srv/b build=delta")
	if err != nil {
		t.Fatalf("两个名额时第二个也应获得: %v", err)
	}
	defer second()
	if _, err := slots.TryAcquire("workspace=/srv/c"); !errors.Is(err, ErrHeavySlotsBusy) {
		t.Fatalf("两个名额都被占时第三个应等待: %v", err)
	}
	holders := slots.Holders()
	if len(holders) != 2 || holders[0].Slot != 0 || holders[0].PID != os.Getpid() || holders[0].Note != "workspace=/srv/a build=full" ||
		holders[1].Slot != 1 || holders[1].Note != "workspace=/srv/b build=delta" {
		t.Fatalf("Holders 应按名额序列出进程号与说明: %+v", holders)
	}
}

// TestHeavySlotPrivateDir：名额目录建成 0700；已有目录权限过宽时收回到 0700；目录是符号链接
// 时拒绝使用。
func TestHeavySlotPrivateDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不按 Unix 权限位判断")
	}
	slots := newTestSlots(t, 1)
	release, err := slots.TryAcquire("workspace=/srv/a")
	if err != nil {
		t.Fatal(err)
	}
	release()
	info, err := os.Stat(slots.Dir())
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("名额目录应为 0700: %v %v", info.Mode(), err)
	}
	if err := os.Chmod(slots.Dir(), 0o777); err != nil {
		t.Fatal(err)
	}
	release, err = slots.TryAcquire("workspace=/srv/a")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if info, err := os.Stat(slots.Dir()); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("权限过宽的名额目录应收回到 0700: %v %v", info.Mode(), err)
	}
	base := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(base, "heavy")); err != nil {
		t.Fatal(err)
	}
	linked := NewHeavyBuildSlots(filepath.Join(base, "heavy"), 1)
	if _, err := linked.TryAcquire("workspace=/srv/a"); err == nil || errors.Is(err, ErrHeavySlotsBusy) {
		t.Fatalf("名额目录是符号链接时应拒绝使用: %v", err)
	}
}

// TestDefaultHeavyBuildDir：默认目录在系统临时目录下，按用户区分。
func TestDefaultHeavyBuildDir(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp/openace-heavy-test-root")
	dir := DefaultHeavyBuildDir()
	if filepath.Dir(dir) != os.TempDir() || !strings.HasPrefix(filepath.Base(dir), "openace-heavy-build") {
		t.Fatalf("默认目录应为临时目录下的 openace-heavy-build-<uid>: %s", dir)
	}
	if uid := os.Getuid(); uid >= 0 && !strings.HasSuffix(dir, "-"+strconv.Itoa(uid)) {
		t.Fatalf("默认目录应带用户号: %s", dir)
	}
}
