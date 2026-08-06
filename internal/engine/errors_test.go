package engine

import (
	"errors"
	"fmt"
	"testing"
)

// P7:请求类标记的分类语义——文本原样、可穿透 %w 包装、nil 安全。
func TestInvalidRequestClassification(t *testing.T) {
	if IsInvalidRequest(nil) {
		t.Fatal("nil 不是请求类错误")
	}
	if AsInvalidRequest(nil) != nil {
		t.Fatal("nil 标记应保持 nil")
	}
	if IsInvalidRequest(errors.New("boom")) {
		t.Fatal("未标记错误不得被判为请求类")
	}
	marked := AsInvalidRequest(errors.New("directory is invalid"))
	if !IsInvalidRequest(marked) {
		t.Fatal("标记后应判为请求类")
	}
	if marked.Error() != "directory is invalid" {
		t.Fatalf("错误文本必须原样保留: %q", marked.Error())
	}
	wrapped := fmt.Errorf("outer: %w", marked)
	if !IsInvalidRequest(wrapped) {
		t.Fatal("标记应穿透 %w 包装")
	}
}
