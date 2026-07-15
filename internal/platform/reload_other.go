//go:build !linux

package platform

import "os"

// ReloadSignals 在非 Linux 环境关闭 udev 信号集成。
func ReloadSignals() []os.Signal {
	return nil
}
