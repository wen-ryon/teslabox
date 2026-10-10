//go:build !linux

package automation

import "os"

// 非 Linux 平台只运行协议单元测试，不声明板端权限或掉电持久化已验证。
func validatePrivateStateFile(os.FileInfo) error { return nil }

func syncStateDirectory(string) error { return nil }
