//go:build linux

package systeminfo

import "syscall"

func readDiskUsage(path string) *Usage {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil || stats.Blocks == 0 {
		return nil
	}
	total := stats.Blocks * uint64(stats.Bsize)
	available := stats.Bavail * uint64(stats.Bsize)
	return newUsage(total, available)
}
