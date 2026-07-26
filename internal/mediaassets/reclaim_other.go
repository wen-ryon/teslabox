//go:build !linux

package mediaassets

import (
	"context"
	"errors"
)

func platformReclaimSpace(context.Context, string) (SpaceReclaimResult, error) {
	return SpaceReclaimResult{}, errors.New("虚拟盘空间回收只允许在 Linux 设备上执行")
}
