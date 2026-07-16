//go:build !linux

package systeminfo

func readDiskUsage(string) *Usage { return nil }
