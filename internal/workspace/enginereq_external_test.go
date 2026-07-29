package workspace_test

import "github.com/AoManoh/openace-mcp/internal/engine"

// 测试辅助：外部测试包版本的 engine contract 请求构造。

func wsRef(dir string) engine.WorkspaceRef {
	return engine.WorkspaceRef{DirectoryPath: dir}
}

func syncReq(dir string) engine.SyncRequest {
	return engine.SyncRequest{Workspace: wsRef(dir)}
}

func searchReq(dir string, query string, maxOutputLen int) engine.SearchRequest {
	return engine.SearchRequest{Workspace: wsRef(dir), Query: query, MaxOutputLen: maxOutputLen}
}
