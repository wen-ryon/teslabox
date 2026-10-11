package teslacam

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EventDirectory 分离事件发现与视频索引；元数据未到的目录仍参与历史基线。
type EventDirectory struct {
	Key      string
	Path     string
	Metadata *EventMetadata
	Revision string
}

// ScanEventDirectories 只查看两类事件目录及小型元数据，不遍历最近录像或读取视频。
func (s *Scanner) ScanEventDirectories(ctx context.Context, root string) ([]EventDirectory, error) {
	var result []EventDirectory
	for _, kind := range []struct{ name, key string }{{"SentryClips", "sentry"}, {"SavedClips", "saved"}} {
		base := filepath.Join(root, "TeslaCam", kind.name)
		info, err := os.Lstat(base)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		entries, err := os.ReadDir(base)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return nil, err
			}
			directory := EventDirectory{Key: kind.key + "/" + entry.Name(), Path: filepath.ToSlash(filepath.Join("TeslaCam", kind.name, entry.Name()))}
			metadataPath := filepath.Join(base, entry.Name(), "event.json")
			metadataInfo, err := os.Lstat(metadataPath)
			if err == nil && metadataInfo.Mode().IsRegular() {
				metadata, readErr := readEventMetadata(metadataPath, s.location)
				if readErr == nil {
					directory.Metadata = metadata
				}
				if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
					var readFailure *os.PathError
					// JSON 写入中间态保留目录等待；真正的设备读取失败不能当作无事件。
					if errors.As(readErr, &readFailure) {
						return nil, readErr
					}
				}
			} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			data, _ := json.Marshal(directory.Metadata)
			digest := sha256.Sum256([]byte(fmt.Sprintf("%d|%s", info.ModTime().UnixNano(), data)))
			directory.Revision = hex.EncodeToString(digest[:])
			result = append(result, directory)
		}
	}
	return result, ctx.Err()
}

// ScanEventVideos 只索引指定事件，未选视角只保留时间轴名称，不读取其文件属性。
func (s *Scanner) ScanEventVideos(ctx context.Context, root string, directory EventDirectory, cameras []string, scopes ...string) ([]Video, error) {
	parts := strings.Split(directory.Path, "/")
	if len(parts) != 3 || parts[0] != "TeslaCam" || (parts[1] != "SentryClips" && parts[1] != "SavedClips") || parts[2] == "" || parts[2] == "." || parts[2] == ".." || strings.Contains(directory.Path, "\\") {
		return nil, errors.New("事件目录必须位于受保护的 TeslaCam 事件路径")
	}
	name := filepath.Join(root, filepath.FromSlash(directory.Path))
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return nil, errors.New("事件目录不是可信目录")
	}
	entries, err := os.ReadDir(name)
	if err != nil {
		return nil, err
	}
	selected := make(map[string]bool, len(cameras))
	for _, camera := range cameras {
		selected[camera] = true
	}
	scope := "directory"
	if len(scopes) > 0 {
		scope = scopes[0]
	}
	var last time.Time
	if scope != "directory" {
		for _, entry := range entries {
			matches := videoNamePattern.FindStringSubmatch(strings.ToLower(entry.Name()))
			if len(matches) == 3 && !entry.IsDir() && entry.Type()&fs.ModeSymlink == 0 {
				at, _ := time.ParseInLocation("2006-01-02_15-04-05", matches[1], s.location)
				if at.After(last) {
					last = at
				}
			}
		}
	}
	var videos []Video
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 {
			continue
		}
		matches := videoNamePattern.FindStringSubmatch(strings.ToLower(entry.Name()))
		if len(matches) != 3 {
			continue
		}
		at, _ := time.ParseInLocation("2006-01-02_15-04-05", matches[1], s.location)
		attributes := selected[matches[2]] && eventVideoNeedsAttributes(at, last, directory.Metadata, scope)
		video, ok, err := parseVideo(root, filepath.Join(name, entry.Name()), eventVideoEntry{DirEntry: entry, attributes: attributes}, s.location)
		if err != nil {
			return nil, err
		}
		if ok {
			video.EventMetadata = directory.Metadata
			videos = append(videos, video)
		}
	}
	return videos, nil
}

func eventVideoNeedsAttributes(at, last time.Time, metadata *EventMetadata, scope string) bool {
	if scope == "directory" {
		return true
	}
	if metadata == nil || metadata.OccurredAt == nil {
		return false
	}
	delta := metadata.OccurredAt.Sub(at)
	if scope == "surrounding" {
		return delta >= -67*time.Second && delta <= 187*time.Second || !metadata.OccurredAt.Before(last) && !at.Before(last.Add(-67*time.Second))
	}
	// 最长有效片段为 120 秒；末尾兜底还需要最后一组文件，精确范围随后按视频实际时长核对。
	return delta >= -5*time.Second && delta <= 125*time.Second || !metadata.OccurredAt.Before(last) && !at.Before(last.Add(-5*time.Second))
}

type eventVideoEntry struct {
	fs.DirEntry
	attributes bool
}

func (e eventVideoEntry) Info() (fs.FileInfo, error) {
	if e.attributes {
		return e.DirEntry.Info()
	}
	return eventNameInfo{name: e.Name()}, nil
}

type eventNameInfo struct{ name string }

func (i eventNameInfo) Name() string     { return i.name }
func (eventNameInfo) Size() int64        { return 0 }
func (eventNameInfo) Mode() fs.FileMode  { return 0 }
func (eventNameInfo) ModTime() time.Time { return time.Time{} }
func (eventNameInfo) IsDir() bool        { return false }
func (eventNameInfo) Sys() any           { return nil }
