package certdelivery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateStateRejectsPublicPermissionsAndSymlinks(t *testing.T) {
	directory := t.TempDir()
	os.Chmod(directory, 0o700)
	path := filepath.Join(directory, "state.json")
	if err := writePrivate(path, []byte("private")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivate(path, 1024); err == nil {
		t.Fatal("accepted readable credential")
	}
	if err := writePrivate(path, []byte("changed")); err == nil {
		t.Fatal("overwrote unsafe credential")
	}
	os.Chmod(path, 0o600)
	link := filepath.Join(directory, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivate(link, 1024); err == nil {
		t.Fatal("accepted symlink")
	}
	if err := writePrivate(link, []byte("changed")); err == nil {
		t.Fatal("overwrote symlink")
	}
	hard := filepath.Join(directory, "hard.json")
	if err := os.Link(path, hard); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivate(path, 1024); err == nil {
		t.Fatal("accepted hard-linked credential")
	}
	os.Remove(hard)
	os.Chmod(directory, 0o755)
	if _, err := readPrivate(path, 1024); err == nil {
		t.Fatal("accepted public state directory")
	}
}
