//go:build race

package chunk

// raceEnabled 标记当前测试二进制启用了 race detector：插桩使 parse
// 慢约一个数量级，性能类用例（TestFlatFileSplitBounded）的计时与
// parse 超时行为失真，须跳过；正确性用例不受影响照常执行。
const raceEnabled = true
