package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wen-ryon/teslabox/internal/recordingcache"
)

// UsesSystemDisk 让请求入口按实际输出落点选择空间保护，不再预留外接盘成品空间。
func (e *Exporter) UsesSystemDisk() bool { return e.config.Cache != nil }

func (e *Exporter) ensureOutputFilesystem() error {
	if e.config.Cache != nil {
		return ensureSystemOutputFilesystem(e.config.OutputDir)
	}
	return ensureOutputFilesystem(e.config.OutputDir)
}

func selectedExportPaths(request ExportRequest, source ExportSource) ([]string, error) {
	selected := make(map[string]bool)
	if request.Mode == ExportModeSource {
		for _, file := range source.SourceFiles {
			selected[file.Path] = true
		}
	} else {
		segments, err := planClipExport(request, source)
		if err != nil {
			return nil, err
		}
		for _, segment := range segments {
			for _, video := range segment.source.Videos {
				selected[video.Path] = true
			}
		}
		// 现有 SEI 与拍摄时间优先取前视，保留这项依赖，避免用户切换输出视角时仪表改变。
		if request.BurnSEI || request.BurnTimestamp {
			camera := "front"
			if !slices.ContainsFunc(source.Videos, func(video ExportVideo) bool { return video.Camera == camera }) {
				camera = ""
				for _, candidate := range request.Cameras {
					if slices.ContainsFunc(source.Videos, func(video ExportVideo) bool { return video.Camera == candidate }) {
						camera = candidate
						break
					}
				}
			}
			start := source.SessionStart.Add(time.Duration(request.StartSeconds * float64(time.Second)))
			end := start.Add(time.Duration(request.DurationSeconds * float64(time.Second)))
			for _, video := range source.Videos {
				if video.Camera == camera && video.RecordedAt.Before(end) && video.RecordedAt.Add(time.Duration(video.Duration*float64(time.Second))).After(start) {
					selected[video.Path] = true
				}
			}
		}
	}
	paths := make([]string, 0, len(selected))
	for name := range selected {
		if name == "" {
			return nil, errors.New("任务输入包含空录像路径")
		}
		paths = append(paths, name)
	}
	slices.Sort(paths)
	return paths, nil
}

func localExportPaths(source ExportSource) []string {
	var paths []string
	for _, video := range source.Videos {
		if video.Path != "" {
			paths = append(paths, video.Path)
		}
	}
	for _, file := range source.SourceFiles {
		paths = append(paths, file.Path)
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

func (e *Exporter) stageSource(ctx context.Context, job *exportJob) (*recordingcache.Lease, func(), error) {
	if job.source.SourceIdentity == "" || job.source.SourceRoot == "" || job.source.SnapshotVersion == "" {
		return nil, nil, errors.New("录像盘身份或快照版本不可用，拒绝暂存")
	}
	paths, err := selectedExportPaths(job.request, job.source)
	if err != nil {
		return nil, nil, err
	}
	files := make([]recordingcache.File, 0, len(paths))
	for _, path := range paths {
		relative, err := filepath.Rel(job.source.SourceRoot, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return nil, nil, errors.New("录像输入不在已确认的只读快照中")
		}
		files = append(files, recordingcache.File{Identity: job.source.SourceIdentity + "|" + filepath.ToSlash(relative), Path: path, Version: job.source.SnapshotVersion})
	}
	outputBytes, err := exportOutputWorkingBytes(job.request, job.source)
	if err != nil {
		return nil, nil, err
	}
	lease, releaseSpace, err := e.config.Cache.EnsureLocalReserved(ctx, files, outputBytes, func(done, total uint64) {
		e.mu.Lock()
		task := e.tasks[job.id]
		task.CopiedBytes, task.CopyTotalBytes = done, total
		e.tasks[job.id] = task
		e.mu.Unlock()
	})
	if err != nil {
		return nil, nil, err
	}
	job.source.Videos = slices.Clone(job.source.Videos)
	job.source.SourceFiles = slices.Clone(job.source.SourceFiles)
	for index := range job.source.Videos {
		// 未使用片段只保留时间轴元数据，不能在持久计划中残留已经释放的快照路径。
		job.source.Videos[index].Path = lease.Paths[job.source.Videos[index].Path]
	}
	for index := range job.source.SourceFiles {
		job.source.SourceFiles[index].Path = lease.Paths[job.source.SourceFiles[index].Path]
	}
	job.source.MediaContext = nil
	job.source.SourceRoot = ""
	return lease, releaseSpace, nil
}

func (e *Exporter) reserveOutput(ctx context.Context, request ExportRequest, source ExportSource) (func(), error) {
	bytes, err := exportOutputWorkingBytes(request, source)
	if err != nil {
		return nil, err
	}
	release, err := e.config.Cache.Reserve(ctx, bytes)
	if err != nil {
		return nil, fmt.Errorf("预留系统盘导出空间: %w", err)
	}
	return release, nil
}

func exportOutputWorkingBytes(request ExportRequest, source ExportSource) (uint64, error) {
	bytes := EstimateExportWorkingBytes(request)
	if request.Mode == ExportModeSource {
		bytes = EstimateSourceExportBytes(source)
		if bytes > ^uint64(0)-(1<<20) {
			return 0, errors.New("原画导出空间估算溢出")
		}
		bytes += 1 << 20
	}
	return bytes, nil
}

func (e *Exporter) watchSystemSpace(ctx context.Context, cancel context.CancelCauseFunc) func() {
	watchCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
				if err := e.config.Cache.CheckSpace(watchCtx); err != nil {
					e.logger.Warn("系统盘媒体空间保护停止导出", "error", err)
					cancel(err)
					return
				}
			}
		}
	}()
	return func() { stop(); <-done }
}

func exportContextError(ctx context.Context, err error) error {
	if err != nil && errors.Is(context.Cause(ctx), recordingcache.ErrSpace) {
		return context.Cause(ctx)
	}
	return err
}

func (e *Exporter) publishOutput(ctx context.Context, task ExportTask) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := e.tempPathForTask(task)
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("打开待发布成品: %w", err)
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return fmt.Errorf("同步导出成品: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("关闭导出成品: %w", closeErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(path, e.outputPathForTask(task)); err != nil {
		return fmt.Errorf("发布导出成品: %w", err)
	}
	if err := syncExportDirectory(e.config.OutputDir); err != nil {
		return fmt.Errorf("同步导出成品目录: %w", err)
	}
	return nil
}
