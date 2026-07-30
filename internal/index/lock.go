package index

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// 跨进程构建锁（阶段计划 D6，暗坑 K45/K46）。
//
// WSL /mnt/d（9p/drvfs）上 flock 语义不可靠，因此不用系统锁：
//   - 获取：O_EXCL 创建 owner.lock（内容 = 持有者身份）；
//   - 保活：心跳 goroutine 周期刷新 mtime，并复验文件内容仍是自己；
//   - 接管：mtime 超过 stale 阈值视为持有者已死，temp+rename 原子抢占，
//     rename 后回读内容做最终裁决（并发接管只有一个胜者）；
//   - 复验：持有者每次关键写入前调用 Verify，失锁即中止（K46）。
//
// 查询只读不需锁；锁只约束构建/GC/journal 等写路径。
const (
	lockFileName          = "owner.lock"
	lockHeartbeatInterval = 10 * time.Second
)

// lockStaleAfter 是接管阈值（心跳间隔 ≪ 阈值，K46）；var 仅为测试可缩短。
var lockStaleAfter = 60 * time.Second

// ErrLockHeld 表示锁被其他存活进程持有。
var ErrLockHeld = errors.New("索引写锁被其他进程持有")

// ErrLockLost 表示锁已被接管（持有者必须中止写入）。
var ErrLockLost = errors.New("索引写锁已被其他进程接管")

// ProcessLock 是 profile 子树的跨进程写独占锁。
type ProcessLock struct {
	path     string
	identity string
	lost     atomic.Bool
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// LockPath 返回 store 的锁文件路径。
func (s *Store) LockPath() string {
	return filepath.Join(s.root, lockFileName)
}

func newLockIdentity() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("pid=%d rand=%s", os.Getpid(), hex.EncodeToString(buf))
}

// AcquireLock 获取 store 的写锁；被存活进程持有时返回 ErrLockHeld。
func AcquireLock(store *Store) (*ProcessLock, error) {
	path := store.LockPath()
	identity := newLockIdentity()
	if err := tryCreateLock(path, identity); err != nil {
		if !os.IsExist(err) {
			return nil, err
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				// 持有者恰好释放：重试一次创建。
				if retryErr := tryCreateLock(path, identity); retryErr != nil {
					return nil, ErrLockHeld
				}
			} else {
				return nil, statErr
			}
		} else {
			holder, _ := os.ReadFile(path)
			holderID := strings.TrimSpace(string(holder))
			timeStale := time.Since(info.ModTime()) > lockStaleAfter
			// PID 存活探测（同主机 cache 语义）：持有进程已死则立即
			// 接管，kill -9 后无需等满时间阈值（G4）；探测不确定时
			// 保守回退时间阈值（PID 复用只会推迟接管，不会误抢，K46）。
			if !timeStale && holderProcessAlive(holderID) {
				return nil, fmt.Errorf("%w（%s）", ErrLockHeld, holderID)
			}
			if err := takeoverLock(path, identity); err != nil {
				return nil, err
			}
		}
	}
	lock := &ProcessLock{
		path:     path,
		identity: identity,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go lock.heartbeat()
	return lock, nil
}

func tryCreateLock(path string, identity string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
	if err != nil {
		return err
	}
	_, writeErr := file.WriteString(identity + "\n")
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(path)
		return errors.Join(writeErr, syncErr, closeErr)
	}
	return nil
}

// holderProcessAlive 判定锁身份中的 PID 是否仍存活。信号 0 探测：
// 目标存活（或无权限，保守视为存活）返回 true；进程不存在返回 false；
// 身份不可解析时保守返回 true（只依赖时间阈值接管）。
func holderProcessAlive(identity string) bool {
	var pid int
	if _, err := fmt.Sscanf(identity, "pid=%d", &pid); err != nil || pid <= 0 {
		return true
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return false
		}
		// EPERM 等：进程存在但无权限，保守视为存活。
		return true
	}
	return true
}

// takeoverLock 原子抢占 stale 锁：temp+rename 后回读裁决（K45/K46）。
func takeoverLock(path string, identity string) error {
	temp := path + ".takeover-" + hex.EncodeToString([]byte(identity))[:8]
	tempFile := fmt.Sprintf("%s.%d", temp, os.Getpid())
	if err := os.WriteFile(tempFile, []byte(identity+"\n"), filePerm); err != nil {
		return err
	}
	if err := os.Rename(tempFile, path); err != nil {
		_ = os.Remove(tempFile)
		return err
	}
	// 回读裁决：并发接管时最后一次 rename 生效，未胜出者必须退出。
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(content)) != identity {
		return ErrLockHeld
	}
	return nil
}

// heartbeat 周期刷新 mtime 并复验身份；被接管即置 lost（K46）。
func (l *ProcessLock) heartbeat() {
	defer close(l.done)
	ticker := time.NewTicker(lockHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			if err := l.Verify(); err != nil {
				return
			}
			now := time.Now()
			_ = os.Chtimes(l.path, now, now)
		}
	}
}

// Verify 复验锁仍归自己持有；写路径关键点调用（K46）。
func (l *ProcessLock) Verify() error {
	if l.lost.Load() {
		return ErrLockLost
	}
	content, err := os.ReadFile(l.path)
	if err != nil || strings.TrimSpace(string(content)) != l.identity {
		l.lost.Store(true)
		return ErrLockLost
	}
	return nil
}

// Release 停止心跳并删除锁文件（仍持有时）；幂等。
func (l *ProcessLock) Release() {
	l.stopOnce.Do(func() {
		close(l.stop)
		<-l.done
		if l.Verify() == nil {
			_ = os.Remove(l.path)
		}
	})
}
