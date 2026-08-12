//go:build windows

package exfatreserve

// Windows 不支持对目录句柄执行 Sync；生产目标 Linux 会在发布标记后同步目录。
func syncMarkerDirectory(string) error {
	return nil
}
