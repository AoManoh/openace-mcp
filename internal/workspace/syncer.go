package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AoManoh/openace-mcp/internal/ace"
)

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
	RelPath  string
	BlobName string
	Content  []byte
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
	files, err := scan(root)
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

	unknown, nonindexed, err := s.client.FindMissing(ctx, allNames)
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
		uploads = append(uploads, ace.BlobUpload{
			BlobName: file.BlobName,
			Path:     file.RelPath,
			Content:  string(file.Content),
		})
	}
	if len(uploads) > 0 {
		if err := s.client.BatchUpload(ctx, uploads); err != nil {
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

func scan(root string) ([]fileBlob, error) {
	maxBytes := int64(1 << 20)
	var files []fileBlob
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if shouldSkipDir(name) && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeType != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() == 0 || info.Size() > maxBytes {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if looksBinary(content) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		files = append(files, fileBlob{
			RelPath:  rel,
			BlobName: blobName(rel, content),
			Content:  content,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	return files, nil
}

func shouldSkipDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".next", "dist", "build", "target", ".cache", ".venv", "venv", "__pycache__":
		return true
	default:
		return false
	}
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
		return state{}, "", err
	}
	return st, path, nil
}

func saveState(path string, st state) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func stateFile(root string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(cache, "openace-mcp", "workspaces", hex.EncodeToString(sum[:])+".json"), nil
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
