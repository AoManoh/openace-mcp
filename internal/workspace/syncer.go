package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/pathutil"
)

const defaultMaxFileBytes = 1 << 20

type fileBlob struct {
	AbsPath  string
	RelPath  string
	BlobName string
}

func scan(ctx context.Context, root string) ([]fileBlob, error) {
	return scanWithCache(ctx, root, nil)
}

// scanWithCache 带 stat 短路缓存的扫描(T11):size+mtime 命中即复用
// blobName,跳过读内容与哈希;racy 窗口与删除清理见 StatCache。
// cache 为 nil 时行为与历史 scan 逐字节一致。
func scanWithCache(ctx context.Context, root string, cache *StatCache) ([]fileBlob, error) {
	files, _, err := scanWithCacheStats(ctx, root, cache)
	return files, err
}

// ScanStats 是扫描期的跳过统计(K6 口径:跳过必须如实计数,不静默吞);
// 经 FileAssetSource.LoadWithStats 上抛,localengine 状态面如实上报。
type ScanStats struct {
	// PermissionSkippedFiles 是因 fs.ErrPermission 被跳过的文件条目数。
	PermissionSkippedFiles int
}

// scanFileSkipDisposition 分类扫描期"单文件条目"的读取/stat 错误(M1):
//   - fs.ErrNotExist:ReadDir 与 stat/read 之间文件消失(TOCTOU——
//     构建产物、编辑器临时文件),跳过该文件,留待下次 sync 收敛;
//   - fs.ErrPermission:无读权限文件,跳过并计入 skipped 统计;
//   - 其余(IO 故障、ctx 取消等)保持致命。
//
// 根路径本身与目录级错误不经此函数分类,始终致命(根不可用意味着
// 结果集不可信,目录级错误意味着子树整体缺失,静默继续会产出
// 系统性残缺的索引)。
func scanFileSkipDisposition(err error) (skip bool, permission bool) {
	switch {
	case err == nil:
		return false, false
	case errors.Is(err, fs.ErrNotExist):
		return true, false
	case errors.Is(err, fs.ErrPermission):
		return true, true
	default:
		return false, false
	}
}

func scanWithCacheStats(ctx context.Context, root string, cache *StatCache) ([]fileBlob, ScanStats, error) {
	var stats ScanStats
	root, err := resolveScanRoot(root)
	if err != nil {
		return nil, stats, err
	}
	maxBytes := int64(maxFileBytes())
	// F6 扫描器债修复:ignore 规则按目录栈管理,而非在整个 walk 期单调
	// 累积——旧实现里已完成兄弟子树的规则(经 base 检查必然空转)仍被
	// 每个后续文件逐条求值,大仓深处一次 Match 扫上千条规则(sealed
	// openace 活树 1,088 规则的 SIGQUIT 栈证据)。栈语义与累积语义逐位
	// 等价:base 约束保证非祖先目录的规则永不匹配当前路径,弹出它们
	// 不改变任何判定;规则求值序(外层先、同层后到者胜)保持不变。
	stack := ruleStack{{dirRel: "", rules: loadIgnoreRules(root)}}
	var files []fileBlob
	scanStart := time.Now()
	seen := make(map[string]bool)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			// 此处的 err 只来自根的 Lstat 与目录的 ReadDir(WalkDir 契约,
			// 文件条目的 stat 是惰性的),按 M1 裁决保持致命。
			return err
		}
		name := d.Name()
		rel := ""
		if path != root {
			var relErr error
			rel, relErr = filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			rel = filepath.ToSlash(rel)
		}
		if d.IsDir() {
			if path != root {
				return enterScanDir(&stack, path, rel, name)
			}
			return nil
		}
		if rel == "" {
			rel = name
		}
		stack.unwindTo(rel)
		if shouldAlwaysSkipFile(rel, name) || stack.Match(rel, false) {
			return nil
		}
		if d.Type()&fs.ModeType != 0 {
			return nil
		}
		if cache != nil {
			if info, infoErr := d.Info(); infoErr == nil {
				if hit, ok := cache.lookup(rel, info.Size(), info.ModTime(), scanStart); ok {
					seen[rel] = true
					files = append(files, fileBlob{AbsPath: path, RelPath: rel, BlobName: hit})
					return nil
				}
				content, ok, err := readScanBlob(ctx, path, maxBytes, &stats)
				if err != nil || !ok {
					return err
				}
				name := blobName(rel, content)
				cache.store(rel, info.Size(), info.ModTime(), name)
				seen[rel] = true
				files = append(files, fileBlob{AbsPath: path, RelPath: rel, BlobName: name})
				return nil
			}
		}
		content, ok, err := readScanBlob(ctx, path, maxBytes, &stats)
		if err != nil || !ok {
			return err
		}
		files = append(files, fileBlob{
			AbsPath:  path,
			RelPath:  rel,
			BlobName: blobName(rel, content),
		})
		return nil
	})
	if cache != nil && err == nil {
		cache.prune(seen)
	}
	if err != nil {
		return nil, stats, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	return files, stats, nil
}

// resolveScanRoot 校验并规范化扫描根。H2 第二层防御(第一层是
// pathutil.ResolveWorkspaceRoot 的 EvalSymlinks;此处兜住旧 state 文件里
// 的 canonical_path、直接调用 scan 的路径):根必须解析为一个存在的目录,
// 否则显式报错——与"根不存在时 WalkDir 报错"的行为对齐,禁止把根当
// 单文件收录或静默返回空集。根的叶组件为 symlink→目录时,WalkDir 对根
// 用 Lstat 会把它当"非常规文件"跳过并成功返回空集(H2 机理);解析到
// 真实目录再走,中间组件的 symlink 由内核路径解析处理,不受影响。
func resolveScanRoot(root string) (string, error) {
	rootInfo, err := os.Stat(root)
	if err != nil {
		// P7:根不存在/不可达是调用方传错目录的典型形态,打请求类
		// 标记(daemon 面映射 400 而非 502)。
		return "", engine.AsInvalidRequest(fmt.Errorf("workspace root is not accessible: %w", err))
	}
	if !rootInfo.IsDir() {
		return "", engine.AsInvalidRequest(fmt.Errorf("workspace root is not a directory: %s", root))
	}
	if lst, lerr := os.Lstat(root); lerr == nil && lst.Mode()&fs.ModeSymlink != 0 {
		resolved, rerr := filepath.EvalSymlinks(root)
		if rerr != nil {
			return "", fmt.Errorf("resolve workspace root symlink: %w", rerr)
		}
		root = resolved
	}
	return root, nil
}

// enterScanDir 处理 walk 的目录条目:维护 F6 规则栈并做跳过判定,
// 返回 filepath.SkipDir 或 nil(判定语义与规则求值序不变)。
func enterScanDir(stack *ruleStack, path string, rel string, name string) error {
	if shouldAlwaysSkipDir(name) {
		return filepath.SkipDir
	}
	stack.unwindTo(rel)
	localRules := loadIgnoreRulesForDir(path, rel)
	stack.push(rel, localRules)
	if stack.Match(rel, true) && !localRules.hasAugmentInclude() && !stack.hasAugmentDescendantInclude(rel) {
		return filepath.SkipDir
	}
	return nil
}

// readScanBlob 读取单文件可索引内容并归置 M1 跳过语义:ok=false 表示
// 该文件跳过(不可索引内容,或 TOCTOU/权限错误——权限跳过计入 stats),
// err 仅在致命时非 nil(中止整次扫描)。
func readScanBlob(ctx context.Context, path string, maxBytes int64, stats *ScanStats) ([]byte, bool, error) {
	content, ok, err := readIndexableContent(ctx, path, maxBytes)
	if err != nil {
		// M1:单文件 TOCTOU/权限错误只跳过该文件,不中止整次扫描。
		if skip, denied := scanFileSkipDisposition(err); skip {
			if denied {
				stats.PermissionSkippedFiles++
			}
			return nil, false, nil
		}
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	return content, true, nil
}

func maxFileBytes() int {
	return positiveIntEnv("OPENACE_MAX_FILE_BYTES", defaultMaxFileBytes)
}

func positiveIntEnv(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func shouldAlwaysSkipDir(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", ".jj":
		return true
	default:
		return false
	}
}

func shouldAlwaysSkipFile(rel string, name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	rel = strings.ToLower(filepath.ToSlash(rel))
	if strings.HasPrefix(name, ".env") {
		return true
	}
	switch name {
	case ".npmrc", ".pypirc", ".netrc", ".dockercfg", "session.json", "credentials", "credentials.json", "service-account.json", "token", "tokens.json", "secret.json", "secrets.json", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519":
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".pem", ".key", ".p12", ".pfx", ".jks", ".kdb", ".crt", ".cer", ".der", ".csr", ".p7b", ".p7c":
		return true
	}
	if (strings.HasPrefix(rel, ".augment/") || strings.Contains(rel, "/.augment/")) && name == "session.json" {
		return true
	}
	return false
}

func readIndexableContent(ctx context.Context, path string, maxBytes int64) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxBytes {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	// L2:stat 与读取之间存在 TOCTOU 窗口——文件可能已被替换为超限
	// 大文件,os.ReadFile 会无界整读入内存。读取按 maxBytes+1 设上界,
	// 读满上界即证明实际内容超限,按既有 oversize 跳过口径处理
	// (与上方 stat 期 size > maxBytes 的处置一致:ok=false, err=nil)。
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(content)) > maxBytes {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if looksBinary(content) || !utf8.Valid(content) {
		return nil, false, nil
	}
	return content, true, nil
}

type ignoreRules []ignoreRule

type ignoreRule struct {
	pattern  string
	base     string
	layer    ignoreLayer
	negated  bool
	dirOnly  bool
	anchored bool
}

type ignoreLayer int

const (
	ignoreLayerDefault ignoreLayer = iota
	ignoreLayerGit
	ignoreLayerAugment
)

const defaultIgnoreRuleData = `
node_modules/
.next/
dist/
build/
target/
.cache/
.venv/
venv/
__pycache__/
.pytest_cache/
.ruff_cache/
.mypy_cache/
.idea/
.vscode/
coverage/
tmp/
.turbo/
.parcel-cache/
.pnpm-store/
`

func loadIgnoreRules(root string) ignoreRules {
	rules := parseIgnoreRulesWithBase(defaultIgnoreRuleData, "", ignoreLayerDefault)
	return append(rules, loadIgnoreRulesForDir(root, "")...)
}

func loadIgnoreRulesForDir(dir string, base string) ignoreRules {
	var rules ignoreRules
	for _, spec := range []struct {
		name  string
		layer ignoreLayer
	}{
		{name: ".gitignore", layer: ignoreLayerGit},
		{name: ".ignore", layer: ignoreLayerGit},
		// .openaceignore 是 canonical(方案⑤,2026-08-02,B 语义):
		// 同目录存在时 .augmentignore 规则整体被遮蔽(逐目录粒度,
		// 迁移=改名可渐进);仅 alias 时兼容语义与历史一致。层级同
		// augment 层(可 ! re-include gitignored,hard deny 不可覆盖)。
		{name: ".openaceignore", layer: ignoreLayerAugment},
		{name: ".augmentignore", layer: ignoreLayerAugment},
	} {
		if spec.name == ".augmentignore" {
			if _, err := os.Stat(filepath.Join(dir, ".openaceignore")); err == nil {
				continue
			}
		}
		data, err := os.ReadFile(filepath.Join(dir, spec.name))
		if err != nil {
			continue
		}
		rules = append(rules, parseIgnoreRulesWithBase(string(data), base, spec.layer)...)
	}
	return rules
}

func parseIgnoreRulesWithBase(data string, base string, layer ignoreLayer) ignoreRules {
	var rules ignoreRules
	base = filepath.ToSlash(filepath.Clean(base))
	if base == "." {
		base = ""
	}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negated := strings.HasPrefix(line, "!")
		if negated {
			line = strings.TrimSpace(strings.TrimPrefix(line, "!"))
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		anchored := strings.HasPrefix(line, "/")
		line = strings.TrimPrefix(line, "/")
		dirOnly := strings.HasSuffix(line, "/")
		line = strings.TrimSuffix(line, "/")
		line = filepath.ToSlash(filepath.Clean(line))
		line = strings.TrimPrefix(line, "./")
		if line == "" || line == "." {
			continue
		}
		rules = append(rules, ignoreRule{
			pattern:  line,
			base:     base,
			layer:    layer,
			negated:  negated,
			dirOnly:  dirOnly,
			anchored: anchored,
		})
	}
	return rules
}

// ruleFrame 是目录栈上的一层规则(dirRel="" 为根帧:内置默认 + 根级
// ignore 文件,永不弹出)。
type ruleFrame struct {
	dirRel string
	rules  ignoreRules
}

// ruleStack 按祖先链维护活动规则(F6 修复):Match/hasAugmentDescendantInclude
// 只遍历当前路径祖先目录贡献的规则,与历史"全量累积"判定逐位等价
// (非祖先规则受 base 约束必然不匹配),复杂度从 O(仓内全部规则) 降到
// O(祖先链规则)。
type ruleStack []ruleFrame

// push 压入新目录帧(空规则也压,保证 unwind 语义均匀)。
func (s *ruleStack) push(dirRel string, rules ignoreRules) {
	*s = append(*s, ruleFrame{dirRel: dirRel, rules: rules})
}

// unwindTo 弹出所有不是 rel 祖先的帧(WalkDir 为 DFS 字典序,离开子树
// 后首个非子树条目触发弹出)。根帧永驻。
func (s *ruleStack) unwindTo(rel string) {
	for len(*s) > 1 {
		top := (*s)[len(*s)-1]
		if rel == top.dirRel || strings.HasPrefix(rel, top.dirRel+"/") {
			return
		}
		*s = (*s)[:len(*s)-1]
	}
}

// Match 语义与 ignoreRules.Match 一致:先非 augment 层后 augment 层,
// 同层内后加载(更深目录)者胜;帧序即加载序。
func (s ruleStack) Match(rel string, isDir bool) bool {
	rel = pathpkg.Clean(filepath.ToSlash(rel))
	if rel == "." || rel == "" {
		return false
	}
	ignored := false
	for _, frame := range s {
		for _, rule := range frame.rules {
			if rule.layer != ignoreLayerAugment && rule.matches(rel, isDir) {
				ignored = !rule.negated
			}
		}
	}
	for _, frame := range s {
		for _, rule := range frame.rules {
			if rule.layer == ignoreLayerAugment && rule.matches(rel, isDir) {
				ignored = !rule.negated
			}
		}
	}
	return ignored
}

func (s ruleStack) hasAugmentDescendantInclude(rel string) bool {
	for _, frame := range s {
		if frame.rules.hasAugmentDescendantInclude(rel) {
			return true
		}
	}
	return false
}

func (rules ignoreRules) Match(rel string, isDir bool) bool {
	rel = pathpkg.Clean(filepath.ToSlash(rel))
	if rel == "." || rel == "" {
		return false
	}
	ignored := false
	for _, rule := range rules {
		if rule.layer != ignoreLayerAugment && rule.matches(rel, isDir) {
			ignored = !rule.negated
		}
	}
	for _, rule := range rules {
		if rule.layer == ignoreLayerAugment && rule.matches(rel, isDir) {
			ignored = !rule.negated
		}
	}
	return ignored
}

func (rules ignoreRules) hasAugmentInclude() bool {
	for _, rule := range rules {
		if rule.layer == ignoreLayerAugment && rule.negated {
			return true
		}
	}
	return false
}

func (rules ignoreRules) hasAugmentDescendantInclude(rel string) bool {
	rel = pathpkg.Clean(filepath.ToSlash(rel))
	if rel == "." || rel == "" {
		return false
	}
	for _, rule := range rules {
		if rule.layer == ignoreLayerAugment && rule.negated && rule.canMatchInside(rel) {
			return true
		}
	}
	return false
}

// matches 判定单条规则是否命中 rel。dirOnly 语义(诊断 L3):尾随 /
// 的目录规则(如 build/)只匹配目录本身,同名普通文件不受影响——此前
// 该约束只在否定分支生效,非否定的目录规则会误伤同名文件。目录被
// 忽略后其内容仍按包含语义命中(路径模式经祖先命中/前缀包含,裸段
// 模式经非最终段命中),不受 dirOnly 约束影响。
func (rule ignoreRule) matches(rel string, isDir bool) bool {
	rel, ok := rule.relForBase(rel)
	if !ok || rel == "" {
		return false
	}
	if rule.dirOnly && rule.negated && !isDir {
		return false
	}
	pattern := rule.pattern
	if strings.Contains(pattern, "/") || rule.anchored {
		if matchPath(pattern, rel) {
			if !rule.dirOnly || isDir {
				return true
			}
			// rel 是普通文件且规则只匹配目录:文件本身不命中;但其
			// 某个祖先目录命中该模式时(a/** 类通配才可能),按"目录
			// 被忽略则内容整体被忽略"的包含语义仍命中。此分支仅非
			// 否定规则可达(否定 + dirOnly + 文件已在上方提前返回)。
			for i := len(rel) - 1; i > 0; i-- {
				if rel[i] == '/' && matchPath(pattern, rel[:i]) {
					return true
				}
			}
			return false
		}
		return !rule.negated && hasPathPrefix(rel, pattern)
	}
	if rule.negated {
		return matchPath(pattern, pathpkg.Base(rel))
	}
	segments := strings.Split(rel, "/")
	for i, segment := range segments {
		if !matchPath(pattern, segment) {
			continue
		}
		// dirOnly 规则命中最终段且该条目为普通文件时不命中;非最终段
		// 命中意味着 rel 位于同名目录之内,属包含语义,始终命中。
		if rule.dirOnly && !isDir && i == len(segments)-1 {
			continue
		}
		return true
	}
	return false
}

func (rule ignoreRule) canMatchInside(dir string) bool {
	if rule.base != "" {
		if dir == rule.base {
			return true
		}
		if strings.HasPrefix(dir, rule.base+"/") {
			inside := strings.TrimPrefix(dir, rule.base+"/")
			return patternCanMatchInside(rule.pattern, inside)
		}
		return strings.HasPrefix(rule.base, dir+"/")
	}
	return patternCanMatchInside(rule.pattern, dir)
}

func patternCanMatchInside(pattern string, dir string) bool {
	if dir == "" {
		return true
	}
	if !strings.Contains(pattern, "/") {
		return false
	}
	if matchPath(pattern, dir) || strings.HasPrefix(pattern, dir+"/") {
		return true
	}
	if !hasDoubleStarSegment(pattern) {
		return false
	}
	prefix := prefixBeforeDoubleStar(pattern)
	return prefix == "" || dir == prefix || strings.HasPrefix(dir, prefix+"/") || strings.HasPrefix(prefix, dir+"/")
}

func prefixBeforeDoubleStar(pattern string) string {
	segments := strings.Split(pattern, "/")
	var prefix []string
	for _, segment := range segments {
		if segment == "**" {
			break
		}
		prefix = append(prefix, segment)
	}
	return strings.Join(prefix, "/")
}

func (rule ignoreRule) relForBase(rel string) (string, bool) {
	if rule.base == "" {
		return rel, true
	}
	if rel == rule.base {
		return "", false
	}
	prefix := rule.base + "/"
	if !strings.HasPrefix(rel, prefix) {
		return "", false
	}
	return strings.TrimPrefix(rel, prefix), true
}

func matchPath(pattern string, value string) bool {
	if hasDoubleStarSegment(pattern) && matchPathSegments(strings.Split(pattern, "/"), strings.Split(value, "/")) {
		return true
	}
	if ok, err := pathpkg.Match(pattern, value); err == nil && ok {
		return true
	}
	return pattern == value
}

func hasDoubleStarSegment(pattern string) bool {
	for _, segment := range strings.Split(pattern, "/") {
		if segment == "**" {
			return true
		}
	}
	return false
}

func matchPathSegments(patterns []string, values []string) bool {
	if len(patterns) == 0 {
		return len(values) == 0
	}
	if patterns[0] == "**" {
		if matchPathSegments(patterns[1:], values) {
			return true
		}
		for i := range values {
			if matchPathSegments(patterns[1:], values[i+1:]) {
				return true
			}
		}
		return false
	}
	if len(values) == 0 {
		return false
	}
	if ok, err := pathpkg.Match(patterns[0], values[0]); (err == nil && ok) || patterns[0] == values[0] {
		return matchPathSegments(patterns[1:], values[1:])
	}
	return false
}

func hasPathPrefix(rel string, prefix string) bool {
	return rel == prefix || strings.HasPrefix(rel, prefix+"/")
}

// looksBinary 全量探测 NUL(诊断 L4):内容此刻已整读入内存(上限
// maxFileBytes),全量扫描无额外 IO 成本;NUL 是合法 UTF-8(U+0000),
// 只查前 8000 字节会让 NUL 靠后的伪文本绕过 utf8.Valid 进入索引。
func looksBinary(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0
}

// blobName 是文件身份哈希。摘要输入采用显式无歧义编码(诊断 L1):
//
//	sha256( decimal(len(rel)) "\x00" rel "\x00" content )
//
// 十进制长度前缀 + NUL 分隔使 (rel, content) → 输入字节串为单射:
// 长度段只含数字、以首个 NUL 终止,rel 按长度精确取回,余下为
// content——裸拼接 sha256(rel+content) 下 ("a","bc") 与 ("ab","c")
// 的歧义碰撞不再可构造。
//
// 本编码变更使全部 blobName 变化,属破坏性变更,随 chunk profile v4 /
// A2' 模板升级的同一未发布 BREAKING 窗口落地:全部消费方(statcache、
// legacy state.BlobNames、manifest FileEntry.ContentHash、ACE 上传)
// 均把 blobName 当不透明等值比较,无格式假设;升级后首次 sync 表现为
// 一次性全量变更(重上传/重建),chunk 级向量按 embedKey 复用不受影响。
func blobName(rel string, content []byte) string {
	h := sha256.New()
	io.WriteString(h, strconv.Itoa(len(rel)))
	h.Write([]byte{0})
	io.WriteString(h, rel)
	h.Write([]byte{0})
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

func cacheNamespace() string {
	namespace := strings.TrimSpace(os.Getenv("OPENACE_CACHE_NAMESPACE"))
	if namespace == "" {
		return "default"
	}
	namespace = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, namespace)
	namespace = strings.Trim(namespace, ".-")
	if namespace == "" {
		return "default"
	}
	return namespace
}

func cacheRoot() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("OPENACE_CACHE_DIR")); dir != "" {
		expanded, err := pathutil.ExpandUser(dir)
		if err != nil {
			return "", err
		}
		return filepath.Abs(expanded)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "openace-mcp"), nil
}

type CacheSnapshot struct {
	Dir       string `json:"cache_dir"`
	Namespace string `json:"cache_namespace"`
}

func CurrentCacheSnapshot() (CacheSnapshot, error) {
	dir, err := cacheRoot()
	if err != nil {
		return CacheSnapshot{}, err
	}
	return CacheSnapshot{
		Dir:       dir,
		Namespace: cacheNamespace(),
	}, nil
}
