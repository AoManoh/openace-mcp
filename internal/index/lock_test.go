package index

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// TestLockExclusive 是 D6 基线：同一 store 的第二个获取者被显式拒绝。
func TestLockExclusive(t *testing.T) {
	store := newV2TestStore(t)
	first, err := AcquireLock(store)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if _, err := AcquireLock(store); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("第二个获取者应被拒绝: %v", err)
	}
	first.Release()
	second, err := AcquireLock(store)
	if err != nil {
		t.Fatalf("释放后应可获取: %v", err)
	}
	second.Release()
}

// TestLockStaleTakeover 是暗坑 K45：持有者死亡（心跳停止）后可被接管。
func TestLockStaleTakeover(t *testing.T) {
	store := newV2TestStore(t)
	// 模拟死亡持有者：手工写锁文件并把 mtime 拨回 stale 阈值之前。
	if err := tryCreateLock(store.LockPath(), "pid=99999 rand=deadbeef"); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * lockStaleAfter)
	if err := os.Chtimes(store.LockPath(), stale, stale); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireLock(store)
	if err != nil {
		t.Fatalf("stale 锁应可接管: %v", err)
	}
	defer lock.Release()
	if err := lock.Verify(); err != nil {
		t.Fatalf("接管后应持有: %v", err)
	}
}

// TestLockFreshNotTakenOver 是暗坑 K46 的反向：存活持有者的新鲜锁绝不
// 被抢（用本进程 PID 模拟存活持有者）。
func TestLockFreshNotTakenOver(t *testing.T) {
	store := newV2TestStore(t)
	identity := fmt.Sprintf("pid=%d rand=alive", os.Getpid())
	if err := tryCreateLock(store.LockPath(), identity); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(store); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("新鲜锁不得被接管: %v", err)
	}
}

// TestLockDeadHolderTakenOverImmediately 是 G4：持有进程已死（PID 探测）
// 时无需等满时间阈值即可接管。
func TestLockDeadHolderTakenOverImmediately(t *testing.T) {
	store := newV2TestStore(t)
	// PID 99999999 超出 pid_max，必然不存在。
	if err := tryCreateLock(store.LockPath(), "pid=99999999 rand=dead"); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireLock(store) // mtime 仍新鲜
	if err != nil {
		t.Fatalf("死持有者应立即被接管: %v", err)
	}
	lock.Release()
}

// TestLockLostAbortsHolder 是暗坑 K46：被接管的持有者复验必须失败。
func TestLockLostAbortsHolder(t *testing.T) {
	store := newV2TestStore(t)
	lock, err := AcquireLock(store)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	// 模拟接管：覆写锁身份。
	if err := os.WriteFile(store.LockPath(), []byte("pid=11111 rand=usurper\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lock.Verify(); !errors.Is(err, ErrLockLost) {
		t.Fatalf("失锁复验应报 ErrLockLost: %v", err)
	}
	// 失锁后 Release 不得误删新持有者的锁文件。
	lock.Release()
	content, err := os.ReadFile(store.LockPath())
	if err != nil || string(content) != "pid=11111 rand=usurper\n" {
		t.Fatalf("新持有者锁文件不得被误删: %q err=%v", content, err)
	}
}

// TestConcurrentTakeoverSingleWinner 是暗坑 K45：并发接管只有一个胜者。
func TestConcurrentTakeoverSingleWinner(t *testing.T) {
	store := newV2TestStore(t)
	if err := tryCreateLock(store.LockPath(), "pid=99999 rand=dead"); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * lockStaleAfter)
	if err := os.Chtimes(store.LockPath(), stale, stale); err != nil {
		t.Fatal(err)
	}
	const contenders = 8
	var wg sync.WaitGroup
	winners := make(chan *ProcessLock, contenders)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if lock, err := AcquireLock(store); err == nil {
				winners <- lock
			}
		}()
	}
	wg.Wait()
	close(winners)
	var held []*ProcessLock
	for lock := range winners {
		// rename 竞速的非胜者在回读裁决中已被拒绝；这里复验兜底。
		if lock.Verify() == nil {
			held = append(held, lock)
		}
		defer lock.Release()
	}
	if len(held) != 1 {
		t.Fatalf("并发接管应恰有一个胜者: %d", len(held))
	}
}
