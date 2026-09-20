package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxCachedThumbnailBytes = 4 << 20
const maxThumbnailCacheBytes = 256 << 20

// CachedThumbnail 返回当前录像盘身份下的缩略图副本；缓存缺失不影响录像功能。
func (m *Manager) CachedThumbnail(id string) ([]byte, time.Time, bool) {
	name, ok := m.cachedThumbnailPath(id)
	if !ok {
		return nil, time.Time{}, false
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, time.Time{}, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxCachedThumbnailBytes {
		return nil, time.Time{}, false
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, time.Time{}, false
	}
	return data, info.ModTime(), true
}

// HasCachedThumbnail 只检查系统盘缓存，不读取图片内容。
func (m *Manager) HasCachedThumbnail(id string) bool {
	name, ok := m.cachedThumbnailPath(id)
	if !ok {
		return false
	}
	info, err := os.Stat(name)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0 && info.Size() <= maxCachedThumbnailBytes
}

// StoreCachedThumbnail 原子保存页面实际使用过的缩略图，不预读整盘图片。
func (m *Manager) StoreCachedThumbnail(id string, data []byte) error {
	name, ok := m.cachedThumbnailPath(id)
	if !ok || len(data) == 0 || len(data) > maxCachedThumbnailBytes {
		return errors.New("缩略图缓存标识或大小无效")
	}
	directory := filepath.Dir(name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".thumbnail-*.pending")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if _, err := os.Stat(name); err == nil {
		return nil
	}
	return os.Rename(temporaryName, name)
}

func (m *Manager) cachedThumbnailPath(id string) (string, bool) {
	if m.cfg.IndexCacheDir == "" || len(id) != 16 {
		return "", false
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", false
	}
	m.stateMu.RLock()
	device := deviceIndexKey(Snapshot{DataDrive: m.state.DataDrive, Backing: m.state.Backing})
	m.stateMu.RUnlock()
	if device == "" {
		return "", false
	}
	digest := sha256.Sum256([]byte(device))
	directory := filepath.Join(m.cfg.IndexCacheDir, "thumbnails", hex.EncodeToString(digest[:16]))
	return filepath.Join(directory, id+".png"), true
}

func (m *Manager) pruneThumbnailCache(state Snapshot) {
	if m.cfg.IndexCacheDir == "" || state.MediaSnapshot == nil {
		return
	}
	root := filepath.Join(m.cfg.IndexCacheDir, "thumbnails")
	current, ok := m.cachedThumbnailPathForState(state, "0000000000000000")
	if !ok {
		return
	}
	currentDirectory := filepath.Dir(current)
	keep := make(map[string]struct{})
	for _, video := range state.Videos {
		if len(video.ThumbnailID) == 16 {
			keep[video.ThumbnailID+".png"] = struct{}{}
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		directory := filepath.Join(root, entry.Name())
		if !entry.IsDir() {
			continue
		}
		files, readErr := os.ReadDir(directory)
		if readErr != nil {
			return
		}
		for _, file := range files {
			if file.IsDir() || strings.HasSuffix(file.Name(), ".pending") {
				continue
			}
			_, exists := keep[file.Name()]
			if directory != currentDirectory || !exists {
				_ = os.Remove(filepath.Join(directory, file.Name()))
			}
		}
		if directory != currentDirectory {
			_ = os.Remove(directory)
		}
	}
	pruneThumbnailCacheBudget(currentDirectory)
}

func pruneThumbnailCacheBudget(directory string) {
	type cachedFile struct {
		name    string
		size    int64
		modTime time.Time
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	files := make([]cachedFile, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".pending") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, cachedFile{name: filepath.Join(directory, entry.Name()), size: info.Size(), modTime: info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	for _, file := range files {
		if total <= maxThumbnailCacheBytes {
			break
		}
		if os.Remove(file.name) == nil {
			total -= file.size
		}
	}
}

func (m *Manager) cachedThumbnailPathForState(state Snapshot, id string) (string, bool) {
	device := deviceIndexKey(state)
	if m.cfg.IndexCacheDir == "" || device == "" {
		return "", false
	}
	digest := sha256.Sum256([]byte(device))
	return filepath.Join(m.cfg.IndexCacheDir, "thumbnails", hex.EncodeToString(digest[:16]), fmt.Sprintf("%s.png", id)), true
}
