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
	"sync"
	"time"
	"unicode/utf8"

	"github.com/AoManoh/openace-mcp/internal/ace"
	"github.com/AoManoh/openace-mcp/internal/pathutil"
)

const defaultUploadBatchBytes = 1 << 20
const defaultFindMissingBatchSize = 1000
const defaultMaxFileBytes = 1 << 20
const defaultRetrievalTimeout = 90 * time.Second

type IndexStage string

const (
	IndexStageIdle          IndexStage = "idle"
	IndexStageScanning      IndexStage = "scanning"
	IndexStageReconciling   IndexStage = "reconciling"
	IndexStageUploading     IndexStage = "uploading"
	IndexStageCheckpointing IndexStage = "checkpointing"
	IndexStageReady         IndexStage = "ready"
	IndexStageFailed        IndexStage = "failed"
)

type SyncReason string

const (
	SyncReasonManual     SyncReason = "manual"
	SyncReasonRetrieval  SyncReason = "retrieval"
	SyncReasonBackground SyncReason = "background"
)

type ACEClient interface {
	FindMissing(context.Context, []string) ([]string, []string, error)
	BatchUpload(context.Context, []ace.BlobUpload) error
	CheckpointBlobs(context.Context, string, []string, []string) (string, error)
	CodebaseRetrieval(context.Context, string, ace.RetrievalOptions) (string, error)
}

type Syncer struct {
	client   ACEClient
	mu       sync.Mutex
	inflight map[string]*syncCall
	statuses map[string]WorkspaceStatus
}

type syncCall struct {
	done      chan struct{}
	cancel    context.CancelFunc
	result    Result
	err       error
	waiters   int
	cancelled bool
}

type Result struct {
	Text         string
	CheckpointID string
	FileCount    int
	Uploaded     int
	Added        int
	Deleted      int
}

type WorkspaceStatus struct {
	DirectoryPath  string     `json:"directory_path"`
	CheckpointID   string     `json:"checkpoint_id,omitempty"`
	FileCount      int        `json:"file_count"`
	InFlight       bool       `json:"in_flight"`
	Stage          IndexStage `json:"stage"`
	LastSyncReason SyncReason `json:"last_sync_reason,omitempty"`
	LastErrorStage IndexStage `json:"last_error_stage,omitempty"`
	LastUploaded   int        `json:"last_uploaded,omitempty"`
	LastAdded      int        `json:"last_added,omitempty"`
	LastDeleted    int        `json:"last_deleted,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	LastStartedAt  *time.Time `json:"last_started_at,omitempty"`
	LastFinishedAt *time.Time `json:"last_finished_at,omitempty"`
	StageStartedAt *time.Time `json:"stage_started_at,omitempty"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`
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
	return &Syncer{
		client:   client,
		inflight: make(map[string]*syncCall),
		statuses: make(map[string]WorkspaceStatus),
	}
}

func (s *Syncer) Retrieve(ctx context.Context, dir string, query string, maxOutputLen int) (Result, error) {
	sync, err := s.sync(ctx, dir, SyncReasonRetrieval)
	if err != nil {
		return Result{}, err
	}
	retrieveCtx, cancel := retrievalTimeoutContext(ctx)
	defer cancel()
	text, err := s.client.CodebaseRetrieval(retrieveCtx, query, ace.RetrievalOptions{
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
	return s.sync(ctx, dir, SyncReasonManual)
}

func (s *Syncer) sync(ctx context.Context, dir string, reason SyncReason) (Result, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return Result{}, err
	}
	return s.syncSingleflight(ctx, root, reason)
}

func (s *Syncer) WorkspaceStatus(ctx context.Context, dir string) (WorkspaceStatus, error) {
	if err := ctx.Err(); err != nil {
		return WorkspaceStatus{}, err
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return WorkspaceStatus{}, err
	}

	s.mu.Lock()
	if status, ok := s.statuses[root]; ok {
		s.mu.Unlock()
		return cloneWorkspaceStatus(status), nil
	}
	s.mu.Unlock()

	st, _, err := loadState(root)
	if err != nil {
		return WorkspaceStatus{}, err
	}
	return workspaceStatusFromState(root, st), nil
}

func (s *Syncer) ListWorkspaceStatuses(ctx context.Context) ([]WorkspaceStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	statuses := make([]WorkspaceStatus, 0, len(s.statuses))
	for _, status := range s.statuses {
		statuses = append(statuses, cloneWorkspaceStatus(status))
	}
	s.mu.Unlock()
	sort.Slice(statuses, func(i, j int) bool {
		return statuses[i].DirectoryPath < statuses[j].DirectoryPath
	})
	return statuses, nil
}

func (s *Syncer) syncSingleflight(ctx context.Context, root string, reason SyncReason) (Result, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}

		s.mu.Lock()
		if s.inflight == nil {
			s.inflight = make(map[string]*syncCall)
		}
		if call, ok := s.inflight[root]; ok {
			if call.cancelled {
				done := call.done
				s.mu.Unlock()
				select {
				case <-done:
					continue
				case <-ctx.Done():
					return Result{}, ctx.Err()
				}
			}
			call.waiters++
			s.mu.Unlock()
			return s.waitSyncCall(ctx, root, call)
		}

		runCtx, cancel := context.WithCancel(context.Background())
		call := &syncCall{
			done:    make(chan struct{}),
			cancel:  cancel,
			waiters: 1,
		}
		s.inflight[root] = call
		s.markSyncStartedLocked(root, reason)
		s.mu.Unlock()

		go s.runSyncCall(runCtx, root, call)
		return s.waitSyncCall(ctx, root, call)
	}
}

func (s *Syncer) runSyncCall(ctx context.Context, root string, call *syncCall) {
	result, err := s.syncRoot(ctx, root)

	s.mu.Lock()
	call.result = result
	call.err = err
	s.markSyncFinishedLocked(root, result, err)
	if current, ok := s.inflight[root]; ok && current == call {
		delete(s.inflight, root)
	}
	close(call.done)
	s.mu.Unlock()
}

func (s *Syncer) waitSyncCall(ctx context.Context, root string, call *syncCall) (Result, error) {
	select {
	case <-call.done:
		return call.result, call.err
	case <-ctx.Done():
		select {
		case <-call.done:
			return call.result, call.err
		default:
		}
		s.releaseSyncCall(root, call)
		return Result{}, ctx.Err()
	}
}

func (s *Syncer) releaseSyncCall(root string, call *syncCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.inflight[root]
	if !ok || current != call {
		return
	}
	if call.waiters > 0 {
		call.waiters--
	}
	if call.waiters == 0 {
		call.cancelled = true
		call.cancel()
	}
}

func (s *Syncer) markSyncStartedLocked(root string, reason SyncReason) {
	if s.statuses == nil {
		s.statuses = make(map[string]WorkspaceStatus)
	}
	now := time.Now().UTC()
	status := s.statuses[root]
	status.DirectoryPath = root
	status.InFlight = true
	status.Stage = IndexStageScanning
	status.StageStartedAt = &now
	status.LastSyncReason = reason
	status.LastErrorStage = ""
	status.LastStartedAt = &now
	s.statuses[root] = status
}

func (s *Syncer) markSyncStage(root string, stage IndexStage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statuses == nil {
		s.statuses = make(map[string]WorkspaceStatus)
	}
	now := time.Now().UTC()
	status := s.statuses[root]
	status.DirectoryPath = root
	status.Stage = stage
	status.StageStartedAt = &now
	status.InFlight = true
	s.statuses[root] = status
}

func (s *Syncer) markSyncFinishedLocked(root string, result Result, err error) {
	if s.statuses == nil {
		s.statuses = make(map[string]WorkspaceStatus)
	}
	now := time.Now().UTC()
	status := s.statuses[root]
	status.DirectoryPath = root
	status.InFlight = false
	status.LastFinishedAt = &now
	if err != nil {
		errorStage := status.Stage
		if errorStage == "" || errorStage == IndexStageReady {
			errorStage = IndexStageFailed
		}
		status.Stage = IndexStageFailed
		status.StageStartedAt = &now
		status.LastErrorStage = errorStage
		status.LastError = err.Error()
		s.statuses[root] = status
		return
	}
	status.Stage = IndexStageReady
	status.StageStartedAt = &now
	status.CheckpointID = result.CheckpointID
	status.FileCount = result.FileCount
	status.LastUploaded = result.Uploaded
	status.LastAdded = result.Added
	status.LastDeleted = result.Deleted
	status.LastError = ""
	status.LastErrorStage = ""
	status.UpdatedAt = &now
	s.statuses[root] = status
}

func workspaceStatusFromState(root string, st state) WorkspaceStatus {
	status := WorkspaceStatus{
		DirectoryPath: root,
		CheckpointID:  st.CheckpointID,
		FileCount:     len(st.BlobNames),
		Stage:         IndexStageIdle,
	}
	if st.CheckpointID != "" || len(st.BlobNames) > 0 {
		status.Stage = IndexStageReady
	}
	if !st.UpdatedAt.IsZero() {
		updated := st.UpdatedAt.UTC()
		status.UpdatedAt = &updated
	}
	return status
}

func cloneWorkspaceStatus(status WorkspaceStatus) WorkspaceStatus {
	status.LastStartedAt = cloneTime(status.LastStartedAt)
	status.LastFinishedAt = cloneTime(status.LastFinishedAt)
	status.StageStartedAt = cloneTime(status.StageStartedAt)
	status.UpdatedAt = cloneTime(status.UpdatedAt)
	return status
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copied := value.UTC()
	return &copied
}

func (s *Syncer) syncRoot(ctx context.Context, root string) (Result, error) {
	s.markSyncStage(root, IndexStageScanning)
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

	s.markSyncStage(root, IndexStageReconciling)
	unknown, nonindexed, err := findMissingBatched(ctx, s.client, allNames, findMissingBatchSize())
	if err != nil {
		return Result{}, err
	}
	toUpload := uniqueStrings(append(unknown, nonindexed...))
	uploads := make([]ace.BlobUpload, 0, len(toUpload))
	if len(toUpload) > 0 {
		s.markSyncStage(root, IndexStageUploading)
	}
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
		s.markSyncStage(root, IndexStageCheckpointing)
		checkpointID, err := s.client.CheckpointBlobs(ctx, st.CheckpointID, added, deleted)
		if err != nil {
			return Result{}, err
		}
		st.CheckpointID = checkpointID
	}
	st.BlobNames = current
	st.UpdatedAt = time.Now().UTC()
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
			if path != root {
				if shouldAlwaysSkipDir(name) {
					return filepath.SkipDir
				}
				localRules := loadIgnoreRulesForDir(path, rel)
				rules = append(rules, localRules...)
				if rules.Match(rel, true) && !localRules.hasAugmentInclude() && !rules.hasAugmentDescendantInclude(rel) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if rel == "" {
			rel = name
		}
		if shouldAlwaysSkipFile(rel, name) || rules.Match(rel, false) {
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

func retrievalTimeout() time.Duration {
	return positiveDurationEnv("OPENACE_RETRIEVAL_TIMEOUT", defaultRetrievalTimeout)
}

func retrievalTimeoutContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, retrievalTimeout())
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

func positiveDurationEnv(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
		return parsed
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return fallback
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
		{name: ".augmentignore", layer: ignoreLayerAugment},
	} {
		data, err := os.ReadFile(filepath.Join(dir, spec.name))
		if err != nil {
			continue
		}
		rules = append(rules, parseIgnoreRulesWithBase(string(data), base, spec.layer)...)
	}
	return rules
}

func parseIgnoreRules(data string) ignoreRules {
	return parseIgnoreRulesWithBase(data, "", ignoreLayerGit)
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
			return true
		}
		return !rule.negated && hasPathPrefix(rel, pattern)
	}
	if rule.negated {
		return matchPath(pattern, pathpkg.Base(rel))
	}
	for _, segment := range strings.Split(rel, "/") {
		if matchPath(pattern, segment) {
			return true
		}
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
