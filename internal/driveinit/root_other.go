//go:build !linux

package driveinit

import "errors"

// RequireRoot 阻止在非 Linux 系统执行板端磁盘初始化。
func RequireRoot(execute bool) error {
	if execute {
		return errors.New("--execute 只允许在 Linux 系统运行")
	}
	return nil
}
