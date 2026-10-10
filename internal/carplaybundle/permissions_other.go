//go:build !linux

package carplaybundle

import "os"

func trustedParent(info os.FileInfo, allowSticky bool) error {
	// 产品安装仅在 ARM64 Linux 开放；其他平台保留纯归档测试能力。
	return nil
}

func trustedMember(info os.FileInfo) error {
	return nil
}
