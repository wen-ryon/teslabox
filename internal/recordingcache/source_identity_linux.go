package recordingcache

import (
	"fmt"
	"io/fs"
	"syscall"
)

func sourceFileIdentity(info fs.FileInfo) string {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("device:%d,inode:%d", stat.Dev, stat.Ino)
	}
	return "identity:unavailable"
}
