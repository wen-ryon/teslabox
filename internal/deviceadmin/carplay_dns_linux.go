package deviceadmin

import (
	"os"
	"syscall"
)

func defaultCarPlayDNSDirectory() string { return "/etc/NetworkManager/dnsmasq-shared.d" }

func carPlayDNSPermissionsSafe(info os.FileInfo) bool {
	value, ok := info.Sys().(*syscall.Stat_t)
	return ok && value.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0o022 == 0
}

func syncCarPlayDNSDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
