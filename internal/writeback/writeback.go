// Package writeback 负责限制 Linux 脏页缓存，避免 USB Gadget 写入突发耗尽内存后长时间停顿。
package writeback

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

const (
	defaultRoot = "/proc/sys/vm"
	// DefaultBackgroundBytes 让后台尽早写回，避免 USB Gadget 的高速突发积压到内存上限。
	DefaultBackgroundBytes = 64 * 1024 * 1024
	// DefaultDirtyBytes 限制主机确认写入与底层闪存持续写入之间的最大差额。
	DefaultDirtyBytes = 256 * 1024 * 1024
)

// Config 定义写回阈值及其 procfs 根目录，测试可使用临时目录隔离系统参数。
type Config struct {
	Root            string
	BackgroundBytes uint64
	DirtyBytes      uint64
}

// ApplyDefault 为 TeslaBox 设备应用稳定写入阈值；非 Linux 开发环境不修改宿主机参数。
func ApplyDefault() error {
	if runtime.GOOS != "linux" {
		return nil
	}
	return Apply(Config{
		Root:            defaultRoot,
		BackgroundBytes: DefaultBackgroundBytes,
		DirtyBytes:      DefaultDirtyBytes,
	})
}

// Apply 以字节阈值覆盖内核比例阈值，避免内存容量差异造成不可预测的大块回写停顿。
func Apply(config Config) error {
	if config.BackgroundBytes == 0 || config.DirtyBytes == 0 {
		return fmt.Errorf("写回阈值必须大于零")
	}
	if config.BackgroundBytes >= config.DirtyBytes {
		return fmt.Errorf("后台写回阈值必须小于最大脏页阈值")
	}
	root := filepath.Clean(config.Root)
	if err := writeThreshold(root, "dirty_background_bytes", config.BackgroundBytes); err != nil {
		return err
	}
	if err := writeThreshold(root, "dirty_bytes", config.DirtyBytes); err != nil {
		return err
	}
	return nil
}

func writeThreshold(root, name string, value uint64) error {
	path := filepath.Join(root, name)
	content := []byte(strconv.FormatUint(value, 10) + "\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("写入 %s: %w", name, err)
	}
	return nil
}
