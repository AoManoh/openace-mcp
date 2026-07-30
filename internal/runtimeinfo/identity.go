package runtimeinfo

import (
	"time"

	"github.com/AoManoh/openace-mcp/internal/buildinfo"
)

// ServedBy is compact runtime provenance for daemon-backed responses.
// It intentionally lives below daemon/workspace packages so transport structs
// can expose identity without depending on the daemon implementation.
type ServedBy struct {
	Service string `json:"service,omitempty"`
	Engine  string `json:"engine,omitempty"`
	// EngineProfile 是引擎配置指纹（provider 身份 + 降级开关的 hash，
	// 不含凭据）；daemon 复用要求与请求方一致（Stage 3 暗坑 K29）。
	EngineProfile  string          `json:"engine_profile,omitempty"`
	PID            int             `json:"pid,omitempty"`
	StartedAt      time.Time       `json:"started_at,omitempty"`
	ListenAddr     string          `json:"listen_addr,omitempty"`
	Build          buildinfo.Info  `json:"build"`
	CacheDir       string          `json:"cache_dir,omitempty"`
	CacheNamespace string          `json:"cache_namespace,omitempty"`
	Capabilities   map[string]bool `json:"capabilities,omitempty"`
}
