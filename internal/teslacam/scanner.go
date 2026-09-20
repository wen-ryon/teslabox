package teslacam

import (
	"context"
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
	"sync"
	"sync/atomic"
	"time"
)

var videoNamePattern = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2})-([a-z_]+)\.mp4$`)

const (
	defaultClipDurationSeconds = 60
	durationStatusEstimated    = "estimated"
	durationStatusVerified     = "verified"
	durationStatusUnavailable  = "unavailable"
)

// Video 是 REST API 暴露的单路 TeslaCam 视频元数据。
type Video struct {
	durationCache *durationResult
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
	DurationState string         `json:"duration_state,omitempty"`
}

// 每次扫描独立分配结果，避免同名覆盖文件继承旧快照时长。
type durationResult struct {
	budget          *atomic.Int64
	proof           *durationProof
	priority        *durationPriority
	index           int
	queued          atomic.Bool
	mu              sync.Mutex
	duration        float64
	state           string
	pendingDuration float64
}

func (c *durationResult) keepProof(proof *durationProof) {
	if c.proof != nil && c.budget != nil {
		cost := int64(64 + len(c.proof.reads)*64)
		for _, read := range c.proof.reads {
			cost += int64(len(read.data))
		}
		c.budget.Add(-cost)
		c.proof = nil
	}
	if proof == nil || c.budget == nil {
		return
	}
	cost := int64(64 + len(proof.reads)*64)
	for _, read := range proof.reads {
		cost += int64(len(read.data))
	}
	if c.budget.Add(cost) <= 32*1024*1024 {
		c.proof = proof
	} else {
		c.budget.Add(-cost)
	}
}

// DurationKnown 用于跳过已有结果，避免每次校准仍为缓存命中等待一次定时器。
func DurationKnown(video Video) bool {
	c := video.durationCache
	if c == nil || !c.mu.TryLock() {
		return false
	}
	defer c.mu.Unlock()
	return c.state != ""
}

type durationPriority struct{ queue chan int }

// PrioritizeDurations 把当前页面需要的片段放入有界队列，不在请求中读取文件。
func PrioritizeDurations(videos []Video) {
	for _, video := range videos {
		c := video.durationCache
		if c == nil || c.priority == nil || !c.mu.TryLock() {
			continue
		}
		pending := c.state == ""
		c.mu.Unlock()
		if pending && c.queued.CompareAndSwap(false, true) {
			select {
			case c.priority.queue <- c.index:
			default:
				c.queued.Store(false)
			}
		}
	}
}

// TakePriorityDuration 返回当前索引的一项优先任务，队列不跨快照共享。
func TakePriorityDuration(videos []Video) (int, bool) {
	if len(videos) == 0 || videos[0].durationCache == nil || videos[0].durationCache.priority == nil {
		return 0, false
	}
	select {
	case index := <-videos[0].durationCache.priority.queue:
		videos[index].durationCache.queued.Store(false)
		return index, true
	default:
		return 0, false
	}
}

// ApplyCachedDurations 只读取共享结果，让列表请求不产生磁盘读取。
func ApplyCachedDurations(videos []Video) {
	for i := range videos {
		if cached := videos[i].durationCache; cached != nil {
			if !cached.mu.TryLock() {
				continue
			}
			if cached.state != "" {
				videos[i].Duration, videos[i].DurationState = cached.duration, cached.state
			}
			cached.mu.Unlock()
		}
	}
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

// Scanner 只建立文件索引，时长证据由后台或详情请求在当前快照内核验。
type Scanner struct {
	location *time.Location
	mu       sync.Mutex
	previous map[string]*durationResult
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
	return s.ScanContext(context.Background(), root)
}

// ScanContext 允许快照超时取消扫描，不在目录遍历期间读取视频内容。
func (s *Scanner) ScanContext(ctx context.Context, root string) ([]Video, error) {
	return s.ScanWithProgress(ctx, root, nil)
}

// ScanWithProgress 上报本轮实际发现的文件数，不把未完成扫描当作可播放索引。
func (s *Scanner) ScanWithProgress(ctx context.Context, root string, progress func(int)) ([]Video, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	videos := make([]Video, 0, 256)
	if progress != nil {
		progress(0)
		defer func() { progress(len(videos)) }()
	}
	budget := &atomic.Int64{}
	events := make(map[string]*EventMetadata)
	type thumbnailInfo struct {
		path        string
		size        int64
		modTime     time.Time
		fingerprint string
	}
	thumbnails := make(map[string]thumbnailInfo)
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
				return errors.Join(walkErr, ctx.Err())
			}
			if err := ctx.Err(); err != nil {
				return err
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
				info, err := entry.Info()
				if err != nil {
					return fmt.Errorf("读取缩略图属性: %w", err)
				}
				thumbnail := thumbnailInfo{
					path: filepath.ToSlash(relative), size: info.Size(), modTime: info.ModTime(),
				}
				// RecentClips 永远复用同一张 thumb.png，且更新时间由车机决定。
				// 只校验这一张小图的内容，避免 exFAT 粗粒度时间戳和同大小覆盖命中旧缓存。
				if strings.EqualFold(filepath.Base(filepath.Dir(path)), "RecentClips") && info.Size() <= maxCachedThumbnailSourceBytes {
					if data, readErr := os.ReadFile(path); readErr == nil {
						digest := sha256.Sum256(data)
						thumbnail.fingerprint = hex.EncodeToString(digest[:8])
					}
				}
				thumbnails[filepath.Clean(filepath.Dir(path))] = thumbnail
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
				video.durationCache = &durationResult{budget: budget}
				if previous := s.previous[video.Path]; previous != nil && previous.mu.TryLock() {
					proof, duration, state := previous.proof, previous.duration, previous.state
					if state == "" {
						duration = previous.pendingDuration
					}
					previous.mu.Unlock()
					// 这里只传递候选证据，不读取视频；新快照必须按需核验后才能复用时长。
					if (state == durationStatusVerified || state == "") && duration > 0 && proof != nil && proof.size == video.Size {
						video.durationCache.keepProof(proof)
						video.durationCache.pendingDuration = duration
					}
				}
				videos = append(videos, video)
				if progress != nil && len(videos)%64 == 0 {
					progress(len(videos))
				}
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
		thumbnail := thumbnails[directory]
		videos[index].ThumbnailPath = thumbnail.path
		if videos[index].ThumbnailPath != "" {
			videos[index].ThumbnailID = thumbnailID(videos[index].ThumbnailPath, thumbnail.size, thumbnail.modTime, thumbnail.fingerprint)
		}
	}

	slices.SortFunc(videos, func(a, b Video) int {
		return b.RecordedAt.Compare(a.RecordedAt)
	})
	priority := &durationPriority{queue: make(chan int, 256)}
	s.previous = make(map[string]*durationResult, len(videos))
	for i := range videos {
		s.previous[videos[i].Path] = videos[i].durationCache
		videos[i].durationCache.priority = priority
		videos[i].durationCache.index = i
	}
	return videos, nil
}

// ApplyPreviewDurations 仅为列表副本提供历史时长，不能作为播放或导出的核验结果。
func ApplyPreviewDurations(videos []Video) {
	ApplyCachedDurations(videos)
	for i := range videos {
		c := videos[i].durationCache
		if c == nil || !c.mu.TryLock() {
			continue
		}
		if c.state == "" && c.pendingDuration > 0 && c.proof != nil {
			videos[i].Duration, videos[i].DurationState = c.pendingDuration, "cached"
		}
		c.mu.Unlock()
	}
}

const maxCachedThumbnailSourceBytes = 4 << 20

func thumbnailID(relative string, size int64, modTime time.Time, fingerprint string) string {
	identity := fmt.Sprintf("%s\x00%d\x00%d\x00%s", filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative))), size, modTime.UnixNano(), fingerprint)
	digest := sha256.Sum256([]byte(identity))
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
	case normalized == "vehicle_auto_emergency_braking":
		return "vehicle_emergency_braking"
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
	return Video{
		ID:         hex.EncodeToString(digest[:8]),
		Path:       relative,
		ClipType:   clipType,
		Event:      event,
		Camera:     matches[2],
		RecordedAt: recordedAt,
		Size:       info.Size(),
		// TeslaCam 通常按一分钟切片。索引阶段不解析每个 MP4 的容器，
		// 避免大容量录像盘在打开页面时产生数千次随机读取。
		Duration:      defaultClipDurationSeconds,
		DurationState: durationStatusEstimated,
	}, true, nil
}

// RefineVideoDurations 按需读取指定视频的精确时长；损坏、删除或尚未写完的文件保留索引阶段的一分钟兜底值并标记不可读取。
func RefineVideoDurations(root string, videos []Video) {
	for index := range videos {
		refineVideoDuration(root, &videos[index])
	}
}

func refineVideoDuration(root string, video *Video) {
	if cached := video.durationCache; cached != nil {
		cached.mu.Lock()
		defer cached.mu.Unlock()
		if cached.state != "" {
			video.Duration, video.DurationState = cached.duration, cached.state
			return
		}
		defer func() { cached.duration, cached.state = video.Duration, video.DurationState }()
	}
	relative := filepath.Clean(filepath.FromSlash(video.Path))
	if filepath.IsAbs(relative) || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		markVideoDurationUnavailable(video)
		return
	}
	path := filepath.Join(root, relative)
	if cached := video.durationCache; cached != nil {
		if cached.pendingDuration > 0 && cached.proof.matches(path) {
			video.Duration, video.DurationState = cached.pendingDuration, durationStatusVerified
			cached.pendingDuration = 0
			return
		}
		cached.pendingDuration = 0
		cached.keepProof(nil)
	}
	duration, proof, err := readMP4DurationProof(path)
	if err == nil && duration > 0 {
		if video.durationCache != nil {
			video.durationCache.keepProof(proof)
		}
		video.Duration = duration
		video.DurationState = durationStatusVerified
		return
	}
	markVideoDurationUnavailable(video)
}

func markVideoDurationUnavailable(video *Video) {
	if video.Duration <= 0 {
		video.Duration = defaultClipDurationSeconds
	}
	video.DurationState = durationStatusUnavailable
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
