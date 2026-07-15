package teslacam

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

var videoNamePattern = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2})-([a-z_]+)\.mp4$`)

// Video 是 REST API 暴露的单路 TeslaCam 视频元数据。
type Video struct {
	ID         string    `json:"id"`
	Path       string    `json:"path"`
	ClipType   string    `json:"clip_type"`
	Event      string    `json:"event,omitempty"`
	Camera     string    `json:"camera"`
	RecordedAt time.Time `json:"recorded_at"`
	Size       int64     `json:"size"`
}

// Scanner 只读取目录元数据，不打开视频内容，降低扫描开销。
type Scanner struct{}

// NewScanner 创建无状态扫描器。
func NewScanner() *Scanner {
	return &Scanner{}
}

// Scan 遍历 TeslaCam 和 TeslaTrackMode，并忽略临时及未知文件。
func (s *Scanner) Scan(root string) ([]Video, error) {
	videos := make([]Video, 0, 256)
	for _, directory := range []string{"TeslaCam", "TeslaTrackMode"} {
		base := filepath.Join(root, directory)
		if _, err := os.Stat(base); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("读取 %s: %w", base, err)
		}

		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 || !strings.EqualFold(filepath.Ext(entry.Name()), ".mp4") {
				return nil
			}
			video, ok, err := parseVideo(root, path, entry)
			if err != nil {
				return err
			}
			if ok {
				videos = append(videos, video)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("扫描 %s: %w", base, err)
		}
	}

	slices.SortFunc(videos, func(a, b Video) int {
		return b.RecordedAt.Compare(a.RecordedAt)
	})
	return videos, nil
}

func parseVideo(root, path string, entry fs.DirEntry) (Video, bool, error) {
	matches := videoNamePattern.FindStringSubmatch(strings.ToLower(entry.Name()))
	if len(matches) != 3 {
		return Video{}, false, nil
	}
	recordedAt, err := time.ParseInLocation("2006-01-02_15-04-05", matches[1], time.Local)
	if err != nil {
		return Video{}, false, nil
	}
	info, err := entry.Info()
	if err != nil {
		return Video{}, false, fmt.Errorf("读取视频信息: %w", err)
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return Video{}, false, fmt.Errorf("计算视频相对路径: %w", err)
	}
	relative = filepath.ToSlash(relative)
	clipType, event := classifyPath(relative)
	digest := sha256.Sum256([]byte(relative))
	return Video{
		ID:         hex.EncodeToString(digest[:8]),
		Path:       relative,
		ClipType:   clipType,
		Event:      event,
		Camera:     matches[2],
		RecordedAt: recordedAt,
		Size:       info.Size(),
	}, true, nil
}

func classifyPath(relative string) (string, string) {
	parts := strings.Split(relative, "/")
	if len(parts) < 2 {
		return "unknown", ""
	}
	if parts[0] == "TeslaTrackMode" {
		return "track", ""
	}
	clipType := map[string]string{
		"RecentClips": "recent",
		"SavedClips":  "saved",
		"SentryClips": "sentry",
	}[parts[1]]
	if clipType == "" {
		clipType = "unknown"
	}
	if (clipType == "saved" || clipType == "sentry") && len(parts) > 3 {
		return clipType, parts[2]
	}
	return clipType, ""
}
