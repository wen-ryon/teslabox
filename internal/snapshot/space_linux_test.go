//go:build linux

package snapshot

import "testing"

func TestGenericSpaceAcceptsNonXFS(t *testing.T) {
	total, _, err := platformSpace("/run")
	if err != nil || total == 0 {
		t.Fatalf("普通容量读取不应限定 XFS: %v", err)
	}
	if _, _, err := platformXFSSpace("/run"); err == nil {
		t.Fatal("外层检查应拒绝 tmpfs")
	}
}
