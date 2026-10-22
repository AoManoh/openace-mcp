package localengine

import (
	"fmt"
	"math"

	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/vector"
)

func vectorRowBytes(dimension int) (int64, bool) {
	if dimension <= 0 || int64(dimension) > math.MaxInt64/4 {
		return 0, false
	}
	return int64(dimension) * 4, true
}

func vectorRowsWithin(budget int64, dimension int) int {
	rowBytes, ok := vectorRowBytes(dimension)
	if !ok || budget <= 0 {
		return 0
	}
	rows := budget / rowBytes
	if rows > math.MaxInt {
		return math.MaxInt
	}
	return int(rows)
}

func markUnloadedKeys(prior *priorVectors, store *index.Store, segment index.SegmentRef, dimension int, needed map[string]bool) {
	reader, err := vector.OpenRowReader(store.SegmentPathFor(segment.ID), dimension, segment.VectorsIndexChecksum)
	if err != nil {
		return
	}
	defer func() { _ = reader.Close() }()
	for _, entry := range reader.Entries() {
		if needed != nil && !needed[entry.ContentHash] {
			continue
		}
		if _, ok := prior.activeByHash[entry.ContentHash]; ok {
			continue
		}
		if _, ok := prior.olderByHash[entry.ContentHash]; ok {
			continue
		}
		prior.unloaded[entry.ContentHash] = true
	}
}

func (e *Engine) residentRowLimit() (int, bool) {
	if e.vectorMemoryBudget <= 0 && e.vectorMaxResident <= 0 {
		return 0, false
	}
	return e.vectorMaxResident, true
}

func saturatingAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func (e *Engine) budgetBlock(journal *index.Journal, retainedRows int64, possibleRows int, missing []string) string {
	if e.vectorMemoryBudget <= 0 {
		return ""
	}
	rowBytes, ok := vectorRowBytes(e.embedCfg.Dimension)
	if !ok {
		return fmt.Sprintf("vector memory budget cannot hold a %d-dimensional vector row (%s); missing chunks left uncovered explicitly", e.embedCfg.Dimension, EnvVectorMemoryBudget)
	}
	rowLimit := e.vectorMemoryBudget / rowBytes
	rows := saturatingAdd(retainedRows, int64(possibleRows))
	if rows > rowLimit {
		return fmt.Sprintf("vector memory budget exceeded before embedding: %d physical vector rows (%d retained + %d possible in this build) > %d rows that fit in %d bytes at %d bytes per row (%s); missing chunks left uncovered explicitly",
			rows, retainedRows, possibleRows, rowLimit, e.vectorMemoryBudget, rowBytes, EnvVectorMemoryBudget)
	}
	current, needed, ok := journal.CapacityFor(missing)
	if !ok {
		return fmt.Sprintf("journal byte budget exceeded before embedding: %d existing bytes + %d bytes for %d new keys > %d bytes (%s); missing chunks left uncovered explicitly",
			current, needed, len(missing), e.vectorMemoryBudget, EnvVectorMemoryBudget)
	}
	return ""
}

func loadPriorRowsVerified(prior *priorVectors, store *index.Store, segment index.SegmentRef, dimension int, hop int, needed map[string]bool, limit int, limited bool, loadedRows int) int {
	target := prior.activeByHash
	if hop != 0 {
		target = prior.olderByHash
	}
	maxRows := -1
	if limited {
		maxRows = limit - loadedRows
		if maxRows < 0 {
			maxRows = 0
		}
	}
	chosen := make(map[string]bool)
	loaded, err := vector.LoadRows(store.SegmentPathFor(segment.ID), dimension, segment.VectorsChecksum, segment.VectorsIndexChecksum, func(entry vector.Entry) bool {
		key := entry.ContentHash
		if !needed[key] || chosen[key] {
			return false
		}
		if _, ok := prior.activeByHash[key]; ok {
			return false
		}
		if _, ok := prior.olderByHash[key]; ok {
			return false
		}
		chosen[key] = true
		return true
	}, maxRows)
	if err != nil {
		return loadedRows
	}
	if hop == 0 {
		prior.activeLoadedRows += loaded.Count
		prior.activeLoadedSegments++
	}
	for i, entry := range loaded.Entries {
		target[entry.ContentHash] = loaded.Data[i]
	}
	for _, entry := range loaded.Skipped {
		prior.unloaded[entry.ContentHash] = true
	}
	loadedRows += len(loaded.Entries)
	prior.loadedRows = loadedRows
	return loadedRows
}

func (e *Engine) budgetedKeys(records []chunkRecord) map[string]bool {
	if _, limited := e.residentRowLimit(); !limited {
		return nil
	}
	needed := make(map[string]bool, len(records))
	for _, record := range records {
		needed[embedKey(record)] = true
	}
	return needed
}

func (e *Engine) loadSiblingRows(store *index.Store, manifest *index.Manifest, prior *priorVectors, needed map[string]bool) priorVectors {
	want := make(map[string]bool, len(needed))
	for key := range needed {
		if _, ok := prior.activeByHash[key]; ok {
			continue
		}
		if _, ok := prior.olderByHash[key]; ok {
			continue
		}
		if _, ok := prior.crossProfileByHash[key]; ok {
			continue
		}
		want[key] = true
	}
	return e.loadPriorVectorsFrom(store, manifest, want, true, prior.loadedRows)
}

func (e *Engine) unloadedPauseError(count int) error {
	return fmt.Errorf("%s=%d cannot load %d existing paid vectors needed by this build; build paused before any provider request and the current revision stays in service; raise %s to resume without paying again",
		EnvVectorMemoryBudget, e.vectorMemoryBudget, count, EnvVectorMemoryBudget)
}
