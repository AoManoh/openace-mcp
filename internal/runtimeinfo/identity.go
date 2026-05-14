package runtimeinfo

import (
	"time"

	"github.com/AoManoh/openace-mcp/internal/buildinfo"
)

// ServedBy is compact runtime provenance for daemon-backed responses.
// It intentionally lives below daemon/workspace packages so transport structs
// can expose identity without depending on the daemon implementation.
type ServedBy struct {
	Service        string          `json:"service,omitempty"`
	PID            int             `json:"pid,omitempty"`
	StartedAt      time.Time       `json:"started_at,omitempty"`
	ListenAddr     string          `json:"listen_addr,omitempty"`
	Build          buildinfo.Info  `json:"build"`
	CacheDir       string          `json:"cache_dir,omitempty"`
	CacheNamespace string          `json:"cache_namespace,omitempty"`
	Capabilities   map[string]bool `json:"capabilities,omitempty"`
}
