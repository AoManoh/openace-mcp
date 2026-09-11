//go:build linux

package reliability

import (
	"os"
	"testing"
)

func memoryFixture(overrides map[string]string) func(string) ([]byte, error) {
	files := map[string]string{
		"/proc/meminfo":                            "MemAvailable: 8192 kB\n",
		"/proc/self/cgroup":                        "0::/parent/child\n",
		"/proc/self/mountinfo":                     "20 1 0:20 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n",
		"/sys/fs/cgroup/cgroup.procs":              "",
		"/sys/fs/cgroup/parent/cgroup.procs":       "",
		"/sys/fs/cgroup/parent/child/cgroup.procs": "1\n",
		"/sys/fs/cgroup/parent/memory.max":         "max\n",
		"/sys/fs/cgroup/parent/child/memory.max":   "max\n",
	}
	for path, content := range overrides {
		files[path] = content
	}
	return func(path string) ([]byte, error) {
		if content, ok := files[path]; ok {
			return []byte(content), nil
		}
		return nil, os.ErrNotExist
	}
}

func TestAvailableMemoryIncludesCgroupAncestors(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  int64
	}{
		{"unlimited", nil, 8192 * 1024},
		{"child", map[string]string{
			"/sys/fs/cgroup/parent/child/memory.max":     "1048576",
			"/sys/fs/cgroup/parent/child/memory.current": "262144",
		}, 786432},
		{"parent_tighter", map[string]string{
			"/sys/fs/cgroup/parent/child/memory.max":     "1048576",
			"/sys/fs/cgroup/parent/child/memory.current": "262144",
			"/sys/fs/cgroup/parent/memory.max":           "2097152",
			"/sys/fs/cgroup/parent/memory.current":       "1966080",
		}, 131072},
		{"over_limit", map[string]string{
			"/sys/fs/cgroup/parent/child/memory.max":     "100",
			"/sys/fs/cgroup/parent/child/memory.current": "101",
		}, 0},
		{"host_exhausted", map[string]string{"/proc/meminfo": "MemAvailable: 0 kB\n"}, 0},
		{"subtree_mount", map[string]string{
			"/proc/self/mountinfo":                "20 1 0:20 /parent /sys/fs/cgroup rw - cgroup2 cgroup rw\n",
			"/sys/fs/cgroup/child/cgroup.procs":   "1",
			"/sys/fs/cgroup/child/memory.max":     "4096",
			"/sys/fs/cgroup/child/memory.current": "1024",
		}, 3072},
		{"v1_memory", map[string]string{
			"/proc/self/cgroup":                                 "5:cpu:/parent/child\n6:memory:/parent/child\n",
			"/proc/self/mountinfo":                              "20 1 0:20 /parent /sys/fs/cgroup/memory rw - cgroup cgroup rw,memory\n",
			"/sys/fs/cgroup/memory/cgroup.procs":                "",
			"/sys/fs/cgroup/memory/child/cgroup.procs":          "1",
			"/sys/fs/cgroup/memory/memory.limit_in_bytes":       "8192",
			"/sys/fs/cgroup/memory/memory.usage_in_bytes":       "7168",
			"/sys/fs/cgroup/memory/child/memory.limit_in_bytes": "4096",
			"/sys/fs/cgroup/memory/child/memory.usage_in_bytes": "1024",
		}, 1024},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, known, complete := readAvailableMemory(memoryFixture(tc.files))
			if !known || got != tc.want {
				t.Fatalf("memory=%d known=%v; want=%d", got, known, tc.want)
			}
			wantComplete := tc.name == "unlimited" || tc.name == "child" || tc.name == "parent_tighter"
			if complete != wantComplete {
				t.Fatalf("完整资源判定=%v want=%v", complete, wantComplete)
			}
		})
	}
}

func TestAvailableMemoryDoesNotAssumeMissingLimits(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"missing_group":         {"/proc/self/cgroup": "0::/missing\n"},
		"missing_usage":         {"/sys/fs/cgroup/parent/child/memory.max": "4096"},
		"negative_usage":        {"/sys/fs/cgroup/parent/child/memory.max": "4096", "/sys/fs/cgroup/parent/child/memory.current": "-1"},
		"invalid_limit":         {"/sys/fs/cgroup/parent/child/memory.max": "garbage"},
		"missing_membership":    {"/proc/self/cgroup": ""},
		"missing_mount":         {"/proc/self/mountinfo": ""},
		"missing_memavailable":  {"/proc/meminfo": "MemFree: 4096 kB\n"},
		"memavailable_overflow": {"/proc/meminfo": "MemAvailable: 9223372036854775807 kB\n"},
	} {
		t.Run(name, func(t *testing.T) {
			if memory, _, complete := readAvailableMemory(memoryFixture(files)); complete {
				t.Fatalf("不完整资源证据被标为已知: %d", memory)
			}
		})
	}
}

func TestAvailableMemoryRetainsKnownBounds(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"host_known": {"/proc/self/cgroup": ""},
		"child_known": {
			"/sys/fs/cgroup/parent/child/memory.max":     "4096",
			"/sys/fs/cgroup/parent/child/memory.current": "3072",
			"/sys/fs/cgroup/parent/memory.max":           "invalid",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, known, complete := readAvailableMemory(memoryFixture(files))
			want := int64(8192 * 1024)
			if name == "child_known" {
				want = 1024
			}
			if !known || got != want {
				t.Fatalf("已知内存上界丢失: bytes=%d known=%t want=%d", got, known, want)
			}
			if complete {
				t.Fatal("缺失的限制被当作完整资源证据")
			}
		})
	}
}
