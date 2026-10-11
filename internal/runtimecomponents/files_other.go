//go:build !linux

package runtimecomponents

import "os"

func trustedDirectory(info os.FileInfo) error { return nil }
func trustedFile(info os.FileInfo) error      { return nil }
func syncDirectory(p string) error            { return nil }

func replaceArchive(source, target string) error {
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(source, target)
}
