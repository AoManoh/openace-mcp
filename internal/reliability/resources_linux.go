//go:build linux

package reliability

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func readResources() resourceSnapshot {
	available, known, complete := readAvailableMemory(os.ReadFile)
	r := resourceSnapshot{availableBytes: available, memoryKnown: known, incomplete: !complete}
	var limit syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit) != nil {
		return r
	}
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return r
	}
	// 为查询、索引写入与其他进程内文件保留四分之一的描述符配额。
	maxInt := uint64(^uint64(0) >> 1)
	free := int64(min(limit.Cur-limit.Cur/4, maxInt)) - int64(len(fds))
	r.fdKnown, r.availableFD = true, max(int64(0), free)
	return r
}

// known 表示已有可执行的可用量上界；complete 表示可见限制已完整读取。
// 上界不完整时仍用于拒绝超大请求，但不能据此扩大并发窗口。
func readAvailableMemory(readFile func(string) ([]byte, error)) (available int64, known, complete bool) {
	data, err := readFile("/proc/meminfo")
	if err != nil {
		return 0, false, false
	}
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) == 3 && fields[0] == "MemAvailable:" && fields[2] == "kB" {
			n, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil || n < 0 || n > int64(^uint64(0)>>1)/1024 {
				return 0, false, false
			}
			available = n * 1024
			found = true
			break
		}
	}
	if !found {
		return 0, false, false
	}
	// 已确认的零余量不能被另一个不可读的资源文件变成“未知后放行”。
	if available == 0 {
		return 0, true, false
	}
	membership, err := readFile("/proc/self/cgroup")
	if err != nil {
		return available, true, false
	}
	mounts, err := readFile("/proc/self/mountinfo")
	if err != nil {
		return available, true, false
	}
	group, mount, mountRoot, v2, ok := memoryCgroup(string(membership), string(mounts))
	if !ok {
		return available, true, false
	}
	limitName, usageName := "memory.limit_in_bytes", "memory.usage_in_bytes"
	if v2 {
		limitName, usageName = "memory.max", "memory.current"
	}
	// cgroup 的祖先限制同样作用于当前进程。只读挂载根会漏掉 systemd
	// 服务或容器子组的限制；只读叶节点又会漏掉祖先被其他进程用尽的额度。
	for dir := group; ; dir = filepath.Dir(dir) {
		if _, err := readFile(filepath.Join(dir, "cgroup.procs")); err != nil {
			return available, true, false
		}
		limitData, err := readFile(filepath.Join(dir, limitName))
		if err != nil && !(v2 && os.IsNotExist(err)) {
			return available, true, false
		}
		// v2 根组与未启用 memory controller 的子组没有 memory.max。
		// cgroup.procs 已证明目录存在，仍须检查它的所有可见祖先。
		if err == nil && !(v2 && strings.TrimSpace(string(limitData)) == "max") {
			limit, err := strconv.ParseInt(strings.TrimSpace(string(limitData)), 10, 64)
			if err != nil || limit < 0 {
				return available, true, false
			}
			available = min(available, limit)
			usedData, err := readFile(filepath.Join(dir, usageName))
			if err != nil {
				return available, true, false
			}
			used, err := strconv.ParseInt(strings.TrimSpace(string(usedData)), 10, 64)
			if err != nil || used < 0 {
				return available, true, false
			}
			available = min(available, max(int64(0), limit-used))
			if available == 0 {
				return 0, true, false
			}
		}
		if dir == mount {
			break
		}
	}
	// 仅能访问子树 bind mount 时，上层约束不可见，不能宣称完整已知。
	return available, true, mountRoot == "/"
}

func memoryCgroup(membership, mountinfo string) (group, mount, rootPath string, v2, ok bool) {
	var unified, memory string
	for _, line := range strings.Split(membership, "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			continue
		}
		if fields[0] == "0" && fields[1] == "" {
			unified = fields[2]
		}
		if hasController(fields[1], "memory") {
			memory = fields[2]
		}
	}
	member := memory
	if member == "" {
		member, v2 = unified, true
	}
	if !filepath.IsAbs(member) || filepath.Clean(member) != member {
		return "", "", "", v2, false
	}
	bestRoot := ""
	for _, line := range strings.Split(mountinfo, "\n") {
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			continue
		}
		fields, fs := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(fields) < 6 || len(fs) < 3 {
			continue
		}
		if v2 && fs[0] != "cgroup2" || !v2 && (fs[0] != "cgroup" || !hasController(fs[2], "memory")) {
			continue
		}
		root, point := unescapeMountPath(fields[3]), unescapeMountPath(fields[4])
		if !filepath.IsAbs(root) || !filepath.IsAbs(point) {
			continue
		}
		rel, err := filepath.Rel(root, member)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		// 多个 bind mount 可见时选包含更多祖先的挂载，避免漏读更上层限额。
		if bestRoot != "" && len(root) >= len(bestRoot) {
			continue
		}
		bestRoot, mount, group = root, filepath.Clean(point), filepath.Join(point, rel)
	}
	return group, mount, bestRoot, v2, bestRoot != ""
}

func hasController(list, controller string) bool {
	for _, item := range strings.Split(list, ",") {
		if item == controller {
			return true
		}
	}
	return false
}

func unescapeMountPath(path string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(path)
}
