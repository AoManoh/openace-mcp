package index

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// 重型构建名额：按用户、跨进程共享。估算峰值内存很大的构建（例如 30 多万个 chunk 的
// 仓库全量重建，约 1.8 GiB）在多个 openACE 进程里同时跑时内存会叠加，生产 daemon、
// 预建 daemon 与另起的 daemon 之间原本互不感知。名额是系统临时目录下的一组锁文件
// slot-<n>.lock，复用写锁的 O_EXCL 创建、心跳与接管规则：持有者进程已死立即接管，
// 卡住超过 60 秒没有心跳按超时接管。目录不随 XDG_CACHE_HOME、OPENACE_CACHE_DIR 变化，
// 不同缓存目录的进程也互相可见。

// ErrHeavySlotsBusy 表示全部名额都被存活进程持有。
var ErrHeavySlotsBusy = errors.New("重型构建名额已满")

// heavySlotPoll 是等待名额时的轮询间隔；var 仅为测试可缩短。
var heavySlotPoll = 500 * time.Millisecond

// HeavyBuildSlots 是一组跨进程共享的重型构建名额。
type HeavyBuildSlots struct {
	dir   string
	count int
}

// HeavyBuildHolder 是一个被占名额的持有者：名额序号、进程号与持有者写的说明。
type HeavyBuildHolder struct {
	Slot int
	PID  int
	Note string
}

// DefaultHeavyBuildDir 返回默认的名额目录 os.TempDir()/openace-heavy-build-<uid>；
// 取不到用户号的平台（Windows 的临时目录本来就按用户区分）不带后缀。
func DefaultHeavyBuildDir() string {
	name := "openace-heavy-build"
	if uid := os.Getuid(); uid >= 0 {
		name += "-" + strconv.Itoa(uid)
	}
	return filepath.Join(os.TempDir(), name)
}

// NewHeavyBuildSlots 返回 dir 下 count 个名额（小于 1 按 1）。目录在第一次获取名额
// 时建立并检查。
func NewHeavyBuildSlots(dir string, count int) *HeavyBuildSlots {
	return &HeavyBuildSlots{dir: dir, count: max(count, 1)}
}

// Dir 返回名额目录。
func (h *HeavyBuildSlots) Dir() string { return h.dir }

// Count 返回名额数。
func (h *HeavyBuildSlots) Count() int { return h.count }

func (h *HeavyBuildSlots) slotPath(slot int) string {
	return filepath.Join(h.dir, fmt.Sprintf("slot-%d.lock", slot))
}

// TryAcquire 依次尝试每个名额，拿到一个就返回释放函数；全部被存活进程持有时返回
// ErrHeavySlotsBusy；名额目录不可用时返回该错误。
func (h *HeavyBuildSlots) TryAcquire(note string) (func(), error) {
	if err := ensurePrivateDir(h.dir); err != nil {
		return nil, err
	}
	for slot := 0; slot < h.count; slot++ {
		lock, err := AcquireLockAt(h.slotPath(slot), note)
		if err == nil {
			return lock.Release, nil
		}
		if !errors.Is(err, ErrLockHeld) {
			return nil, err
		}
	}
	return nil, ErrHeavySlotsBusy
}

// Acquire 等到拿到一个名额，每 heavySlotPoll 尝试一次；ctx 取消时返回 ctx 的错误，
// 不占名额。
func (h *HeavyBuildSlots) Acquire(ctx context.Context, note string) (func(), error) {
	for {
		release, err := h.TryAcquire(note)
		if !errors.Is(err, ErrHeavySlotsBusy) {
			return release, err
		}
		timer := time.NewTimer(heavySlotPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Holders 按名额序列出当前被占的名额；读不出的锁文件跳过。
func (h *HeavyBuildSlots) Holders() []HeavyBuildHolder {
	var holders []HeavyBuildHolder
	for slot := 0; slot < h.count; slot++ {
		holder, err := ReadLockHolder(h.slotPath(slot))
		if err != nil {
			continue
		}
		holders = append(holders, HeavyBuildHolder{Slot: slot, PID: holder.PID, Note: holder.Note})
	}
	return holders
}

// ensurePrivateDir 建立名额目录（0700）并检查：必须是目录本身而不是符号链接，Unix 上
// 属于当前用户；权限过宽时收回到 0700。系统临时目录对所有用户可写，别人预先建好的
// 同名目录可以放入假的锁文件，让本用户的重型构建一直排队。
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("重型构建名额目录 %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("重型构建名额目录 %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("重型构建名额目录 %s 不是目录（可能是符号链接），不使用", dir)
	}
	if err := checkDirOwner(dir, info); err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("重型构建名额目录 %s 权限 %v 过宽，收回到 0700 失败: %w", dir, info.Mode().Perm(), err)
		}
	}
	return nil
}
