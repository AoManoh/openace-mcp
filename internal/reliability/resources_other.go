//go:build !linux

package reliability

// 尚无经过验证的资源采样时不自动扩窗，快照明确显示 resource-unknown。
func readResources() resourceSnapshot { return resourceSnapshot{} }
