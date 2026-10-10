//go:build !linux

package certdelivery

import "os"

func checkDirectory(_ string) error  { return nil }
func checkFile(_ os.FileInfo) error  { return nil }
func syncDirectory(_ *os.File) error { return nil }
