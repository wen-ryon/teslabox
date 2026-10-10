//go:build !linux

package deviceadmin

import "os"

func defaultCarPlayDNSDirectory() string { return "" }

func carPlayDNSPermissionsSafe(info os.FileInfo) bool { return true }

func syncCarPlayDNSDirectory(path string) error { return nil }
