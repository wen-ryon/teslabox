//go:build !linux

package updater

import "os"

// 非 Linux 平台仅运行文件逻辑测试，不能执行设备安装事务。
func syncDirectory(string) error            { return nil }
func safeFileMetadata(os.FileInfo) error    { return nil }
func safeParentDirectory(os.FileInfo) error { return nil }
