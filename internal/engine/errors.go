package engine

import "errors"

// P7(review 二批):daemon HTTP 面此前把所有引擎错误一刀切映射为 502,
// 目录非法/参数被拒/查询为空等"调用方可修复"错误也伪装成网关故障——
// 按状态码做重试决策的调用方会对必然再失败的输入错误盲目重试。引擎在
// 错误产生点打上请求类标记,daemon 据此 4xx/502 分流;错误文本原样保留。

// invalidRequestError 是请求类错误标记(errors.As 判定,文本直返内层)。
type invalidRequestError struct{ err error }

func (e invalidRequestError) Error() string { return e.err.Error() }
func (e invalidRequestError) Unwrap() error { return e.err }

// AsInvalidRequest 把 err 标记为调用方可修复的请求类错误(nil 直返)。
func AsInvalidRequest(err error) error {
	if err == nil {
		return nil
	}
	return invalidRequestError{err: err}
}

// IsInvalidRequest 判定 err 链上是否携带请求类标记。
func IsInvalidRequest(err error) bool {
	var target invalidRequestError
	return errors.As(err, &target)
}
