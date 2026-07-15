//go:build linux

package imageinit

import (
	"errors"
	"os"
)

// RequireRoot 阻止非 root 用户执行真实镜像初始化。
func RequireRoot(execute bool) error {
	if execute && os.Geteuid() != 0 {
		return errors.New("使用 --execute 时必须以 root 身份运行")
	}
	return nil
}
