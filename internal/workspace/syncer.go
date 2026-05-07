package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

const (
	providerStateCold    = "cold"
	providerStateReady   = "ready"
	providerStateFailed  = "failed"
	providerStateBackoff = "backoff"
)

type ACEClient interface {
	FindMissing(context.Context, []string) ([]string, []string, error)
	BatchUpload(context.Context, []ace.BlobUpload) error
	CheckpointBlobs(context.Context, string, []string, []string) (string, error)
	CodebaseRetrieval(context.Context, string, ace.RetrievalOptions) (string, error)
}

type upstreamHealthReporter interface {
	HealthSnapshot() ace.HealthSnapshot
}

type ClientRouter interface {
	DefaultProviderProfileID() string
	ClientForProviderProfile(providerProfileID string) (ACEClient, error)
	HealthSnapshotForProviderProfile(providerProfileID string) (ace.HealthSnapshot, bool)
}

type Syncer struct {
	router   ClientRouter
	mu       sync.Mutex
	inflight map[string]*syncCall
	statuses map[string]WorkspaceStatus
}

type stateKey struct {
	root              string
	providerProfileID string
}

func (k stateKey) mapKey() string {
	if k.providerProfileID == "" {
		return k.root
	}
	return k.providerProfileID + "\x00" + k.root
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
	Text              string
	ProviderProfileID string `json:"provider_profile_id,omitempty"`
	CheckpointID      string
	FileCount         int
	Uploaded          int
	Added             int
	Deleted           int
}

type WorkspaceStatus struct {
	DirectoryPath          string     `json:"directory_path"`
	ProviderProfileID      string     `json:"provider_profile_id,omitempty"`
	ProviderState          string     `json:"provider_state,omitempty"`
	CheckpointID           string     `json:"checkpoint_id,omitempty"`
	FileCount              int        `json:"file_count"`
	InFlight               bool       `json:"in_flight"`
	Stage                  IndexStage `json:"stage"`
	LastSyncReason         SyncReason `json:"last_sync_reason,omitempty"`
	LastErrorStage         IndexStage `json:"last_error_stage,omitempty"`
	LastUploaded           int        `json:"last_uploaded,omitempty"`
	LastAdded              int        `json:"last_added,omitempty"`
	LastDeleted            int        `json:"last_deleted,omitempty"`
	WatchEnabled           bool       `json:"watch_enabled,omitempty"`
	WatchScheduled         bool       `json:"watch_scheduled,omitempty"`
	WatchRunning           bool       `json:"watch_running,omitempty"`
	LastError              string     `json:"last_error,omitempty"`
	WatchError             string     `json:"watch_error,omitempty"`
	UpstreamStatus         string     `json:"upstream_status,omitempty"`
	UpstreamLastStatusCode int        `json:"upstream_last_status_code,omitempty"`
	UpstreamRetryAfter     string     `json:"upstream_retry_after,omitempty"`
	UpstreamBackoffUntil   *time.Time `json:"upstream_backoff_until,omitempty"`
	UpstreamLastError      string     `json:"upstream_last_error,omitempty"`
	UpstreamLastFailure    *time.Time `json:"upstream_last_failure,omitempty"`
	UpstreamLastSuccess    *time.Time `json:"upstream_last_success,omitempty"`
	LastStartedAt          *time.Time `json:"last_started_at,omitempty"`
	LastFinishedAt         *time.Time `json:"last_finished_at,omitempty"`
	StageStartedAt         *time.Time `json:"stage_started_at,omitempty"`
	LastWatchAt            *time.Time `json:"last_watch_at,omitempty"`
	NextWatchAt            *time.Time `json:"next_watch_at,omitempty"`
	LastBackgroundSyncAt   *time.Time `json:"last_background_sync_at,omitempty"`
	UpdatedAt              *time.Time `json:"updated_at,omitempty"`
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
	return NewSyncerWithRouter(staticClientRouter{client: client})
}

func NewSyncerWithRouter(router ClientRouter) *Syncer {
	return &Syncer{
		router:   router,
		inflight: make(map[string]*syncCall),
		statuses: make(map[string]WorkspaceStatus),
	}
}

type staticClientRouter struct {
	client ACEClient
}

func (r staticClientRouter) DefaultProviderProfileID() string {
	return ""
}

func (r staticClientRouter) ClientForProviderProfile(providerProfileID string) (ACEClient, error) {
	if strings.TrimSpace(providerProfileID) != "" {
		return nil, fmt.Errorf("provider_profile_id %q is not configured", providerProfileID)
	}
	if r.client == nil {
		return nil, errors.New("ACE client is not configured")
	}
	return r.client, nil
}

func (r staticClientRouter) HealthSnapshotForProviderProfile(providerProfileID string) (ace.HealthSnapshot, bool) {
	if strings.TrimSpace(providerProfileID) != "" {
		return ace.HealthSnapshot{}, false
	}
	reporter, ok := r.client.(upstreamHealthReporter)
	if !ok {
		return ace.HealthSnapshot{}, false
	}
	return reporter.HealthSnapshot(), true
}

func (s *Syncer) clientForProvider(providerProfileID string) (ACEClient, error) {
	if s.router == nil {
		return nil, errors.New("ACE client router is not configured")
	}
	return s.router.ClientForProviderProfile(providerProfileID)
}

func (s *Syncer) Retrieve(ctx context.Context, dir string, query string, maxOutputLen int) (Result, error) {
	return s.RetrieveWithProvider(ctx, dir, "", query, maxOutputLen)
}

func (s *Syncer) RetrieveWithProvider(ctx context.Context, dir string, providerProfileID string, query string, maxOutputLen int) (Result, error) {
	sync, err := s.sync(ctx, dir, providerProfileID, SyncReasonRetrieval)
	if err != nil {
		return Result{}, err
	}
	client, err := s.clientForProvider(sync.ProviderProfileID)
	if err != nil {
		return Result{}, err
	}
	retrieveCtx, cancel := retrievalTimeoutContext(ctx)
	defer cancel()
	text, err := client.CodebaseRetrieval(retrieveCtx, query, ace.RetrievalOptions{
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
	return s.SyncWithProvider(ctx, dir, "")
}

func (s *Syncer) SyncWithProvider(ctx context.Context, dir string, providerProfileID string) (Result, error) {
	return s.sync(ctx, dir, providerProfileID, SyncReasonManual)
}

func (s *Syncer) SyncBackground(ctx context.Context, dir string) (Result, error) {
	return s.SyncBackgroundWithProvider(ctx, dir, "")
}

func (s *Syncer) SyncBackgroundWithProvider(ctx context.Context, dir string, providerProfileID string) (Result, error) {
	return s.sync(ctx, dir, providerProfileID, SyncReasonBackground)
}

func (s *Syncer) sync(ctx context.Context, dir string, providerProfileID string, reason SyncReason) (Result, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return Result{}, err
	}
	key := s.stateKey(root, providerProfileID)
	return s.syncSingleflight(ctx, key, reason)
}

func (s *Syncer) stateKey(root string, providerProfileID string) stateKey {
	id := strings.TrimSpace(providerProfileID)
	defaultID := ""
	if s.router != nil {
		defaultID = strings.TrimSpace(s.router.DefaultProviderProfileID())
	}
	if id == defaultID {
		id = ""
	}
	return stateKey{root: root, providerProfileID: id}
}

func (s *Syncer) WorkspaceStatus(ctx context.Context, dir string) (WorkspaceStatus, error) {
	return s.WorkspaceStatusWithProvider(ctx, dir, "")
}

func (s *Syncer) WorkspaceStatusWithProvider(ctx context.Context, dir string, providerProfileID string) (WorkspaceStatus, error) {
	if err := ctx.Err(); err != nil {
		return WorkspaceStatus{}, err
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return WorkspaceStatus{}, err
	}
	key := s.stateKey(root, providerProfileID)
	if strings.TrimSpace(providerProfileID) != "" {
		if _, err := s.clientForProvider(key.providerProfileID); err != nil {
			return WorkspaceStatus{}, err
		}
	}

	s.mu.Lock()
	if status, ok := s.statuses[key.mapKey()]; ok {
		s.mu.Unlock()
		return s.withUpstreamHealth(key, cloneWorkspaceStatus(status)), nil
	}
	s.mu.Unlock()

	st, _, err := loadStateForProvider(root, key.providerProfileID)
	if err != nil {
		return WorkspaceStatus{}, err
	}
	return s.withUpstreamHealth(key, workspaceStatusFromState(key, st)), nil
}

func (s *Syncer) ListWorkspaceStatuses(ctx context.Context) ([]WorkspaceStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	statuses := make([]WorkspaceStatus, 0, len(s.statuses))
	for _, status := range s.statuses {
		key := s.stateKey(status.DirectoryPath, status.ProviderProfileID)
		statuses = append(statuses, s.withUpstreamHealth(key, cloneWorkspaceStatus(status)))
	}
	s.mu.Unlock()
	sort.Slice(statuses, func(i, j int) bool {
		if statuses[i].DirectoryPath == statuses[j].DirectoryPath {
			return statuses[i].ProviderProfileID < statuses[j].ProviderProfileID
		}
		return statuses[i].DirectoryPath < statuses[j].DirectoryPath
	})
	return statuses, nil
}

func (s *Syncer) WorkspaceChanged(ctx context.Context, dir string) (bool, error) {
	return s.WorkspaceChangedWithProvider(ctx, dir, "")
}

func (s *Syncer) WorkspaceChangedWithProvider(ctx context.Context, dir string, providerProfileID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	key := s.stateKey(root, providerProfileID)
	if strings.TrimSpace(providerProfileID) != "" {
		if _, err := s.clientForProvider(key.providerProfileID); err != nil {
			return false, err
		}
	}
	assets, err := FileAssetSource{}.Load(ctx, root)
	if err != nil {
		return false, err
	}
	st, _, err := loadStateForProvider(root, key.providerProfileID)
	if err != nil {
		return false, err
	}
	current := assets.blobMap()
	return !sameBlobMap(st.BlobNames, current), nil
}

func (s *Syncer) syncSingleflight(ctx context.Context, key stateKey, reason SyncReason) (Result, error) {
	mapKey := key.mapKey()
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}

		s.mu.Lock()
		if s.inflight == nil {
			s.inflight = make(map[string]*syncCall)
		}
		if call, ok := s.inflight[mapKey]; ok {
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
			return s.waitSyncCall(ctx, key, call)
		}

		runCtx, cancel := context.WithCancel(context.Background())
		call := &syncCall{
			done:    make(chan struct{}),
			cancel:  cancel,
			waiters: 1,
		}
		s.inflight[mapKey] = call
		s.markSyncStartedLocked(key, reason)
		s.mu.Unlock()

		go s.runSyncCall(runCtx, key, call)
		return s.waitSyncCall(ctx, key, call)
	}
}

func (s *Syncer) runSyncCall(ctx context.Context, key stateKey, call *syncCall) {
	result, err := s.syncRoot(ctx, key)

	s.mu.Lock()
	call.result = result
	call.err = err
	s.markSyncFinishedLocked(key, result, err)
	mapKey := key.mapKey()
	if current, ok := s.inflight[mapKey]; ok && current == call {
		delete(s.inflight, mapKey)
	}
	close(call.done)
	s.mu.Unlock()
}

func (s *Syncer) waitSyncCall(ctx context.Context, key stateKey, call *syncCall) (Result, error) {
	select {
	case <-call.done:
		return call.result, call.err
	case <-ctx.Done():
		select {
		case <-call.done:
			return call.result, call.err
		default:
		}
		s.releaseSyncCall(key, call)
		return Result{}, ctx.Err()
	}
}

func (s *Syncer) releaseSyncCall(key stateKey, call *syncCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.inflight[key.mapKey()]
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

func (s *Syncer) markSyncStartedLocked(key stateKey, reason SyncReason) {
	if s.statuses == nil {
		s.statuses = make(map[string]WorkspaceStatus)
	}
	now := time.Now().UTC()
	mapKey := key.mapKey()
	status := s.statuses[mapKey]
	status.DirectoryPath = key.root
	status.ProviderProfileID = key.providerProfileID
	status.ProviderState = providerStateCold
	status.InFlight = true
	status.Stage = IndexStageScanning
	status.StageStartedAt = &now
	status.LastSyncReason = reason
	status.LastErrorStage = ""
	status.LastStartedAt = &now
	s.statuses[mapKey] = status
}

func (s *Syncer) markSyncStage(key stateKey, stage IndexStage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statuses == nil {
		s.statuses = make(map[string]WorkspaceStatus)
	}
	now := time.Now().UTC()
	mapKey := key.mapKey()
	status := s.statuses[mapKey]
	status.DirectoryPath = key.root
	status.ProviderProfileID = key.providerProfileID
	status.Stage = stage
	status.StageStartedAt = &now
	status.InFlight = true
	if status.ProviderState == "" {
		status.ProviderState = providerStateCold
	}
	s.statuses[mapKey] = status
}

func (s *Syncer) markSyncFinishedLocked(key stateKey, result Result, err error) {
	if s.statuses == nil {
		s.statuses = make(map[string]WorkspaceStatus)
	}
	now := time.Now().UTC()
	mapKey := key.mapKey()
	status := s.statuses[mapKey]
	status.DirectoryPath = key.root
	status.ProviderProfileID = key.providerProfileID
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
		status.ProviderState = providerStateFailed
		s.statuses[mapKey] = status
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
	status.ProviderState = providerStateReady
	status.UpdatedAt = &now
	s.statuses[mapKey] = status
}

func workspaceStatusFromState(key stateKey, st state) WorkspaceStatus {
	status := WorkspaceStatus{
		DirectoryPath:     key.root,
		ProviderProfileID: key.providerProfileID,
		ProviderState:     providerStateCold,
		CheckpointID:      st.CheckpointID,
		FileCount:         len(st.BlobNames),
		Stage:             IndexStageIdle,
	}
	if st.CheckpointID != "" || len(st.BlobNames) > 0 {
		status.Stage = IndexStageReady
		status.ProviderState = providerStateReady
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
	status.LastWatchAt = cloneTime(status.LastWatchAt)
	status.NextWatchAt = cloneTime(status.NextWatchAt)
	status.LastBackgroundSyncAt = cloneTime(status.LastBackgroundSyncAt)
	status.UpstreamBackoffUntil = cloneTime(status.UpstreamBackoffUntil)
	status.UpstreamLastFailure = cloneTime(status.UpstreamLastFailure)
	status.UpstreamLastSuccess = cloneTime(status.UpstreamLastSuccess)
	status.UpdatedAt = cloneTime(status.UpdatedAt)
	return status
}

func (s *Syncer) withUpstreamHealth(key stateKey, status WorkspaceStatus) WorkspaceStatus {
	if s.router == nil {
		return status
	}
	health, ok := s.router.HealthSnapshotForProviderProfile(key.providerProfileID)
	if !ok {
		return status
	}
	if health.Status == "" && health.LastStatusCode == 0 && health.LastError == "" && health.BackoffUntil == nil && health.LastFailureAt == nil && health.LastSuccessAt == nil {
		return status
	}
	status.UpstreamStatus = health.Status
	if health.Status == "backoff" {
		status.ProviderState = providerStateBackoff
	}
	status.UpstreamLastStatusCode = health.LastStatusCode
	if health.RetryAfter > 0 {
		status.UpstreamRetryAfter = health.RetryAfter.String()
	} else {
		status.UpstreamRetryAfter = ""
	}
	status.UpstreamBackoffUntil = cloneTime(health.BackoffUntil)
	status.UpstreamLastError = health.LastError
	status.UpstreamLastFailure = cloneTime(health.LastFailureAt)
	status.UpstreamLastSuccess = cloneTime(health.LastSuccessAt)
	return status
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copied := value.UTC()
	return &copied
}

func (s *Syncer) syncRoot(ctx context.Context, key stateKey) (Result, error) {
	client, err := s.clientForProvider(key.providerProfileID)
	if err != nil {
		return Result{}, err
	}
	s.markSyncStage(key, IndexStageScanning)
	assets, err := FileAssetSource{}.Load(ctx, key.root)
	if err != nil {
		return Result{}, err
	}
	st, statePath, err := loadStateForProvider(key.root, key.providerProfileID)
	if err != nil {
		return Result{}, err
	}
	if st.BlobNames == nil {
		st.BlobNames = map[string]string{}
	}

	current := assets.blobMap()
	byName := assets.byBlobName()
	allNames := assets.blobNames()

	added, deleted := diff(st.BlobNames, current)
	if st.CheckpointID == "" {
		added = allNames
		deleted = nil
	}

	s.markSyncStage(key, IndexStageReconciling)
	unknown, nonindexed, err := findMissingBatched(ctx, client, allNames, findMissingBatchSize())
	if err != nil {
		return Result{}, err
	}
	toUpload := uniqueStrings(append(unknown, nonindexed...))
	uploads := make([]ace.BlobUpload, 0, len(toUpload))
	if len(toUpload) > 0 {
		s.markSyncStage(key, IndexStageUploading)
	}
	for _, name := range toUpload {
		asset, ok := byName[name]
		if !ok {
			continue
		}
		upload, err := asset.upload(ctx)
		if err != nil {
			return Result{}, err
		}
		uploads = append(uploads, upload)
	}
	if len(uploads) > 0 {
		if err := batchUpload(ctx, client, uploads, uploadBatchBytes()); err != nil {
			return Result{}, err
		}
	}

	if len(added) > 0 || len(deleted) > 0 || st.CheckpointID == "" {
		s.markSyncStage(key, IndexStageCheckpointing)
		checkpointID, err := client.CheckpointBlobs(ctx, st.CheckpointID, added, deleted)
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
		ProviderProfileID: key.providerProfileID,
		CheckpointID:      st.CheckpointID,
		FileCount:         len(assets),
		Uploaded:          len(uploads),
		Added:             len(added),
		Deleted:           len(deleted),
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
	return loadStateForProvider(root, "")
}

func loadStateForProvider(root string, providerProfileID string) (state, string, error) {
	path, err := stateFileForProvider(root, providerProfileID)
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
	return stateFileForProvider(root, "")
}

func stateFileForProvider(root string, providerProfileID string) (string, error) {
	cache, err := cacheRoot()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(root))
	providerProfileID = strings.TrimSpace(providerProfileID)
	if providerProfileID != "" {
		return filepath.Join(cache, "workspaces", cacheNamespace(), "profiles", safeProviderProfileID(providerProfileID), hex.EncodeToString(sum[:])+".json"), nil
	}
	return filepath.Join(cache, "workspaces", cacheNamespace(), hex.EncodeToString(sum[:])+".json"), nil
}

func safeProviderProfileID(providerProfileID string) string {
	raw := strings.TrimSpace(providerProfileID)
	providerProfileID = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, raw)
	if providerProfileID == "" || providerProfileID == "." || providerProfileID == ".." {
		return "default"
	}
	if providerProfileID != raw {
		sum := sha256.Sum256([]byte(raw))
		providerProfileID += "-" + hex.EncodeToString(sum[:8])
	}
	return providerProfileID
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

func blobMap(files []fileBlob) map[string]string {
	current := make(map[string]string, len(files))
	for _, file := range files {
		current[file.RelPath] = file.BlobName
	}
	return current
}

func sameBlobMap(left map[string]string, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for path, leftName := range left {
		if right[path] != leftName {
			return false
		}
	}
	return true
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
	if r.ProviderProfileID != "" {
		return fmt.Sprintf("provider_profile_id=%s checkpoint=%s files=%d uploaded=%d added=%d deleted=%d", r.ProviderProfileID, r.CheckpointID, r.FileCount, r.Uploaded, r.Added, r.Deleted)
	}
	return fmt.Sprintf("checkpoint=%s files=%d uploaded=%d added=%d deleted=%d", r.CheckpointID, r.FileCount, r.Uploaded, r.Added, r.Deleted)
}
