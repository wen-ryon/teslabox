package service

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/wen-ryon/teslabox/internal/backing"
	"github.com/wen-ryon/teslabox/internal/config"
	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/storage"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

func TestThumbnailCacheStoresByDeviceAndPrunesReplacedVersion(t *testing.T) {
	m := &Manager{cfg: config.Config{IndexCacheDir: t.TempDir()}}
	m.state = Snapshot{
		DataDrive: &storage.Device{UUID: "disk-a"}, Backing: backing.Status{Ready: true, ImagePath: "teslacam.img"},
		MediaSnapshot: &mediasnapshot.View{ID: "snapshot"},
		Videos:        []teslacam.Video{{ThumbnailID: "2222222222222222"}},
	}
	if err := m.StoreCachedThumbnail("1111111111111111", []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := m.StoreCachedThumbnail("2222222222222222", []byte("current")); err != nil {
		t.Fatal(err)
	}
	if data, _, ok := m.CachedThumbnail("2222222222222222"); !ok || string(data) != "current" {
		t.Fatalf("current cache = %q, %v", data, ok)
	}
	m.pruneThumbnailCache(m.Snapshot())
	if m.HasCachedThumbnail("1111111111111111") {
		t.Fatal("replaced thumbnail cache was not pruned")
	}
	if !m.HasCachedThumbnail("2222222222222222") {
		t.Fatal("current thumbnail cache was pruned")
	}

	name, ok := m.cachedThumbnailPath("2222222222222222")
	if !ok {
		t.Fatal("cache path unavailable")
	}
	info, err := os.Stat(name)
	if err != nil || filepath.Ext(name) != ".png" || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("cache file = %#v, %v", info, err)
	}
}

func TestThumbnailCacheDoesNotCrossDevices(t *testing.T) {
	m := &Manager{cfg: config.Config{IndexCacheDir: t.TempDir()}}
	m.state = Snapshot{DataDrive: &storage.Device{UUID: "disk-a"}, Backing: backing.Status{Ready: true, ImagePath: "teslacam.img"}}
	if err := m.StoreCachedThumbnail("1111111111111111", []byte("private")); err != nil {
		t.Fatal(err)
	}
	m.state.DataDrive = &storage.Device{UUID: "disk-b"}
	if m.HasCachedThumbnail("1111111111111111") {
		t.Fatal("thumbnail cache leaked across devices")
	}
}
