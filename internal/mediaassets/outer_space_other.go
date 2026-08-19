//go:build !linux

package mediaassets

import "errors"

func platformOuterSpace(string) (uint64, uint64, error) {
	return 0, 0, errors.New("外层空间检查只允许在 Linux 设备上执行")
}
