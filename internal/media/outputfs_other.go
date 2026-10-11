//go:build !linux

package media

func ensureOutputFilesystem(string) error {
	// 非 Linux 构建只用于开发和单元测试，真实挂载边界由板端设备号校验。
	return nil
}

func ensureSystemOutputFilesystem(string) error { return nil }

// 非 Linux 环境只用于开发测试；板端通过目录 fsync 确认文件发布及恢复日志的落盘顺序。
func syncExportDirectory(string) error { return nil }
