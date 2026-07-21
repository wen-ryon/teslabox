//go:build !linux

package media

func ensureOutputFilesystem(string) error {
	// 非 Linux 构建只用于开发和单元测试，真实挂载边界由板端设备号校验。
	return nil
}
