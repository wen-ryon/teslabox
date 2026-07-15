//go:build linux

package gadget

import (
	"errors"
	"os"
)

// RequireRoot 阻止非 root 用户修改 configfs Gadget。
func RequireRoot(execute bool) error {
	if execute && os.Geteuid() != 0 {
		return errors.New("使用 --execute 时必须以 root 身份运行")
	}
	return nil
}
