package workspace

import (
	"context"
)

// ContextAsset is the minimal file-backed asset shape used by the workspace pipeline.
type ContextAsset struct {
	AbsPath  string
	RelPath  string
	BlobName string
}

type AssetSet []ContextAsset

type AssetSource interface {
	Load(ctx context.Context, root string) (AssetSet, error)
}

// FileAssetSource 扫描 workspace 产出资产集;Cache 非 nil 时按
// (size,mtime) 短路复用 blobName(T11 定值,长驻进程每 workspace
// 一个实例),nil 时行为与历史逐字节一致。Progress 非 nil 时在扫描期
// 周期性回报已收录文件数(P-gray-01:大仓首扫不再长期显示 files=0)。
type FileAssetSource struct {
	Cache    *StatCache
	Progress func(scanned int)
}

var _ AssetSource = FileAssetSource{}

func (s FileAssetSource) Load(ctx context.Context, root string) (AssetSet, error) {
	assets, _, err := s.LoadWithStats(ctx, root)
	return assets, err
}

// LoadWithStats 同 Load,并回传扫描跳过统计(K6 上抛口径:调用方状态面
// 如实上报权限跳过数,不静默吞)。
func (s FileAssetSource) LoadWithStats(ctx context.Context, root string) (AssetSet, ScanStats, error) {
	files, stats, err := scanWithProgress(ctx, root, s.Cache, s.Progress)
	if err != nil {
		return nil, stats, err
	}
	return assetSetFromFileBlobs(files), stats, nil
}

func assetSetFromFileBlobs(files []fileBlob) AssetSet {
	assets := make(AssetSet, 0, len(files))
	for _, file := range files {
		assets = append(assets, ContextAsset(file))
	}
	return assets
}

func (assets AssetSet) fileBlobs() []fileBlob {
	files := make([]fileBlob, 0, len(assets))
	for _, asset := range assets {
		files = append(files, fileBlob(asset))
	}
	return files
}

// ReadIndexableContent 以与 workspace 扫描一致的内容门禁读取文件：
// 常规文件、大小上限(含纯文本超限带)、非二进制、合法 UTF-8。ok=false 表示
// 文件不可索引。local-hybrid 引擎复用该入口，禁止绕开 AssetPolicy 自建
// 第二套判定。
func ReadIndexableContent(ctx context.Context, absPath string) ([]byte, bool, error) {
	content, ok, _, err := readIndexableContent(ctx, absPath, int64(maxFileBytes()), int64(maxTextFileBytes()))
	return content, ok, err
}
