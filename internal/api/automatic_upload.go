package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wen-ryon/teslabox/internal/media"
	"github.com/wen-ryon/teslabox/internal/nasupload"
	"github.com/wen-ryon/teslabox/internal/service"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

// EventUploadController 只提供受保护快照，不允许自动上传直接读取车机正在写入的文件系统。
type EventUploadController interface {
	AcquireEventSnapshot(context.Context) (service.Snapshot, func(), error)
	AcquireMediaSnapshot() (service.Snapshot, func())
	RecordingIdentity(service.Snapshot) (string, error)
}

// RunEventUploads 关闭时不扫描，开启后定时发现车辆事件；服务停止会取消扫描，准备任务由持久队列接管。
func RunEventUploads(ctx context.Context, controller EventUploadController, uploads *nasupload.Manager, inspector *media.Inspector, logger *slog.Logger) {
	runEventUploads(ctx, controller, uploads, inspector, logger, time.Local)
}

// RunEventUploadsInLocation 与页面共用车辆时区，避免独立事件扫描按板子时区错配分钟。
func RunEventUploadsInLocation(ctx context.Context, controller EventUploadController, uploads *nasupload.Manager, inspector *media.Inspector, logger *slog.Logger, location *time.Location) {
	runEventUploads(ctx, controller, uploads, inspector, logger, location)
}

func runEventUploads(ctx context.Context, controller EventUploadController, uploads *nasupload.Manager, inspector *media.Inspector, logger *slog.Logger, location *time.Location) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	var ready <-chan struct{}
	if source, ok := controller.(interface{ MediaSnapshotReady() <-chan struct{} }); ok {
		ready = source.MediaSnapshotReady()
	}
	scanner := newAutomaticEventScanner(location)
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-ready:
		}
		if uploads.Automatic().Enabled {
			tickCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			err := scanner.tick(tickCtx, controller, uploads, inspector, time.Now())
			cancel()
			uploads.RecordAutomaticScan(time.Now(), err)
			if err != nil && ctx.Err() == nil {
				logger.Warn("自动事件上传等待下一次核对", "error", err)
			}
		}
		timer.Reset(15 * time.Second)
	}
}

func eventUploadTick(ctx context.Context, controller EventUploadController, uploads *nasupload.Manager, inspector *media.Inspector, now time.Time) error {
	state, release, err := controller.AcquireEventSnapshot(ctx)
	if err != nil {
		return err
	}
	defer release()
	return eventUploadState(ctx, controller, uploads, inspector, now, state, nil)
}

func eventUploadState(ctx context.Context, controller EventUploadController, uploads *nasupload.Manager, inspector *media.Inspector, now time.Time, state service.Snapshot, discovered []string) error {
	identity, err := controller.RecordingIdentity(state)
	if err != nil {
		return err
	}
	groups := groupVideos(state.Videos, videoGroupTolerance)
	catalog := make(map[string][]videoGroup)
	for _, group := range groups {
		if group.ClipType == "recent" || group.Event == "" {
			continue
		}
		key := group.ClipType + "/" + group.Event
		catalog[key] = append(catalog[key], group)
	}
	directories := slices.Clone(discovered)
	var candidates []nasupload.AutoEvent
	for directory, moments := range catalog {
		directories = append(directories, directory)
		slices.SortFunc(moments, func(a, b videoGroup) int { return a.RecordedAt.Compare(b.RecordedAt) })
		catalog[directory] = moments
		metadata := moments[len(moments)-1].EventMetadata
		if !automaticVehicleEvent(metadata) || metadata.OccurredAt == nil {
			continue
		}
		policy := uploads.AutomaticPolicy(identity, directory)
		selected := automaticSelectedMoments(ctx, state.MediaSnapshot.MountPath, moments, metadata, policy, inspector)
		fingerprint, err := autoFingerprint(state.MediaSnapshot.MountPath, selected, metadata)
		if err != nil {
			return err
		}
		candidates = append(candidates, nasupload.AutoEvent{Directory: directory, Camera: metadata.CameraView, OccurredAt: *metadata.OccurredAt, Fingerprint: fingerprint, LastVersion: state.MediaSnapshot.ID})
	}
	events, err := uploads.ObserveAutomatic(identity, directories, candidates, now)
	if err != nil {
		return err
	}
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !uploads.AutomaticNeedsPreparation(event) {
			continue
		}
		moments := catalog[event.Directory]
		// 本轮未复查的已完成事件不覆盖其结果或触发新的文件读取。
		if len(moments) == 0 && len(discovered) > 0 {
			continue
		}
		if len(moments) == 0 {
			if err := uploads.SetAutomaticResult(event.ID, "waiting_files", "源录像目录已不可用，尚未暂存的片段需要录像盘恢复", nil); err != nil {
				return err
			}
			continue
		}
		if event.Camera == "" || !slices.Contains([]string{"front", "back", "left_repeater", "right_repeater", "left_pillar", "right_pillar"}, event.Camera) {
			if err := uploads.SetAutomaticResult(event.ID, "waiting_files", "事件视角无法识别，未自动改用其他摄像头", nil); err != nil {
				return err
			}
			continue
		}
		if event.Target.ID == "" {
			if err := uploads.SetAutomaticResult(event.ID, "waiting_files", "等待指定默认上传目标", nil); err != nil {
				return err
			}
			continue
		}
		indices, planErr := automaticMinuteRange(ctx, state.MediaSnapshot.MountPath, moments, event, inspector)
		var taskIDs []string
		if planErr == nil {
			// 主视频全部先准备，附加视角缺失不能阻塞其他分钟的事件主视频。
			for _, index := range indices {
				moment := moments[index]
				main := event.Target.Quality
				main.Layout = "separate"
				task, err := enqueueStableAutomaticMinute(ctx, controller, uploads, state, identity, event, moment, []string{event.Camera}, main, "event", inspector, now)
				if err != nil {
					planErr = err
					continue
				}
				taskIDs = append(taskIDs, task.ID)
			}
			if event.Policy.SupplementLayout != "" {
				for _, index := range indices {
					moment := moments[index]
					cameras := automaticLayoutCameras(event.Policy.SupplementLayout)
					task, err := enqueueStableAutomaticMinute(ctx, controller, uploads, state, identity, event, moment, cameras, event.Policy.SupplementQuality, "supplement", inspector, now)
					if err != nil {
						planErr = err
						continue
					}
					taskIDs = append(taskIDs, task.ID)
				}
			}
		}
		status, message := "queued", ""
		if planErr != nil {
			status, message = "waiting_files", planErr.Error()
		} else if event.Policy.Scope == "surrounding" && len(indices) < 3 {
			message = "前后一分钟不齐，仅上传同目录中实际存在的相邻片段；后续新增会继续补齐"
		}
		if err := uploads.SetAutomaticResult(event.ID, status, message, taskIDs); err != nil {
			return err
		}
	}
	return nil
}

func automaticVehicleEvent(metadata *teslacam.EventMetadata) bool {
	if metadata == nil {
		return false
	}
	reason := strings.ToLower(strings.TrimSpace(metadata.Reason))
	return strings.HasPrefix(reason, "sentry_") || reason == "vehicle_auto_emergency_braking"
}

func autoFingerprint(root string, moments []videoGroup, metadata *teslacam.EventMetadata) (string, error) {
	h := sha256.New()
	data, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	_, _ = h.Write(data)
	for _, moment := range moments {
		for _, video := range moment.Videos {
			name, err := snapshotVideoPath(root, video.Path)
			if err != nil {
				return "", err
			}
			info, err := os.Stat(name)
			if err != nil {
				return "", err
			}
			_, _ = fmt.Fprintf(h, "%s|%d|%d\n", video.Path, info.Size(), info.ModTime().UnixNano())
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func automaticDuration(ctx context.Context, root string, video teslacam.Video, inspector *media.Inspector) (float64, error) {
	if video.Size <= 0 {
		return 0, errors.New("等待录像文件写入完成")
	}
	if video.DurationState == "verified" && video.Duration > 0 && video.Duration <= 120 && !math.IsNaN(video.Duration) && !math.IsInf(video.Duration, 0) {
		return video.Duration, nil
	}
	if inspector == nil {
		return 0, errors.New("等待核对录像实际时长")
	}
	name, err := snapshotVideoPath(root, video.Path)
	if err != nil {
		return 0, err
	}
	meta, err := inspector.Probe(ctx, name)
	if err != nil {
		return 0, errors.New("录像尚不可读，等待写入完成")
	}
	duration, err := strconv.ParseFloat(meta.Format.Duration, 64)
	if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > 120 {
		return 0, errors.New("录像时长无效，等待下一次核对")
	}
	return duration, nil
}

func automaticMinuteRange(ctx context.Context, root string, moments []videoGroup, event nasupload.AutoEvent, inspector *media.Inspector) ([]int, error) {
	if len(moments) == 0 {
		return nil, errors.New("事件目录暂不可用")
	}
	index := -1
	for i, moment := range moments {
		for videoIndex, video := range moment.Videos {
			if video.Camera != event.Camera {
				continue
			}
			if event.OccurredAt.Before(video.RecordedAt) || i != len(moments)-1 && event.OccurredAt.Sub(video.RecordedAt) > 120*time.Second {
				continue
			}
			duration, err := automaticDuration(ctx, root, video, inspector)
			if err != nil {
				return nil, err
			}
			// 同一次冻结快照内复用刚核验的结果，范围指纹和入队不重复运行 ffprobe。
			moments[i].Videos[videoIndex].Duration = duration
			moments[i].Videos[videoIndex].DurationState = "verified"
			end := video.RecordedAt.Add(time.Duration(duration * float64(time.Second)))
			if !event.OccurredAt.Before(video.RecordedAt) && event.OccurredAt.Before(end) {
				index = i
			} else if i == len(moments)-1 && !event.OccurredAt.Before(end) {
				index = i
			}
		}
	}
	if index < 0 {
		return nil, errors.New("事件时间未命中录像，等待元数据或缺失片段补齐")
	}
	if event.Policy.Scope == "directory" {
		indices := make([]int, len(moments))
		for i := range indices {
			indices[i] = i
		}
		return indices, nil
	}
	indices := []int{index}
	if event.Policy.Scope == "surrounding" {
		if index > 0 && moments[index].RecordedAt.Sub(moments[index-1].RecordedAt) <= 62*time.Second {
			indices = append([]int{index - 1}, indices...)
		}
		if index+1 < len(moments) && moments[index+1].RecordedAt.Sub(moments[index].RecordedAt) <= 62*time.Second {
			indices = append(indices, index+1)
		}
	}
	return indices, nil
}

func automaticLayoutCameras(layout string) []string {
	return map[string][]string{"composite6": {"left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"}, "composite4a": {"front", "back", "left_repeater", "right_repeater"}, "composite4b": {"front", "back", "left_repeater", "right_repeater"}, "composite3": {"front", "left_repeater", "right_repeater"}}[layout]
}

func enqueueAutomaticMinute(ctx context.Context, controller EventUploadController, uploads *nasupload.Manager, state service.Snapshot, identity string, event nasupload.AutoEvent, moment videoGroup, cameras []string, quality nasupload.Quality, role string, inspector *media.Inspector) (nasupload.Task, error) {
	if err := ctx.Err(); err != nil {
		return nasupload.Task{}, err
	}
	var inputs []nasupload.Input
	revision := sha256.New()
	for _, camera := range cameras {
		index := slices.IndexFunc(moment.Videos, func(v teslacam.Video) bool { return v.Camera == camera })
		if index < 0 {
			return nasupload.Task{}, fmt.Errorf("%s缺少必要视角 %s，等待补齐", role, camera)
		}
		video := moment.Videos[index]
		if _, err := automaticDuration(ctx, state.MediaSnapshot.MountPath, video, inspector); err != nil {
			return nasupload.Task{}, err
		}
		name, err := snapshotVideoPath(state.MediaSnapshot.MountPath, video.Path)
		if err != nil {
			return nasupload.Task{}, err
		}
		info, err := os.Stat(name)
		if err != nil || info.Size() <= 0 {
			return nasupload.Task{}, errors.New("录像输入暂不可用")
		}
		inputs = append(inputs, nasupload.Input{MomentID: moment.ID, Camera: camera, Name: filepath.Base(name), Path: name, RecordedAt: video.RecordedAt, Size: uint64(info.Size())})
		_, _ = fmt.Fprintf(revision, "%s|%d|%d\n", video.Path, info.Size(), info.ModTime().UnixNano())
	}
	var current service.Snapshot
	var release func()
	if source, ok := controller.(interface {
		AcquireEventSnapshotLease(service.Snapshot) (service.Snapshot, func())
	}); ok {
		current, release = source.AcquireEventSnapshotLease(state)
	} else {
		current, release = controller.AcquireMediaSnapshot()
	}
	if current.MediaSnapshot == nil || current.MediaSnapshot.ID != state.MediaSnapshot.ID {
		release()
		return nasupload.Task{}, errors.New("录像快照已轮换，等待下一次核对")
	}
	anchor := slices.IndexFunc(moment.Videos, func(v teslacam.Video) bool { return v.Camera == event.Camera })
	if anchor < 0 {
		release()
		return nasupload.Task{}, errors.New("事件视角尚未补齐")
	}
	req := nasupload.Request{TargetID: event.Target.ID, SessionID: event.Directory, MomentIDs: []string{moment.ID}, Cameras: cameras, Quality: &quality, AutoEventID: event.ID, AutoKey: event.ID + "|" + moment.Videos[anchor].Path + "|" + role + "|" + hex.EncodeToString(revision.Sum(nil)), Role: role}
	return uploads.Create(req, identity, current.MediaSnapshot.MountPath, current.MediaSnapshot.ID, inputs, current.MediaContext, release)
}
