//go:build !windows

package exfatreserve

import (
	"fmt"
	"os"
	"path/filepath"
)

func syncMarkerDirectory(markerPath string) error {
	directory, err := os.Open(filepath.Dir(markerPath))
	if err != nil {
		return fmt.Errorf("打开 TeslaCam 容量保护标记目录: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("同步 TeslaCam 容量保护标记目录: %w", err)
	}
	return nil
}
