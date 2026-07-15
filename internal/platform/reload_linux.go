//go:build linux

package platform

import (
	"os"
	"syscall"
)

// ReloadSignals 返回 udev 用于触发即时刷新的信号。
func ReloadSignals() []os.Signal {
	return []os.Signal{syscall.SIGUSR1}
}
