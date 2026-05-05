package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AoManoh/openace-mcp/internal/ace"
)

const defaultUploadBatchBytes = 1 << 20
const defaultFindMissingBatchSize = 1000
const defaultMaxFileBytes = 1 << 20

type ACEClient interface {
	FindMissing(context.Context, []string) ([]string, []string, error)
	BatchUpload(context.Context, []ace.BlobUpload) error
	CheckpointBlobs(context.Context, string, []string, []string) (string, error)
	CodebaseRetrieval(context.Context, string, ace.RetrievalOptions) (string, error)
}

type Syncer struct {
	client ACEClient
}

type Result struct {
	Text         string
	CheckpointID string
	FileCount    int
	Uploaded     int
	Added        int
	Deleted      int
}

type state struct {
	CheckpointID string            `json:"checkpoint_id,omitempty"`
	BlobNames    map[string]string `json:"blob_names,omitempty"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

type fileBlob struct {
	AbsPath  string
	RelPath  string
	BlobName string
}

func NewSyncer(client ACEClient) *Syncer {
	return &Syncer{client: client}
}

func (s *Syncer) Retrieve(ctx context.Context, dir string, query string, maxOutputLen int) (Result, error) {
	sync, err := s.Sync(ctx, dir)
	if err != nil {
		return Result{}, err
	}
	text, err := s.client.CodebaseRetrieval(ctx, query, ace.RetrievalOptions{
		CheckpointID: sync.CheckpointID,
		MaxOutputLen: maxOutputLen,
	})
	if err != nil {
		return Result{}, err
	}
	sync.Text = text
	return sync, nil
}

func (s *Syncer) Sync(ctx context.Context, dir string) (Result, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return Result{}, err
	}
	files, err := scan(ctx, root)
	if err != nil {
		return Result{}, err
	}
	st, statePath, err := loadState(root)
	if err != nil {
		return Result{}, err
	}
	if st.BlobNames == nil {
		st.BlobNames = map[string]string{}
	}

	current := make(map[string]string, len(files))
	byName := make(map[string]fileBlob, len(files))
	allNames := make([]string, 0, len(files))
	for _, file := range files {
		current[file.RelPath] = file.BlobName
		byName[file.BlobName] = file
		allNames = append(allNames, file.BlobName)
	}
	sort.Strings(allNames)

	added, deleted := diff(st.BlobNames, current)
	if st.CheckpointID == "" {
		added = allNames
		deleted = nil
	}

	unknown, nonindexed, err := findMissingBatched(ctx, s.client, allNames, findMissingBatchSize())
	if err != nil {
		return Result{}, err
	}
	toUpload := uniqueStrings(append(unknown, nonindexed...))
	uploads := make([]ace.BlobUpload, 0, len(toUpload))
	for _, name := range toUpload {
		file, ok := byName[name]
		if !ok {
			continue
		}
		content, ok, err := readIndexableContent(ctx, file.AbsPath, int64(maxFileBytes()))
		if err != nil {
			return Result{}, err
		}
		if !ok {
			return Result{}, fmt.Errorf("file is no longer indexable during sync: %s", file.RelPath)
		}
		if currentName := blobName(file.RelPath, content); currentName != file.BlobName {
			return Result{}, fmt.Errorf("file changed during sync: %s", file.RelPath)
		}
		uploads = append(uploads, ace.BlobUpload{
			BlobName: file.BlobName,
			Path:     file.RelPath,
			Content:  string(content),
		})
	}
	if len(uploads) > 0 {
		if err := batchUpload(ctx, s.client, uploads, uploadBatchBytes()); err != nil {
			return Result{}, err
		}
	}

	if len(added) > 0 || len(deleted) > 0 || st.CheckpointID == "" {
		checkpointID, err := s.client.CheckpointBlobs(ctx, st.CheckpointID, added, deleted)
		if err != nil {
			return Result{}, err
		}
		st.CheckpointID = checkpointID
	}
	st.BlobNames = current
	st.UpdatedAt = time.Now()
	if err := saveState(statePath, st); err != nil {
		return Result{}, err
	}

	return Result{
		CheckpointID: st.CheckpointID,
		FileCount:    len(files),
		Uploaded:     len(uploads),
		Added:        len(added),
		Deleted:      len(deleted),
	}, nil
}

func scan(ctx context.Context, root string) ([]fileBlob, error) {
	maxBytes := int64(maxFileBytes())
	rules := loadIgnoreRules(root)
	var files []fileBlob
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
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
			if path != root && (shouldSkipDir(name) || rules.Match(rel, true)) {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == "" {
			rel = name
		}
		if shouldSkipFile(rel, name) || rules.Match(rel, false) {
			return nil
		}
		if d.Type()&fs.ModeType != 0 {
			return nil
		}
		content, ok, err := readIndexableContent(ctx, path, maxBytes)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		files = append(files, fileBlob{
			AbsPath:  path,
			RelPath:  rel,
			BlobName: blobName(rel, content),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	return files, nil
}

func findMissingBatched(ctx context.Context, client ACEClient, blobNames []string, batchSize int) ([]string, []string, error) {
	if batchSize <= 0 {
		batchSize = defaultFindMissingBatchSize
	}
	var unknown []string
	var nonindexed []string
	for start := 0; start < len(blobNames); start += batchSize {
		end := start + batchSize
		if end > len(blobNames) {
			end = len(blobNames)
		}
		batchUnknown, batchNonindexed, err := client.FindMissing(ctx, blobNames[start:end])
		if err != nil {
			return nil, nil, err
		}
		unknown = append(unknown, batchUnknown...)
		nonindexed = append(nonindexed, batchNonindexed...)
	}
	return uniqueStrings(unknown), uniqueStrings(nonindexed), nil
}

func batchUpload(ctx context.Context, client ACEClient, uploads []ace.BlobUpload, maxBytes int) error {
	batches := uploadBatches(uploads, maxBytes)
	for i, batch := range batches {
		if err := client.BatchUpload(ctx, batch); err != nil {
			return fmt.Errorf("upload batch %d/%d files=%d bytes=%d first=%s last=%s: %w", i+1, len(batches), len(batch), uploadBatchSize(batch), firstUploadPath(batch), lastUploadPath(batch), err)
		}
	}
	return nil
}

func uploadBatches(uploads []ace.BlobUpload, maxBytes int) [][]ace.BlobUpload {
	if maxBytes <= 0 {
		maxBytes = defaultUploadBatchBytes
	}
	var batches [][]ace.BlobUpload
	var current []ace.BlobUpload
	currentBytes := 0
	for _, upload := range uploads {
		size := uploadPayloadSize(upload)
		if len(current) > 0 && currentBytes+size > maxBytes {
			batches = append(batches, current)
			current = nil
			currentBytes = 0
		}
		current = append(current, upload)
		currentBytes += size
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

func uploadBatchBytes() int {
	return positiveIntEnv("OPENACE_UPLOAD_BATCH_BYTES", defaultUploadBatchBytes)
}

func findMissingBatchSize() int {
	return positiveIntEnv("OPENACE_FIND_MISSING_BATCH_SIZE", defaultFindMissingBatchSize)
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

func uploadPayloadSize(upload ace.BlobUpload) int {
	payload := map[string]string{
		"blob_name": upload.BlobName,
		"path":      upload.Path,
		"content":   upload.Content,
	}
	data, err := json.Marshal(payload)
	if err == nil {
		return len(data) + 1
	}
	return len(upload.BlobName) + len(upload.Path) + len(upload.Content) + 128
}

func uploadBatchSize(uploads []ace.BlobUpload) int {
	total := len(`{"blobs":[]}`)
	for _, upload := range uploads {
		total += uploadPayloadSize(upload)
	}
	return total
}

func firstUploadPath(uploads []ace.BlobUpload) string {
	if len(uploads) == 0 {
		return ""
	}
	return uploads[0].Path
}

func lastUploadPath(uploads []ace.BlobUpload) string {
	if len(uploads) == 0 {
		return ""
	}
	return uploads[len(uploads)-1].Path
}

func shouldSkipDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".next", "dist", "build", "target", ".cache", ".venv", "venv", "__pycache__", ".pytest_cache", ".ruff_cache", ".mypy_cache", ".idea", ".vscode", "coverage", "tmp", ".turbo", ".parcel-cache", ".pnpm-store":
		return true
	default:
		return false
	}
}

func shouldSkipFile(rel string, name string) bool {
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
	case ".pem", ".key", ".p12", ".pfx", ".jks", ".kdb":
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
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
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
	negated  bool
	dirOnly  bool
	anchored bool
}

func loadIgnoreRules(root string) ignoreRules {
	var rules ignoreRules
	for _, name := range []string{".gitignore", ".ignore"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		rules = append(rules, parseIgnoreRules(string(data))...)
	}
	return rules
}

func parseIgnoreRules(data string) ignoreRules {
	var rules ignoreRules
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
			negated:  negated,
			dirOnly:  dirOnly,
			anchored: anchored,
		})
	}
	return rules
}

func (rules ignoreRules) Match(rel string, isDir bool) bool {
	rel = pathpkg.Clean(filepath.ToSlash(rel))
	if rel == "." || rel == "" {
		return false
	}
	ignored := false
	for _, rule := range rules {
		if rule.matches(rel, isDir) {
			ignored = !rule.negated
		}
	}
	return ignored
}

func (rule ignoreRule) matches(rel string, isDir bool) bool {
	pattern := rule.pattern
	if rule.dirOnly && !isDir && !hasPathPrefix(rel, pattern) {
		return false
	}
	if strings.Contains(pattern, "/") || rule.anchored {
		if rule.anchored {
			return matchPath(pattern, rel) || hasPathPrefix(rel, pattern)
		}
		for _, candidate := range suffixCandidates(rel) {
			if matchPath(pattern, candidate) || hasPathPrefix(candidate, pattern) {
				return true
			}
		}
		return false
	}
	for _, segment := range strings.Split(rel, "/") {
		if matchPath(pattern, segment) {
			return true
		}
	}
	return false
}

func suffixCandidates(rel string) []string {
	parts := strings.Split(rel, "/")
	candidates := make([]string, 0, len(parts))
	for i := range parts {
		candidates = append(candidates, strings.Join(parts[i:], "/"))
	}
	return candidates
}

func matchPath(pattern string, value string) bool {
	if ok, err := pathpkg.Match(pattern, value); err == nil && ok {
		return true
	}
	return pattern == value
}

func hasPathPrefix(rel string, prefix string) bool {
	return rel == prefix || strings.HasPrefix(rel, prefix+"/")
}

func looksBinary(data []byte) bool {
	limit := len(data)
	if limit > 8000 {
		limit = 8000
	}
	for i := 0; i < limit; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

func blobName(rel string, content []byte) string {
	h := sha256.New()
	h.Write([]byte(rel))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

func loadState(root string) (state, string, error) {
	path, err := stateFile(root)
	if err != nil {
		return state{}, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return state{}, path, nil
		}
		return state{}, "", err
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		backup := path + ".corrupt-" + time.Now().UTC().Format("20060102150405")
		_ = os.Rename(path, backup)
		return state{}, path, nil
	}
	return st, path, nil
}

func saveState(path string, st state) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func stateFile(root string) (string, error) {
	cache, err := cacheRoot()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(cache, "workspaces", cacheNamespace(), hex.EncodeToString(sum[:])+".json"), nil
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
		if strings.HasPrefix(dir, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			dir = filepath.Join(home, strings.TrimPrefix(dir, "~/"))
		}
		return filepath.Abs(dir)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "openace-mcp"), nil
}

func diff(old map[string]string, current map[string]string) ([]string, []string) {
	var added []string
	var deleted []string
	for path, name := range current {
		if oldName, ok := old[path]; !ok {
			added = append(added, name)
		} else if oldName != name {
			deleted = append(deleted, oldName)
			added = append(added, name)
		}
	}
	for path, oldName := range old {
		if _, ok := current[path]; !ok {
			deleted = append(deleted, oldName)
		}
	}
	return uniqueSorted(added), uniqueSorted(deleted)
}

func uniqueStrings(values []string) []string {
	return uniqueSorted(values)
}

func uniqueSorted(values []string) []string {
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		seen[value] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func (r Result) Summary() string {
	return fmt.Sprintf("checkpoint=%s files=%d uploaded=%d added=%d deleted=%d", r.CheckpointID, r.FileCount, r.Uploaded, r.Added, r.Deleted)
}
