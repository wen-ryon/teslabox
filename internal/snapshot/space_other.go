//go:build !linux

package snapshot

import "math"

func platformSpace(string) (uint64, uint64, error) {
	// 非 Linux 构建只用于开发和单元测试，真实容量检查由板端 statfs 完成。
	return math.MaxUint64, math.MaxUint64, nil
}
