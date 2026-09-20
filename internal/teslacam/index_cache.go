package teslacam

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"time"
)

const maxIndexCacheBytes = 64 << 20
const maxIndexCacheVideos = 50000

type cachedRead struct {
	Offset int64
	Data   []byte
}
type cachedVideo struct {
	Video         Video
	Event         *EventMetadata
	ThumbnailID   string
	ThumbnailPath string
	ProofSize     int64
	Reads         []cachedRead
}
type indexPayload struct {
	Version  int
	Identity string
	SavedAt  time.Time
	Videos   []cachedVideo
}
type indexEnvelope struct {
	SHA256  string
	Payload json.RawMessage
}

func validCachedVideo(v Video) bool {
	return len(v.Path) <= 1024 && !strings.Contains(v.Path, "\\") && path.IsAbs(v.Path) == false && path.Clean(v.Path) == v.Path &&
		(strings.HasPrefix(v.Path, "TeslaCam/") || strings.HasPrefix(v.Path, "TeslaTrackMode/")) &&
		!strings.Contains(v.Path, "/../") && v.Size >= 0 && !math.IsNaN(v.Duration) && !math.IsInf(v.Duration, 0) && v.Duration >= 0 && v.Duration <= 86400 &&
		(v.DurationState == "verified" || v.DurationState == "cached" || v.DurationState == "estimated" || v.DurationState == "unavailable")
}

// SaveIndexCache 批量原子保存元数据和时长证据；失败时不覆盖上一份完整缓存。
func SaveIndexCache(directory, identity string, videos []Video) error {
	if identity == "" || len(videos) > maxIndexCacheVideos {
		return errors.New("索引缓存身份为空或数量超限")
	}
	payload := indexPayload{Version: 1, Identity: identity, SavedAt: time.Now(), Videos: make([]cachedVideo, 0, len(videos))}
	copyVideos := append([]Video(nil), videos...)
	ApplyCachedDurations(copyVideos)
	encodedBytes := 1024
	for _, v := range copyVideos {
		if !validCachedVideo(v) {
			return fmt.Errorf("索引缓存录像字段无效: %q", v.Path)
		}
		item := cachedVideo{Video: v, Event: v.EventMetadata, ThumbnailID: v.ThumbnailID, ThumbnailPath: v.ThumbnailPath}
		if c := v.durationCache; c != nil && c.mu.TryLock() {
			if c.proof != nil && (c.state == durationStatusVerified || (c.state == "" && c.pendingDuration > 0)) {
				item.ProofSize = c.proof.size
				item.Video.Duration = c.duration
				item.Video.DurationState = c.state
				if c.state == "" {
					item.Video.Duration, item.Video.DurationState = c.pendingDuration, "cached"
				}
				for _, r := range c.proof.reads {
					item.Reads = append(item.Reads, cachedRead{Offset: r.offset, Data: append([]byte(nil), r.data...)})
				}
			}
			c.mu.Unlock()
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return err
		}
		encodedBytes += len(encoded) + 1
		if encodedBytes > maxIndexCacheBytes/2 {
			return errors.New("索引缓存超过容量上限")
		}
		payload.Videos = append(payload.Videos, item)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if len(data) > maxIndexCacheBytes/2 {
		return errors.New("索引缓存超过容量上限")
	}
	sum := sha256.Sum256(data)
	envelope, err := json.Marshal(indexEnvelope{SHA256: hex.EncodeToString(sum[:]), Payload: data})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	// 固定临时文件避免断电残留文件无限积累；生产环境仅一个缓存写入者。
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := root.OpenFile("index.pending", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove("index.pending")
	if _, err = file.Write(envelope); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = root.Rename("index.pending", "index.json"); err != nil {
		return err
	}
	// Linux 上同步目录项，断电后最多回退到旧的完整文件。
	if dir, err := os.Open(directory); err == nil {
		defer dir.Close()
		_ = dir.Sync()
	}
	return nil
}

// LoadIndexCache 校验版本、身份、校验和及资源边界，只返回历史预览与待核对时长证据。
func LoadIndexCache(directory, identity string) ([]Video, time.Time, error) {
	fail := func(err error) ([]Video, time.Time, error) { return nil, time.Time{}, err }
	root, err := os.OpenRoot(directory)
	if err != nil {
		return fail(err)
	}
	defer root.Close()
	file, err := root.Open("index.json")
	if err != nil {
		return fail(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxIndexCacheBytes {
		return fail(errors.New("索引缓存文件无效或过大"))
	}
	data, err := io.ReadAll(io.LimitReader(file, maxIndexCacheBytes+1))
	if err != nil {
		return fail(err)
	}
	if len(data) > maxIndexCacheBytes {
		return fail(errors.New("索引缓存超过上限"))
	}
	var envelope indexEnvelope
	if err = json.Unmarshal(data, &envelope); err != nil {
		return fail(err)
	}
	sum := sha256.Sum256(envelope.Payload)
	if hex.EncodeToString(sum[:]) != envelope.SHA256 {
		return fail(errors.New("索引缓存校验失败"))
	}
	var payload indexPayload
	if err = json.Unmarshal(envelope.Payload, &payload); err != nil {
		return fail(err)
	}
	if payload.Version != 1 || identity == "" || payload.Identity != identity || payload.SavedAt.IsZero() || payload.SavedAt.After(time.Now().Add(24*time.Hour)) || len(payload.Videos) > maxIndexCacheVideos {
		return fail(errors.New("索引缓存版本或设备身份不匹配"))
	}
	result := make([]Video, 0, len(payload.Videos))
	seen := make(map[string]bool)
	budget := &atomic.Int64{}
	for _, item := range payload.Videos {
		v := item.Video
		if !validCachedVideo(v) || seen[v.Path] {
			return fail(errors.New("索引缓存录像字段无效或重复"))
		}
		seen[v.Path] = true
		v.EventMetadata = item.Event
		// 缓存预览不加载旧快照缩略图。
		proof := &durationProof{size: item.ProofSize}
		used := 0
		if len(item.Reads) > 128 {
			return fail(errors.New("时长证据数量超限"))
		}
		for _, r := range item.Reads {
			used += len(r.Data)
			if used > 4096 || len(r.Data) > 32 || len(r.Data) == 0 || r.Offset < 0 || r.Offset > item.ProofSize-int64(len(r.Data)) {
				return fail(errors.New("时长证据越界"))
			}
			proof.reads = append(proof.reads, durationRead{offset: r.Offset, data: r.Data})
		}
		if len(proof.reads) > 0 && (v.DurationState == durationStatusVerified || v.DurationState == "cached") {
			v.durationCache = &durationResult{budget: budget, duration: v.Duration, state: v.DurationState}
			if v.DurationState == "cached" {
				v.durationCache.state = ""
				v.durationCache.pendingDuration = v.Duration
			}
			v.durationCache.keepProof(proof)
		}
		result = append(result, v)
	}
	return result, payload.SavedAt, nil
}

// SeedCachedDurations 只提供待核对证据；新扫描仍必须读取当前文件匹配解析字段。
func (s *Scanner) SeedCachedDurations(videos []Video) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.previous = make(map[string]*durationResult, len(videos))
	for _, v := range videos {
		if v.durationCache != nil {
			s.previous[v.Path] = v.durationCache
		}
	}
}
