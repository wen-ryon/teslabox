//go:build linux

package driveinit

import (
	"errors"
	"os"
)

// RequireRoot 阻止非 root 用户进入真实磁盘修改流程。
func RequireRoot(execute bool) error {
	if execute && os.Geteuid() != 0 {
		return errors.New("使用 --execute 时必须以 root 身份运行")
	}
	return nil
}
