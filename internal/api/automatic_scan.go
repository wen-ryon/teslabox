package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/wen-ryon/teslabox/internal/media"
	"github.com/wen-ryon/teslabox/internal/nasupload"
	"github.com/wen-ryon/teslabox/internal/service"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

type scannedEventSnapshotSource interface {
	AcquireScannedEventSnapshot(context.Context, func(context.Context, service.Snapshot) ([]teslacam.Video, error)) (service.Snapshot, func(), error)
}

type automaticDirectoryScan struct {
	revision  string
	checkedAt time.Time
}

type automaticEventScanner struct {
	scanner            *teslacam.Scanner
	identity           string
	lastVersion        string
	directories        map[string]automaticDirectoryScan
	scannedDirectories int
}

func newAutomaticEventScanner(location *time.Location) *automaticEventScanner {
	return &automaticEventScanner{scanner: teslacam.NewScanner(location), directories: make(map[string]automaticDirectoryScan)}
}

func (s *automaticEventScanner) tick(ctx context.Context, controller EventUploadController, uploads *nasupload.Manager, inspector *media.Inspector, now time.Time) error {
	source, ok := controller.(scannedEventSnapshotSource)
	if !ok {
		return eventUploadTick(ctx, controller, uploads, inspector, now)
	}
	var directories []string
	s.scannedDirectories = 0
	state, release, err := source.AcquireScannedEventSnapshot(ctx, func(ctx context.Context, state service.Snapshot) ([]teslacam.Video, error) {
		identity, err := controller.RecordingIdentity(state)
		if err != nil {
			return nil, err
		}
		if identity != s.identity {
			s.identity = identity
			s.lastVersion = ""
			s.directories = make(map[string]automaticDirectoryScan)
		}
		// 页面通知和兜底时钟可能指向同一冻结视图，不能重复计作独立观察。
		if s.lastVersion == state.MediaSnapshot.ID {
			return nil, nil
		}
		catalog, err := s.scanner.ScanEventDirectories(ctx, state.MediaSnapshot.MountPath)
		if err != nil {
			return nil, err
		}
		var candidates []nasupload.AutoEvent
		for _, directory := range catalog {
			directories = append(directories, directory.Key)
			metadata := directory.Metadata
			if !automaticVehicleEvent(metadata) || metadata.OccurredAt == nil {
				continue
			}
			data, _ := json.Marshal(metadata)
			hash := sha256.Sum256(data)
			candidates = append(candidates, nasupload.AutoEvent{Directory: directory.Key, Camera: metadata.CameraView, OccurredAt: *metadata.OccurredAt, DiscoveryOnly: true, MetadataFingerprint: hex.EncodeToString(hash[:])})
		}
		events, err := uploads.ObserveAutomatic(identity, directories, candidates, now)
		if err != nil {
			return nil, err
		}
		byDirectory := make(map[string]nasupload.AutoEvent, len(events))
		for _, event := range events {
			byDirectory[event.Directory] = event
		}
		var videos []teslacam.Video
		seen := make(map[string]bool, len(catalog))
		for _, directory := range catalog {
			seen[directory.Key] = true
			event, eligible := byDirectory[directory.Key]
			if !eligible || !automaticVehicleEvent(directory.Metadata) {
				continue
			}
			previous := s.directories[directory.Key]
			pending := uploads.AutomaticNeedsPreparation(event) || event.State == "waiting_files" || event.CoverageNote != ""
			// 目录 mtime 不反映已有文件的持续写入，未完成事件始终复查，完成事件每分钟兜底核对。
			if !pending && previous.revision == directory.Revision && now.Sub(previous.checkedAt) < time.Minute {
				continue
			}
			cameras := append([]string{event.Camera}, automaticLayoutCameras(event.Policy.SupplementLayout)...)
			found, err := s.scanner.ScanEventVideos(ctx, state.MediaSnapshot.MountPath, directory, cameras, event.Policy.Scope)
			if err != nil {
				return nil, err
			}
			videos = append(videos, found...)
			s.scannedDirectories++
			s.directories[directory.Key] = automaticDirectoryScan{revision: directory.Revision, checkedAt: now}
		}
		for key := range s.directories {
			if !seen[key] {
				delete(s.directories, key)
			}
		}
		return videos, nil
	})
	if err != nil {
		return err
	}
	defer release()
	if state.MediaSnapshot.ID == s.lastVersion {
		return nil
	}
	if err := eventUploadState(ctx, controller, uploads, inspector, now, state, directories); err != nil {
		return err
	}
	s.lastVersion = state.MediaSnapshot.ID
	return nil
}

func automaticSelectedMoments(ctx context.Context, root string, moments []videoGroup, metadata *teslacam.EventMetadata, policy nasupload.AutoConfig, inspector *media.Inspector) []videoGroup {
	event := nasupload.AutoEvent{Camera: metadata.CameraView, OccurredAt: *metadata.OccurredAt, Policy: policy}
	indices, err := automaticMinuteRange(ctx, root, moments, event, inspector)
	if err != nil {
		// 不可读时仅保留元数据指纹，逐分钟证据不会因这一结果被当作稳定。
		return nil
	}
	cameras := append([]string{event.Camera}, automaticLayoutCameras(policy.SupplementLayout)...)
	selected := make([]videoGroup, 0, len(indices))
	for _, index := range indices {
		moment := moments[index]
		moment.Videos = slices.Clone(moment.Videos)
		moment.Videos = slices.DeleteFunc(moment.Videos, func(v teslacam.Video) bool { return !slices.Contains(cameras, v.Camera) })
		selected = append(selected, moment)
	}
	return selected
}

func enqueueStableAutomaticMinute(ctx context.Context, controller EventUploadController, uploads *nasupload.Manager, state service.Snapshot, identity string, event nasupload.AutoEvent, moment videoGroup, cameras []string, quality nasupload.Quality, role string, inspector *media.Inspector, now time.Time) (nasupload.Task, error) {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s|%s|%s", event.Camera, event.OccurredAt.Format(time.RFC3339Nano), role)
	var anchor string
	for _, video := range moment.Videos {
		if video.Camera == event.Camera {
			anchor = video.Path
		}
	}
	if anchor == "" {
		return nasupload.Task{}, errors.New("事件视角尚未补齐")
	}
	for _, camera := range cameras {
		index := slices.IndexFunc(moment.Videos, func(v teslacam.Video) bool { return v.Camera == camera })
		if index < 0 {
			return nasupload.Task{}, fmt.Errorf("%s缺少必要视角 %s，等待补齐", role, camera)
		}
		video := moment.Videos[index]
		name, err := snapshotVideoPath(state.MediaSnapshot.MountPath, video.Path)
		if err != nil {
			return nasupload.Task{}, err
		}
		info, err := os.Stat(name)
		if err != nil {
			return nasupload.Task{}, err
		}
		_, _ = fmt.Fprintf(h, "%s|%d|%d\n", video.Path, info.Size(), info.ModTime().UnixNano())
	}
	ready, err := uploads.ObserveAutomaticInput(event.ID, role+"|"+anchor, hex.EncodeToString(h.Sum(nil)), state.MediaSnapshot.ID, now)
	if err != nil {
		return nasupload.Task{}, err
	}
	if !ready {
		return nasupload.Task{}, errors.New("等待两次不同快照及 15 秒稳定窗口确认所需录像")
	}
	return enqueueAutomaticMinute(ctx, controller, uploads, state, identity, event, moment, cameras, quality, role, inspector)
}
