//go:build !linux

package webtls

import "os"

func checkPrivateDirectory(directory string) error { return nil }

func checkPrivateBundle(_ os.FileInfo) error { return nil }
