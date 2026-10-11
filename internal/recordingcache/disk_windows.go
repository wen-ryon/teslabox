//go:build windows

package recordingcache

import "golang.org/x/sys/windows"

func ensureSystemDisk(string) error { return nil }

func diskSpace(path string) (total, free uint64, err error) {
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var diskFree uint64
	err = windows.GetDiskFreeSpaceEx(ptr, &free, &total, &diskFree)
	return total, free, err
}

func syncDirectory(string) error { return nil }
