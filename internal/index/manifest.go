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

// ManifestSchemaVersion 随 manifest 结构不兼容变化而递增。
const ManifestSchemaVersion = 1

// WorkspaceIdentity 是被索引工作区的规范身份。
type WorkspaceIdentity struct {
	CanonicalPath string `json:"canonical_path"`
	PathKind      string `json:"path_kind,omitempty"`
	HostOS        string `json:"host_os,omitempty"`
}

// FileEntry 记录单个文件在本 revision 中的内容身份。
type FileEntry struct {
	ContentHash string `json:"content_hash"`
	ChunkCount  int    `json:"chunk_count"`
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
	VectorCount int `json:"vector_count,omitempty"`
}

// HasVectors 报告本 revision 是否携带向量数据文件。
func (m *Manifest) HasVectors() bool {
	return m.VectorsChecksum != ""
}

// SemanticComplete 报告语义覆盖是否完整（空仓库按完整计）。
func (m *Manifest) SemanticComplete() bool {
	return m.VectorCount >= m.Counts.Chunks
}

// Validate 检查 manifest 自身的结构完整性。
func (m *Manifest) Validate() error {
	if m.SchemaVersion != ManifestSchemaVersion {
		return fmt.Errorf("manifest schema version %d 不受支持（期望 %d）", m.SchemaVersion, ManifestSchemaVersion)
	}
	if m.Revision == "" || m.SegmentID == "" {
		return fmt.Errorf("manifest 缺少 revision/segment 身份")
	}
	if m.ChunksChecksum == "" {
		return fmt.Errorf("manifest 缺少 chunks checksum")
	}
	return nil
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
