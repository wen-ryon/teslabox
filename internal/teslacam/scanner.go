package teslacam

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var videoNamePattern = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2})-([a-z_]+)\.mp4$`)

// Video 是 REST API 暴露的单路 TeslaCam 视频元数据。
type Video struct {
	ID            string         `json:"id"`
	Path          string         `json:"path"`
	ThumbnailID   string         `json:"-"`
	ThumbnailPath string         `json:"-"`
	ClipType      string         `json:"clip_type"`
	Event         string         `json:"event,omitempty"`
	EventMetadata *EventMetadata `json:"-"`
	Camera        string         `json:"camera"`
	RecordedAt    time.Time      `json:"recorded_at"`
	Size          int64          `json:"size"`
	Duration      float64        `json:"duration_seconds,omitempty"`
}

// EventMetadata 保存车辆写入的事件触发信息，避免用目录时间猜测真实触发点。
type EventMetadata struct {
	OccurredAt  *time.Time `json:"occurred_at,omitempty"`
	City        string     `json:"city,omitempty"`
	Street      string     `json:"street,omitempty"`
	Latitude    *float64   `json:"latitude,omitempty"`
	Longitude   *float64   `json:"longitude,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	TriggerType string     `json:"trigger_type,omitempty"`
	Camera      string     `json:"camera,omitempty"`
	CameraView  string     `json:"camera_view,omitempty"`
}

// Scanner 只读取目录元数据，不打开视频内容，降低扫描开销。
type Scanner struct {
	location *time.Location
}

// NewScanner 创建使用车辆文件名时区的扫描器，避免依赖板子系统时区。
func NewScanner(locations ...*time.Location) *Scanner {
	location := time.Local
	if len(locations) == 1 && locations[0] != nil {
		location = locations[0]
	}
	return &Scanner{location: location}
}

// Scan 遍历 TeslaCam 和 TeslaTrackMode，并忽略临时及未知文件。
func (s *Scanner) Scan(root string) ([]Video, error) {
	videos := make([]Video, 0, 256)
	events := make(map[string]*EventMetadata)
	thumbnails := make(map[string]string)
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
			if entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			if strings.EqualFold(entry.Name(), "event.json") {
				metadata, err := readEventMetadata(path, s.location)
				if err == nil {
					events[filepath.Clean(filepath.Dir(path))] = metadata
				}
				return nil
			}
			if strings.EqualFold(entry.Name(), "thumb.png") {
				relative, err := filepath.Rel(root, path)
				if err != nil {
					return fmt.Errorf("计算缩略图相对路径: %w", err)
				}
				thumbnails[filepath.Clean(filepath.Dir(path))] = filepath.ToSlash(relative)
				return nil
			}
			if !strings.EqualFold(filepath.Ext(entry.Name()), ".mp4") {
				return nil
			}
			video, ok, err := parseVideo(root, path, entry, s.location)
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
	for index := range videos {
		videoPath := filepath.Join(root, filepath.FromSlash(videos[index].Path))
		directory := filepath.Clean(filepath.Dir(videoPath))
		videos[index].EventMetadata = events[directory]
		videos[index].ThumbnailPath = thumbnails[directory]
		if videos[index].ThumbnailPath != "" {
			videos[index].ThumbnailID = thumbnailID(videos[index].ThumbnailPath)
		}
	}

	slices.SortFunc(videos, func(a, b Video) int {
		return b.RecordedAt.Compare(a.RecordedAt)
	})
	return videos, nil
}

func thumbnailID(relative string) string {
	digest := sha256.Sum256([]byte(filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))))
	return hex.EncodeToString(digest[:8])
}

func readEventMetadata(path string, location *time.Location) (*EventMetadata, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var raw struct {
		Timestamp string `json:"timestamp"`
		City      string `json:"city"`
		Street    string `json:"street"`
		EstLat    string `json:"est_lat"`
		EstLon    string `json:"est_lon"`
		Reason    string `json:"reason"`
		Camera    string `json:"camera"`
	}
	if err := json.NewDecoder(io.LimitReader(file, 64<<10)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("解析事件元数据: %w", err)
	}
	metadata := &EventMetadata{
		City: strings.TrimSpace(raw.City), Street: strings.TrimSpace(raw.Street),
		Reason: strings.TrimSpace(raw.Reason), Camera: strings.TrimSpace(raw.Camera),
	}
	metadata.TriggerType = classifyEventTrigger(metadata.Reason)
	metadata.CameraView = eventCameraView(metadata.Camera)
	metadata.OccurredAt = parseEventTime(strings.TrimSpace(raw.Timestamp), location)
	metadata.Latitude = parseCoordinate(raw.EstLat, -90, 90)
	metadata.Longitude = parseCoordinate(raw.EstLon, -180, 180)
	return metadata, nil
}

func classifyEventTrigger(reason string) string {
	normalized := strings.ToLower(strings.TrimSpace(reason))
	switch {
	case strings.HasPrefix(normalized, "sentry_panic"):
		return "sentry_alarm"
	case strings.HasPrefix(normalized, "sentry_aware_accel"):
		return "sentry_motion"
	case strings.HasPrefix(normalized, "sentry_aware_object"):
		return "sentry_object"
	case strings.HasPrefix(normalized, "sentry_"):
		return "sentry_other"
	case strings.HasPrefix(normalized, "user_interaction_honk"):
		return "manual_honk"
	case strings.HasPrefix(normalized, "user_interaction_dashcam_"):
		return "manual_save"
	case strings.HasPrefix(normalized, "user_interaction_"):
		return "manual_other"
	default:
		return "unknown"
	}
}

func eventCameraView(camera string) string {
	// event.json 使用的是车端感知相机编号，而录像文件使用视角名称。
	// 前向三路在 TeslaCam 中会合并为 front，未映射编号保持空值以避免错误标点。
	switch strings.TrimSpace(camera) {
	case "0", "1", "2":
		return "front"
	case "3":
		return "left_repeater"
	case "4":
		return "right_repeater"
	case "5":
		return "left_pillar"
	case "6":
		return "right_pillar"
	case "7":
		return "back"
	default:
		return ""
	}
}

func parseEventTime(value string, location *time.Location) *time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
		parsed, err := time.ParseInLocation(layout, value, location)
		if err == nil {
			return &parsed
		}
	}
	return nil
}

func parseCoordinate(value string, minimum, maximum float64) *float64 {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || parsed < minimum || parsed > maximum {
		return nil
	}
	return &parsed
}

func parseVideo(root, path string, entry fs.DirEntry, location *time.Location) (Video, bool, error) {
	matches := videoNamePattern.FindStringSubmatch(strings.ToLower(entry.Name()))
	if len(matches) != 3 {
		return Video{}, false, nil
	}
	recordedAt, err := time.ParseInLocation("2006-01-02_15-04-05", matches[1], location)
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
	duration, _ := readMP4Duration(path)
	return Video{
		ID:         hex.EncodeToString(digest[:8]),
		Path:       relative,
		ClipType:   clipType,
		Event:      event,
		Camera:     matches[2],
		RecordedAt: recordedAt,
		Size:       info.Size(),
		Duration:   duration,
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
