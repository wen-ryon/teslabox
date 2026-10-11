package recordingcache

import (
	"fmt"
	"path/filepath"
)

// ValidateWorkspace 确保新消费者的临时文件也计入同一系统盘预算，拒绝子目录指向外接盘。
func (s *Store) ValidateWorkspace(name string) error {
	real, err := filepath.EvalSymlinks(name)
	if err != nil {
		return fmt.Errorf("解析系统盘工作目录: %w", err)
	}
	if !within(s.cfg.MediaRoot, real) || real == s.cfg.MediaRoot {
		return fmt.Errorf("工作目录不在系统盘媒体目录内")
	}
	return ensureSystemDisk(real)
}
