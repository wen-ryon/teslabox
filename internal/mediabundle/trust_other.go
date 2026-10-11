//go:build !linux

package mediabundle

import "os"

func trustedMediaMember(os.FileInfo) error { return nil }
