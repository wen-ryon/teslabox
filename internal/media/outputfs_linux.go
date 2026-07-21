//go:build linux

package media

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func ensureOutputFilesystem(outputDir string) error {
	ancestor := filepath.Clean(outputDir)
	for {
		if _, err := os.Stat(ancestor); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("检查导出目录所在文件系统: %w", err)
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return errors.New("找不到导出目录的已有父目录")
		}
		ancestor = parent
	}
	var outputStat syscall.Stat_t
	if err := syscall.Stat(ancestor, &outputStat); err != nil {
		return fmt.Errorf("读取导出目录所在文件系统: %w", err)
	}
	var rootStat syscall.Stat_t
	if err := syscall.Stat("/", &rootStat); err != nil {
		return fmt.Errorf("读取系统盘文件系统: %w", err)
	}
	if outputStat.Dev == rootStat.Dev {
		return fmt.Errorf("外接数据盘尚未挂载，拒绝把导出文件写入 TF 系统盘")
	}
	return nil
}
