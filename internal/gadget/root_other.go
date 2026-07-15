//go:build !linux

package gadget

import "errors"

// RequireRoot 阻止在非 Linux 系统修改 configfs Gadget。
func RequireRoot(execute bool) error {
	if execute {
		return errors.New("--execute 只允许在 Linux 系统运行")
	}
	return nil
}
