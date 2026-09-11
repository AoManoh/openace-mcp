package reliability

import (
	"sync"
	"time"
)

type resourceSnapshot struct {
	memoryKnown    bool
	fdKnown        bool
	incomplete     bool
	availableBytes int64
	availableFD    int64
}

func (r resourceSnapshot) known() bool { return r.memoryKnown && r.fdKnown && !r.incomplete }

// /proc 数据最多每 100ms 更新一次，避免每次请求重复枚举文件描述符。
// 已准入请求与逻辑批次还会在 Governor 内预留内存，缓存不代替预留检查。
func newResourceSampler() func() resourceSnapshot {
	var mu sync.Mutex
	var at time.Time
	var cached resourceSnapshot
	return func() resourceSnapshot {
		mu.Lock()
		defer mu.Unlock()
		if at.IsZero() || time.Since(at) >= 100*time.Millisecond {
			cached = readResources()
			at = time.Now()
		}
		return cached
	}
}
