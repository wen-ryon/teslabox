//go:build !linux

package networkroles

import (
	"context"
	"errors"
	"io"
)

// RunDefault 在非 Linux 平台拒绝系统操作，离线测试直接使用隔离目录。
func RunDefault(context.Context, string, io.Writer) error {
	return errors.New("网络角色准备只支持 Linux")
}
