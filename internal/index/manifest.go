// Package index 提供 local-hybrid 引擎的不可变索引持久化：
// staging 构建、checksum 校验、原子发布与 active/previous 回退。
// 任何 segment 目录发布后永不修改；发布只切换 active 指针。
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// manifest schema 版本：
//   - v1（Stage 2/3）：单 segment，字段在 Manifest 顶层。
//   - v2（Stage 4，D1）：多 segment + tombstone，支持增量 delta 与
//     跨 revision segment 共享；v1 在读取时归一为单段 v2 视图。
const (
	ManifestSchemaV1 = 1
	ManifestSchemaV2 = 2
)

// ManifestSchemaVersion 是当前构建路径写出的版本（P4-T03 切换为 v2）。
const ManifestSchemaVersion = ManifestSchemaV1

// WorkspaceIdentity 是被索引工作区的规范身份。
type WorkspaceIdentity struct {
	CanonicalPath string `json:"canonical_path"`
	PathKind      string `json:"path_kind,omitempty"`
	HostOS        string `json:"host_os,omitempty"`
}

// FileEntry 记录单个文件在本 revision 中的内容身份。
// 注记（Stage 2 review S17）：ContentHash 写入的是 workspace 扫描的
// blobName = sha256(relpath + content)，含相对路径成分——用于文件级
// 变更判定足够；embedding 复用键控使用 chunk 级纯内容 hash，与本字段
// 无关。字段名保留以维持 v1 兼容，语义以本注记为准。
type FileEntry struct {
	ContentHash string `json:"content_hash"`
	ChunkCount  int    `json:"chunk_count"`
	// SegmentID 标识当前版本 chunk 所在 segment（v2；v1 归一时填充）。
	SegmentID string `json:"segment_id,omitempty"`
	// Bytes 是该文件已索引 chunk 内容字节数（v2；S20 口径基础——
	// Counts.Bytes = Σ 存活 FileEntry.Bytes；v1 归一后为 0，compaction
	// 全量重建时补齐）。
	Bytes int64 `json:"bytes,omitempty"`
	// CoveredChunks 是该文件有向量的 chunk 数（v2；delta 构建的覆盖
	// 口径基础，暗坑 K31/K51：VectorCount = Σ 存活 CoveredChunks）。
	CoveredChunks int `json:"covered_chunks,omitempty"`
}

// SegmentRef 描述 revision 引用的一个不可变 segment（v2 起）。
// 列表序为优先级升序：基段在前、最新 delta 在后（newest-wins）。
type SegmentRef struct {
	ID             string `json:"id"`
	ChunksChecksum string `json:"chunks_checksum"`
	// ChunksIndexChecksum 校验 chunks.idx 偏移索引（P4-T08 填充）。
	ChunksIndexChecksum  string `json:"chunks_index_checksum,omitempty"`
	VectorsChecksum      string `json:"vectors_checksum,omitempty"`
	VectorsIndexChecksum string `json:"vectors_index_checksum,omitempty"`
	Counts               Counts `json:"counts"`
	VectorCount          int    `json:"vector_count,omitempty"`
}

// Counts 汇总本 revision 的规模。
type Counts struct {
	Files  int   `json:"files"`
	Chunks int   `json:"chunks"`
	Bytes  int64 `json:"bytes"`
}

// Manifest 描述一个不可变索引 revision。字段一经写入不再变更。
type Manifest struct {
	SchemaVersion    int               `json:"schema_version"`
	Workspace        WorkspaceIdentity `json:"workspace"`
	EngineID         string            `json:"engine_id"`
	EngineVersion    string            `json:"engine_version"`
	Revision         string            `json:"revision"`
	PreviousRevision string            `json:"previous_revision,omitempty"`
	PolicyHash       string            `json:"policy_hash,omitempty"`
	ChunkerID        string            `json:"chunker_id"`
	ChunkerVersion   string            `json:"chunker_version"`
	// ChunkerCapabilities 记录每语言实际使用的切分能力（ast|fallback），
	// 禁止把 fallback 上报为 ast。
	ChunkerCapabilities map[string]string    `json:"chunker_capabilities,omitempty"`
	LexicalEngine       string               `json:"lexical_engine"`
	LexicalVersion      string               `json:"lexical_version"`
	SegmentID           string               `json:"segment_id"`
	Files               map[string]FileEntry `json:"files"`
	Counts              Counts               `json:"counts"`
	// ChunksChecksum 是 segment 内 chunks.jsonl 的 sha256，
	// 打开 revision 前校验，失败即回退 previous。
	ChunksChecksum string    `json:"chunks_checksum"`
	CreatedAt      time.Time `json:"created_at"`
	ActivatedAt    time.Time `json:"activated_at"`

	// —— Stage 3 语义路字段（全部 omitempty：embedding 未启用时 manifest
	// 形状与 Stage 2 逐字节同构，旧 manifest 亦可被新代码读取，暗坑 K34）——

	// EmbeddingProvider/Model/Dimension/Dtype 记录向量身份；与 profile
	// 子树对应，禁止混用（暗坑 K24）。不含任何凭据（暗坑 K21）。
	EmbeddingProvider  string `json:"embedding_provider,omitempty"`
	EmbeddingModel     string `json:"embedding_model,omitempty"`
	EmbeddingDimension int    `json:"embedding_dimension,omitempty"`
	EmbeddingDtype     string `json:"embedding_dtype,omitempty"`
	// EmbeddingProfileHash 与索引目录 profile 段一致（阶段计划 D4）。
	EmbeddingProfileHash string `json:"embedding_profile_hash,omitempty"`
	// VectorsChecksum/VectorsIndexChecksum 校验 vectors.dat/vectors.idx；
	// 校验失败仅语义路降级，不废整个 revision（暗坑 K25）。
	VectorsChecksum      string `json:"vectors_checksum,omitempty"`
	VectorsIndexChecksum string `json:"vectors_index_checksum,omitempty"`
	// VectorCount 是已覆盖 chunk 行数；semantic coverage = VectorCount /
	// Counts.Chunks（暗坑 K31，读取时计算不落盘防口径漂移）。
	// v2 语义：存活（newest-wins）chunk 中有向量者的计数。
	VectorCount int `json:"vector_count,omitempty"`

	// —— Stage 4 v2 字段（D1；v1 manifest 读取时经 Normalize 归一）——

	// Segments 是本 revision 引用的全部 segment（优先级升序，最新在后）；
	// delta 链下 segment 可被多个 revision 共享，删除必须引用计数。
	Segments []SegmentRef `json:"segments,omitempty"`
	// Tombstones 是相对 segment 联合视图被删除/改名的 relpath（排序）；
	// 查询期在召回后统一过滤（暗坑 K39）。
	Tombstones []string `json:"tombstones,omitempty"`
}

// Normalize 把 v1 单段 manifest 归一为 v2 视图（幂等）：合成 Segments
// 并为 Files 填充 SegmentID。v2 manifest 原样返回。
func (m *Manifest) Normalize() {
	if len(m.Segments) > 0 {
		return
	}
	if m.SegmentID == "" {
		return
	}
	m.Segments = []SegmentRef{{
		ID:                   m.SegmentID,
		ChunksChecksum:       m.ChunksChecksum,
		VectorsChecksum:      m.VectorsChecksum,
		VectorsIndexChecksum: m.VectorsIndexChecksum,
		Counts:               m.Counts,
		VectorCount:          m.VectorCount,
	}}
	for path, entry := range m.Files {
		if entry.SegmentID == "" {
			entry.SegmentID = m.SegmentID
			m.Files[path] = entry
		}
	}
}

// NewestSegment 返回优先级最高（最新）的 segment 引用；
// 未 Normalize 的 v1 manifest 走 legacy 字段合成（防御路径）。
func (m *Manifest) NewestSegment() SegmentRef {
	if len(m.Segments) > 0 {
		return m.Segments[len(m.Segments)-1]
	}
	return SegmentRef{
		ID:                   m.SegmentID,
		ChunksChecksum:       m.ChunksChecksum,
		VectorsChecksum:      m.VectorsChecksum,
		VectorsIndexChecksum: m.VectorsIndexChecksum,
		Counts:               m.Counts,
		VectorCount:          m.VectorCount,
	}
}

// TombstoneSet 返回 tombstone 查找集。
func (m *Manifest) TombstoneSet() map[string]bool {
	if len(m.Tombstones) == 0 {
		return nil
	}
	set := make(map[string]bool, len(m.Tombstones))
	for _, path := range m.Tombstones {
		set[path] = true
	}
	return set
}

// HasVectors 报告本 revision 是否携带向量数据文件（任一 segment 有即有）。
func (m *Manifest) HasVectors() bool {
	if m.VectorsChecksum != "" {
		return true
	}
	for _, segment := range m.Segments {
		if segment.VectorsChecksum != "" {
			return true
		}
	}
	return false
}

// SemanticComplete 报告语义覆盖是否完整（空仓库按完整计）。
func (m *Manifest) SemanticComplete() bool {
	return m.VectorCount >= m.Counts.Chunks
}

// Validate 检查 manifest 自身的结构完整性（按版本分支；未知版本显式
// 拒绝——旧二进制读到未来格式必须报错而非静默错读，暗坑 K43）。
func (m *Manifest) Validate() error {
	switch m.SchemaVersion {
	case ManifestSchemaV1:
		if m.Revision == "" || m.SegmentID == "" {
			return fmt.Errorf("manifest 缺少 revision/segment 身份")
		}
		if m.ChunksChecksum == "" {
			return fmt.Errorf("manifest 缺少 chunks checksum")
		}
		return nil
	case ManifestSchemaV2:
		if m.Revision == "" {
			return fmt.Errorf("manifest 缺少 revision 身份")
		}
		if len(m.Segments) == 0 {
			return fmt.Errorf("v2 manifest 缺少 segments")
		}
		for i, segment := range m.Segments {
			if segment.ID == "" || segment.ChunksChecksum == "" {
				return fmt.Errorf("v2 manifest 第 %d 个 segment 缺少 id/chunks checksum", i)
			}
		}
		return nil
	default:
		return fmt.Errorf("manifest schema version %d 不受支持（本构建最高支持 %d）", m.SchemaVersion, ManifestSchemaV2)
	}
}

// activePointer 是 active.json 的内容。
type activePointer struct {
	Revision string `json:"revision"`
}

// ChecksumFile 计算文件 sha256 hex。
func ChecksumFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// decodeJSONFile 读取并解码 JSON 文件。
func decodeJSONFile(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}
