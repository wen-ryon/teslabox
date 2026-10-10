//go:build !linux

package webui

import "os"

func privateCarPlayToken(path string, info os.FileInfo) error { return nil }
