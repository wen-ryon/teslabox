//go:build !linux

package recordingcache

import (
	"io/fs"
)

func sourceFileIdentity(fs.FileInfo) string { return "identity:unavailable" }
