package carplaybundle

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	buildversion "github.com/wen-ryon/teslabox/internal/version"
)

//go:embed runtime_recovery.py
var recoverySource []byte

//go:embed runtime_install.py
var installerSource []byte

//go:embed teslabox-carplay-experiment.service.in
var unitTemplate []byte

//go:embed runtime_preflight.py
var preflightSource string

// EnsureEmbedded 完成本地运行时与恢复钩子安装，失败不能被主程序健康检查掩盖。
func EnsureEmbedded(ctx context.Context, logger *slog.Logger) error {
	archive, expected, available, err := openEmbeddedArchive()
	if err != nil || !available {
		return err
	}
	defer archive.Close()
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" || os.Geteuid() != 0 {
		return errors.New("产品 CarPlay 安装仅支持 root ARM64 Linux")
	}
	version, err := stageArchive(archive, expected, "/opt/teslabox/carplay/versions")
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(version, "PACKAGE.json"))
	if err != nil {
		return err
	}
	info, _, err := validateManifest(data)
	if err != nil {
		return err
	}
	if info.Version != buildversion.Version {
		return errors.New("CarPlay 内层版本与主程序发行版本不一致")
	}
	// 所有预检在服务配置发生变化前完成，不把依赖缺失留到录像服务重启后。
	preflightCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := preflightRuntime(preflightCtx, version); err != nil {
		return err
	}
	for _, tool := range []string{"systemctl", "busctl", "teslaboxctl"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("缺少 CarPlay 系统依赖 %s: %w", tool, err)
		}
	}
	return activateRuntime(ctx, logger, version, expected)
}

func preflightRuntime(ctx context.Context, version string) error {
	for _, arguments := range [][]string{
		{filepath.Join(version, "teslabox-carplay"), "--help"},
		{filepath.Join(version, "runtime/bin/node"), "--version"},
		{"/usr/bin/python3", "-B", "-c", "import fcntl, socket, subprocess, hashlib, json; from pathlib import Path"},
		{"/usr/bin/python3", "-B", "-c", preflightSource, version},
		{filepath.Join(version, "runtime/bin/node"), "--input-type=module", "-e",
			"import {pathToFileURL} from 'node:url'; const root=process.argv[1]; const loader=await import(pathToFileURL(root+'/experiments/carplay-native/reference-livi/pinned-auth-loader.mjs')); await loader.loadPinnedStack(); await loader.loadPinnedEventCodecs(); await loader.loadPinnedPairSetup(); await loader.loadPinnedPairVerify(); (await import(pathToFileURL(root+'/experiments/carplay-native/reference-livi/oem-branding.mjs'))).teslaBoxBranding();", version},
	} {
		if err := exec.CommandContext(ctx, arguments[0], arguments[1:]...).Run(); err != nil {
			return fmt.Errorf("CarPlay 本地依赖预检失败: %w", err)
		}
	}
	return nil
}

func activateRuntime(ctx context.Context, logger *slog.Logger, version, expected string) error {
	parent := "/usr/local/libexec"
	if err := checkParents(parent); err != nil {
		return err
	}
	for name, source := range map[string][]byte{
		"teslabox-carplay-runtime-recovery.py":   recoverySource,
		"teslabox-carplay-runtime-install.py":    installerSource,
		"teslabox-carplay-experiment.service.in": unitTemplate,
	} {
		if err := atomicFile(filepath.Join(parent, name), source, 0644); err != nil {
			return err
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	file, err := os.Open(executable)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	installCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	command := exec.CommandContext(installCtx, "/usr/bin/python3", "-B", filepath.Join(parent, "teslabox-carplay-runtime-install.py"),
		"--runtime", version, "--main-sha256", hex.EncodeToString(hash.Sum(nil)), "--runtime-sha256", expected)
	// 错误正文可能包含私有配置，只暴露固定安装阶段和进程退出状态。
	if err := command.Run(); err != nil {
		return fmt.Errorf("CarPlay 激活事务失败，启动钩子将恢复旧组件: %w", err)
	}
	if logger != nil {
		logger.Info("CarPlay 公开运行时已通过本地安装检查", "sha256", expected)
	}
	installedRuntime = version
	return nil
}

func checkParents(target string) error {
	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return errors.New("CarPlay 安装路径不合法")
	}
	for current := target; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return errors.New("CarPlay 安装目录不能包含链接")
		}
		if err == nil {
			if err := trustedParent(info, current == "/tmp" && current != target); err != nil {
				return err
			}
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return os.MkdirAll(target, 0700)
}

func atomicFile(target string, data []byte, mode os.FileMode) error {
	if info, err := os.Lstat(target); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("CarPlay 安装文件不能包含链接")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".carplay-tool-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), target); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(target))
}

func syncDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func stageArchive(source io.Reader, expected, parent string) (string, error) {
	if !digestPattern.MatchString(expected) {
		return "", errors.New("CarPlay 归档摘要不合法")
	}
	if err := checkParents(parent); err != nil {
		return "", err
	}
	archive, err := os.CreateTemp(parent, ".archive-")
	if err != nil {
		return "", err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(source, maxBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxBytes || hex.EncodeToString(hash.Sum(nil)) != expected {
		return "", errors.New("CarPlay 内置归档摘要或大小不符")
	}
	target := filepath.Join(parent, expected)
	if info, err := os.Lstat(target); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("CarPlay 版本目录不合法")
		}
		if err := checkParents(target); err != nil {
			return "", err
		}
		if _, err := verifyDirectory(target); err != nil {
			return "", err
		}
		if _, err := archive.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
		if err := verifyManifestBinding(archive, target); err != nil {
			return "", err
		}
		return target, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	staging, err := os.MkdirTemp(parent, ".runtime-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	if _, err := extractArchive(archive, staging); err != nil {
		return "", err
	}
	if _, err := verifyDirectory(staging); err != nil {
		return "", err
	}
	// 子目录也同步，保证版本重命名后归档成员不会在掉电时丢失。
	if err := filepath.WalkDir(staging, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return syncDirectory(path)
		}
		return nil
	}); err != nil {
		return "", err
	}
	if err := os.Rename(staging, target); err != nil {
		return "", err
	}
	if err := syncDirectory(parent); err != nil {
		return "", err
	}
	return target, nil
}
