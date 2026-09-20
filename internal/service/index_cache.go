package service

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/wen-ryon/teslabox/internal/teslacam"
)

// CachedMediaIndex 是历史预览，不含任何可借用的快照或媒体路径。
type CachedMediaIndex struct {
	ID      string
	SavedAt time.Time
	Videos  []teslacam.Video
}

// CachedMedia 返回设备身份匹配的历史预览；请求核对只排队，不阻塞列表响应。
func (m *Manager) CachedMedia(requestRefresh bool) CachedMediaIndex {
	state := m.Snapshot()
	if state.MediaSnapshot != nil || !state.Backing.Ready || state.DataDrive == nil {
		return CachedMediaIndex{}
	}
	m.indexMu.Lock()
	cached := m.cachedIndex
	same := m.cachedDevice == deviceIndexKey(state)
	m.indexMu.Unlock()
	if !same || cached.ID == "" {
		return CachedMediaIndex{}
	}
	if requestRefresh {
		select {
		case m.indexRefreshCh <- struct{}{}:
		default:
		}
	}
	cached.Videos = append([]teslacam.Video(nil), cached.Videos...)
	return cached
}

func deviceIndexKey(state Snapshot) string {
	if state.DataDrive == nil || !state.Backing.Ready {
		return ""
	}
	return state.DataDrive.UUID + "|" + state.DataDrive.Serial + "|" + state.Backing.ImagePath
}

func (m *Manager) runIndexCache(ctx context.Context) {
	if m.cfg.IndexCacheDir == "" {
		return
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	var identity, savedSnapshot string
	var savedKnown int
	var lastSave time.Time
	var lastError string
	queuedRefreshIdentity := ""
	for {
		state := m.Snapshot()
		key, err := persistentIndexIdentity(state, m.cfg.IndexCacheDir)
		if err == nil {
			key += ":" + m.cfg.TeslaTimeZone
		}
		if err != nil {
			m.indexMu.Lock()
			m.cachedIndex = CachedMediaIndex{}
			m.cachedDevice = ""
			m.indexMu.Unlock()
			identity = ""
			if state.Backing.Ready && err.Error() != lastError {
				m.logger.Warn("持久化索引暂不可用，继续使用内存索引", "error", err)
				lastError = err.Error()
			}
		} else {
			lastError = ""
			if identity != key {
				identity = key
				savedSnapshot = ""
				savedKnown = -1
				lastSave = time.Time{}
				videos, at, loadErr := teslacam.LoadIndexCache(m.cfg.IndexCacheDir, key)
				m.indexMu.Lock()
				m.cachedIndex = CachedMediaIndex{}
				m.cachedDevice = deviceIndexKey(state)
				if loadErr == nil {
					m.cachedIndex = CachedMediaIndex{ID: "cache:" + key, SavedAt: at, Videos: videos}
				}
				m.indexMu.Unlock()
				// 只在没有当前快照时播种，避免迟到的缓存覆盖已扫描出的时长证据。
				if loadErr == nil && state.MediaSnapshot == nil {
					if scanner, ok := m.scanner.(interface{ SeedCachedDurations([]teslacam.Video) }); ok {
						scanner.SeedCachedDurations(videos)
					}
					m.logger.Info("已恢复历史录像索引，等待新快照核对", "video_count", len(videos))
				} else if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
					m.logger.Warn("忽略不可用的历史录像索引", "error", loadErr)
				}
				// 缓存播种完成后再排队首次核对，避免扫描和缓存恢复竞态。
				// 使用 Manager 的串行队列，不为每个浏览器重复创建后台任务。
				if queuedRefreshIdentity != identity {
					select {
					case m.indexRefreshCh <- struct{}{}:
						queuedRefreshIdentity = identity
					case <-ctx.Done():
						return
					}
				}
			}
			if state.MediaSnapshot != nil && state.indexIdentity == key && (lastSave.IsZero() || time.Since(lastSave) >= time.Minute) {
				if state.MediaSnapshot.ID != savedSnapshot {
					m.pruneThumbnailCache(state)
				}
				known := 0
				for _, v := range state.Videos {
					if teslacam.DurationKnown(v) {
						known++
					}
				}
				if state.MediaSnapshot.ID != savedSnapshot || known != savedKnown {
					// 保存的是完整已发布索引；中途失败/取消的扫描绝不写入缓存。
					err := persistentIndexWritable(m.cfg.IndexCacheDir)
					if err == nil {
						err = teslacam.SaveIndexCache(m.cfg.IndexCacheDir, key, state.Videos)
					}
					if err != nil {
						m.logger.Warn("保存历史录像索引失败，保留旧缓存", "error", err)
					} else {
						savedSnapshot = state.MediaSnapshot.ID
						savedKnown = known
						preview := append([]teslacam.Video(nil), state.Videos...)
						teslacam.ApplyPreviewDurations(preview)
						m.indexMu.Lock()
						m.cachedIndex = CachedMediaIndex{ID: "cache:" + key + ":" + savedSnapshot, SavedAt: time.Now(), Videos: preview}
						m.cachedDevice = deviceIndexKey(state)
						m.indexMu.Unlock()
					}
					lastSave = time.Now()
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
