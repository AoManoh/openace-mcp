package workspace

import "github.com/AoManoh/openace-mcp/internal/engine"

// 测试辅助：构造 engine contract 请求，收敛测试内的重复字面量。

func wsRef(dir string) engine.WorkspaceRef {
	return engine.WorkspaceRef{DirectoryPath: dir}
}

func wsRefP(dir string, providerProfileID string) engine.WorkspaceRef {
	return engine.WorkspaceRef{DirectoryPath: dir, ProviderProfileID: providerProfileID}
}

func syncReq(dir string) engine.SyncRequest {
	return engine.SyncRequest{Workspace: wsRef(dir)}
}

func syncReqP(dir string, providerProfileID string) engine.SyncRequest {
	return engine.SyncRequest{Workspace: wsRefP(dir, providerProfileID)}
}

func searchReq(dir string, query string, maxOutputLen int) engine.SearchRequest {
	return engine.SearchRequest{Workspace: wsRef(dir), Query: query, MaxOutputLen: maxOutputLen}
}

func searchReqP(dir string, providerProfileID string, query string, maxOutputLen int) engine.SearchRequest {
	return engine.SearchRequest{Workspace: wsRefP(dir, providerProfileID), Query: query, MaxOutputLen: maxOutputLen}
}
