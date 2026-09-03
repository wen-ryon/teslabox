// Package mediabundle 安装随正式程序签名发布的 Rockchip 媒体运行时。
package mediabundle

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultParent = "/opt/teslabox"
	mediaPrefix   = "opt/teslabox/media/"
	markerName    = ".teslabox-bundle-sha256"
	fsckVersion   = "1.2.0"
	maximumFiles  = 4096
	maximumBytes  = 512 * 1024 * 1024
)

// EnsureEmbedded 安装正式版本内置的媒体运行时；开发构建没有内置包时直接返回。
func EnsureEmbedded(ctx context.Context, logger *slog.Logger) error {
	archive, expectedHash, available, err := openEmbeddedArchive()
	if err != nil || !available {
		return err
	}
	defer archive.Close()
	if err := install(ctx, archive, expectedHash, defaultParent); err != nil {
		return err
	}
	if logger != nil {
		logger.Info("媒体运行时已通过完整性与编码能力检查", "sha256", expectedHash)
	}
	return nil
}

func install(ctx context.Context, archive io.Reader, expectedHash, parent string) error {
	expectedHash = strings.TrimSpace(strings.ToLower(expectedHash))
	if len(expectedHash) != sha256.Size*2 {
		return errors.New("媒体运行时摘要长度无效")
	}
	if _, err := hex.DecodeString(expectedHash); err != nil {
		return errors.New("媒体运行时摘要格式无效")
	}
	if !filepath.IsAbs(parent) || filepath.Clean(parent) == string(filepath.Separator) {
		return errors.New("媒体运行时安装目录无效")
	}
	target := filepath.Join(parent, "media")
	marker := filepath.Join(target, markerName)
	if content, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(content)) == expectedHash {
		return validateRuntime(ctx, target)
	}

	staging, err := os.MkdirTemp(parent, ".media.embedded-")
	if err != nil {
		return fmt.Errorf("创建媒体暂存目录: %w", err)
	}
	defer os.RemoveAll(staging)

	hash := sha256.New()
	if err := extractArchive(io.TeeReader(archive, hash), staging); err != nil {
		return err
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); actual != expectedHash {
		return errors.New("内置媒体运行时 SHA-256 校验失败")
	}
	if err := os.WriteFile(filepath.Join(staging, markerName), []byte(expectedHash+"\n"), 0o644); err != nil {
		return fmt.Errorf("写入媒体版本标记: %w", err)
	}
	if err := validateRuntime(ctx, staging); err != nil {
		return fmt.Errorf("验证新版媒体运行时: %w", err)
	}

	backup := filepath.Join(parent, ".media.previous")
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("清理旧媒体备份: %w", err)
	}
	if _, err := os.Lstat(target); err == nil {
		if err := os.Rename(target, backup); err != nil {
			return fmt.Errorf("备份当前媒体运行时: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("检查当前媒体运行时: %w", err)
	}
	if err := os.Rename(staging, target); err != nil {
		if _, backupErr := os.Lstat(backup); backupErr == nil {
			_ = os.Rename(backup, target)
		}
		return fmt.Errorf("启用新版媒体运行时: %w", err)
	}
	if err := validateRuntime(ctx, target); err != nil {
		_ = os.RemoveAll(target)
		if restoreErr := os.Rename(backup, target); restoreErr != nil {
			return errors.Join(fmt.Errorf("新版媒体运行时启用后验证失败: %w", err), fmt.Errorf("恢复旧媒体运行时: %w", restoreErr))
		}
		return fmt.Errorf("新版媒体运行时启用后验证失败并已恢复: %w", err)
	}
	_ = os.RemoveAll(backup)
	return nil
}

func extractArchive(source io.Reader, staging string) error {
	gzipReader, err := gzip.NewReader(source)
	if err != nil {
		return fmt.Errorf("打开媒体压缩包: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	files := 0
	var total int64
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("读取媒体压缩包: %w", err)
		}
		name := filepath.ToSlash(filepath.Clean(header.Name))
		name = strings.TrimPrefix(name, "./")
		if name == "opt" || name == "opt/teslabox" || name == "opt/teslabox/media" {
			if header.Typeflag != tar.TypeDir {
				return fmt.Errorf("媒体压缩包父路径类型无效 %q", header.Name)
			}
			continue
		}
		if !strings.HasPrefix(name, mediaPrefix) {
			return fmt.Errorf("媒体压缩包包含越界路径 %q", header.Name)
		}
		relative := strings.TrimPrefix(name, mediaPrefix)
		if relative == "" || relative == "." || strings.HasPrefix(relative, "../") || filepath.IsAbs(relative) {
			return fmt.Errorf("媒体压缩包路径无效 %q", header.Name)
		}
		destination := filepath.Join(staging, filepath.FromSlash(relative))
		if !strings.HasPrefix(destination, staging+string(filepath.Separator)) {
			return fmt.Errorf("媒体压缩包路径逃逸 %q", header.Name)
		}
		files++
		if files > maximumFiles || header.Size < 0 || total > maximumBytes-header.Size {
			return errors.New("媒体压缩包超过安全大小限制")
		}
		total += header.Size
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(destination, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(header.Mode) & 0o755
			if mode == 0 {
				mode = 0o644
			}
			file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, tarReader, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			// 动态库使用同目录内的版本别名；只允许不含路径分隔符的相对目标。
			if header.Linkname == "" || header.Linkname == "." || filepath.Base(header.Linkname) != header.Linkname || strings.ContainsAny(header.Linkname, `/\\`) {
				return fmt.Errorf("媒体压缩包符号链接目标无效 %q", header.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(header.Linkname, destination); err != nil {
				return err
			}
		default:
			return fmt.Errorf("媒体压缩包包含不允许的文件类型 %q", header.Name)
		}
	}
	return nil
}

func validateRuntime(ctx context.Context, root string) error {
	ffmpeg := filepath.Join(root, "bin", "ffmpeg")
	ffprobe := filepath.Join(root, "bin", "ffprobe")
	fsckExFAT := filepath.Join(root, "bin", "teslabox-fsck.exfat")
	font := filepath.Join(root, "share", "fonts", "NotoSansCJK-Regular.ttc")
	for _, path := range []string{ffmpeg, ffprobe, fsckExFAT, font} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("缺少媒体运行时文件 %s", path)
		}
	}
	if info, err := os.Stat(fsckExFAT); err != nil || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("TeslaBox 专用 fsck.exfat 不可执行 %s", fsckExFAT)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	fsckVersionOutput, err := exec.CommandContext(probeCtx, fsckExFAT, "-V").CombinedOutput()
	fsckExitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			return fmt.Errorf("检查 TeslaBox 专用 fsck.exfat 版本: %w", err)
		}
		fsckExitCode = exitError.ExitCode()
	}
	if err := validateFsckVersionResult(string(fsckVersionOutput), fsckExitCode); err != nil {
		return err
	}
	checks := []struct {
		args     []string
		required []string
	}{
		{args: []string{"-hide_banner", "-decoders"}, required: []string{"h264_rkmpp", "hevc_rkmpp"}},
		{args: []string{"-hide_banner", "-encoders"}, required: []string{"h264_rkmpp", "hevc_rkmpp"}},
		{args: []string{"-hide_banner", "-filters"}, required: []string{"ass", "subtitles", "scale_rkrga", "overlay_rkrga", "vpp_rkrga"}},
	}
	for _, check := range checks {
		command := exec.CommandContext(probeCtx, ffmpeg, check.args...)
		command.Env = append(environmentWithout("LD_LIBRARY_PATH"), "LD_LIBRARY_PATH="+filepath.Join(root, "lib"))
		output, err := command.CombinedOutput()
		if err != nil {
			return fmt.Errorf("执行 FFmpeg 能力检查: %w", err)
		}
		for _, required := range check.required {
			if !strings.Contains(string(output), required) {
				return fmt.Errorf("FFmpeg 缺少必需能力 %s", required)
			}
		}
	}
	return nil
}

func validateFsckVersionResult(output string, exitCode int) error {
	if !strings.Contains(output, fsckVersion) {
		return fmt.Errorf("TeslaBox 专用 fsck.exfat 版本不匹配：需要 %s", fsckVersion)
	}
	// exfatprogs 1.2.0 的 fsck.exfat 在 -V 输出版本后按 fsck 约定返回语法状态 16。
	// 这里只接受成功或该已知状态，避免把真实执行故障误认为有效版本。
	if exitCode != 0 && exitCode != 16 {
		return fmt.Errorf("检查 TeslaBox 专用 fsck.exfat 版本失败：退出码 %d", exitCode)
	}
	return nil
}

func environmentWithout(name string) []string {
	prefix := name + "="
	environment := make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, prefix) {
			environment = append(environment, value)
		}
	}
	return environment
}
