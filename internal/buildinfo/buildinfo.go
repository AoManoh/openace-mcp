package buildinfo

import (
	"runtime"
	"runtime/debug"
)

// Info captures the runtime/build identity that operators need when a wrapper
// and daemon may be different binaries.
type Info struct {
	Path        string `json:"path,omitempty"`
	Version     string `json:"version,omitempty"`
	VCSRevision string `json:"vcs_revision,omitempty"`
	VCSTime     string `json:"vcs_time,omitempty"`
	VCSModified string `json:"vcs_modified,omitempty"`
	GOOS        string `json:"goos"`
	GOARCH      string `json:"goarch"`
}

func Current() Info {
	info := Info{
		GOOS:   runtime.GOOS,
		GOARCH: runtime.GOARCH,
	}
	build, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	info.Path = build.Path
	info.Version = build.Main.Version
	for _, setting := range build.Settings {
		switch setting.Key {
		case "vcs.revision":
			info.VCSRevision = setting.Value
		case "vcs.time":
			info.VCSTime = setting.Value
		case "vcs.modified":
			info.VCSModified = setting.Value
		}
	}
	return info
}
