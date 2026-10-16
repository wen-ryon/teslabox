package recordingcache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// ErrSourceChanged 区分输入变化与系统盘、权限错误，不以通用失败推断录像已删除。
var ErrSourceChanged = errors.New("录像源在准备期间发生变化")

// SourceChangeError 保留拒绝本次输入的具体证据，供任务日志定位偶发校验失败。
type SourceChangeError struct {
	Path, Version, Stage                                         string
	IdentityChanged, SizeChanged, ModTimeChanged, ContentChanged bool
	BeforeSize, AfterSize                                        int64
	BeforeModTime, AfterModTime                                  int64
	BeforeIdentity, AfterIdentity                                string
	ExpectedDigest, ActualDigest                                 string
}

// Error 仅包含本地录像校验证据，不包含上传目标或凭据。
func (e *SourceChangeError) Error() string {
	return fmt.Sprintf("%s，请重新选择: stage=%s snapshot=%s path=%q identity_changed=%t size_changed=%t mtime_changed=%t content_changed=%t before={size:%d,mtime_ns:%d,%s} after={size:%d,mtime_ns:%d,%s} expected_sha256=%s actual_sha256=%s", ErrSourceChanged, e.Stage, e.Version, e.Path, e.IdentityChanged, e.SizeChanged, e.ModTimeChanged, e.ContentChanged, e.BeforeSize, e.BeforeModTime, e.BeforeIdentity, e.AfterSize, e.AfterModTime, e.AfterIdentity, e.ExpectedDigest, e.ActualDigest)
}

// Unwrap 保持调用方能够按输入变化分类，而不依赖中文错误字符串。
func (e *SourceChangeError) Unwrap() error { return ErrSourceChanged }

func sourceChange(path string, before, after fs.FileInfo) *SourceChangeError {
	return &SourceChangeError{Path: path, IdentityChanged: !os.SameFile(before, after), SizeChanged: before.Size() != after.Size(), ModTimeChanged: !before.ModTime().Equal(after.ModTime()), BeforeSize: before.Size(), AfterSize: after.Size(), BeforeModTime: before.ModTime().UnixNano(), AfterModTime: after.ModTime().UnixNano(), BeforeIdentity: sourceFileIdentity(before), AfterIdentity: sourceFileIdentity(after)}
}

func sourceStage(file File, stage string, err error) error {
	if err == nil {
		return nil
	}
	if changed, ok := errors.AsType[*SourceChangeError](err); ok {
		copy := *changed
		copy.Version = file.Version
		if copy.Stage == "" {
			copy.Stage = stage
		}
		return &copy
	}
	return fmt.Errorf("录像暂存 stage=%s snapshot=%s path=%q: %w", stage, file.Version, file.Path, err)
}
