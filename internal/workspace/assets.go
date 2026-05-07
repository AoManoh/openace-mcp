package workspace

import (
	"context"
	"fmt"
	"sort"

	"github.com/AoManoh/openace-mcp/internal/ace"
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

type FileAssetSource struct{}

var _ AssetSource = FileAssetSource{}

func (FileAssetSource) Load(ctx context.Context, root string) (AssetSet, error) {
	files, err := scan(ctx, root)
	if err != nil {
		return nil, err
	}
	return assetSetFromFileBlobs(files), nil
}

func assetSetFromFileBlobs(files []fileBlob) AssetSet {
	assets := make(AssetSet, 0, len(files))
	for _, file := range files {
		assets = append(assets, ContextAsset{
			AbsPath:  file.AbsPath,
			RelPath:  file.RelPath,
			BlobName: file.BlobName,
		})
	}
	return assets
}

func (assets AssetSet) fileBlobs() []fileBlob {
	files := make([]fileBlob, 0, len(assets))
	for _, asset := range assets {
		files = append(files, fileBlob{
			AbsPath:  asset.AbsPath,
			RelPath:  asset.RelPath,
			BlobName: asset.BlobName,
		})
	}
	return files
}

func (assets AssetSet) blobMap() map[string]string {
	current := make(map[string]string, len(assets))
	for _, asset := range assets {
		current[asset.RelPath] = asset.BlobName
	}
	return current
}

func (assets AssetSet) byBlobName() map[string]ContextAsset {
	byName := make(map[string]ContextAsset, len(assets))
	for _, asset := range assets {
		byName[asset.BlobName] = asset
	}
	return byName
}

func (assets AssetSet) blobNames() []string {
	names := make([]string, 0, len(assets))
	for _, asset := range assets {
		names = append(names, asset.BlobName)
	}
	sort.Strings(names)
	return names
}

func (asset ContextAsset) upload(ctx context.Context) (ace.BlobUpload, error) {
	content, ok, err := readIndexableContent(ctx, asset.AbsPath, int64(maxFileBytes()))
	if err != nil {
		return ace.BlobUpload{}, err
	}
	if !ok {
		return ace.BlobUpload{}, fmt.Errorf("file is no longer indexable during sync: %s", asset.RelPath)
	}
	if currentName := blobName(asset.RelPath, content); currentName != asset.BlobName {
		return ace.BlobUpload{}, fmt.Errorf("file changed during sync: %s", asset.RelPath)
	}
	return ace.BlobUpload{
		BlobName: asset.BlobName,
		Path:     asset.RelPath,
		Content:  string(content),
	}, nil
}
