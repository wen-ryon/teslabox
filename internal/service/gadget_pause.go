package service

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	gadgetGuardPaused       = "paused"
	gadgetGuardAttempt      = "attempt:"
	gadgetGuardService      = "service:"
	gadgetGuardResumeAfter  = "resume-after:"
	defaultUntrackedBootID  = "untracked-boot"
	maximumGadgetBootIDSize = 128
)

type gadgetGuardState struct {
	exists bool
	mode   string
	bootID string
}

func readBootID(path string) (string, error) {
	if path == "" {
		return defaultUntrackedBootID, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取系统 boot_id: %w", err)
	}
	bootID := strings.TrimSpace(string(content))
	if bootID == "" || len(bootID) > maximumGadgetBootIDSize {
		return "", errors.New("系统 boot_id 为空或长度异常")
	}
	for _, value := range bootID {
		if (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') ||
			(value >= '0' && value <= '9') || value == '-' || value == '_' || value == '.' {
			continue
		}
		return "", errors.New("系统 boot_id 包含非法字符")
	}
	return bootID, nil
}

func readBootUptime(path string) (time.Duration, bool, error) {
	if path == "" {
		return 0, false, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, true, fmt.Errorf("读取系统启动时长: %w", err)
	}
	fields := strings.Fields(string(content))
	if len(fields) == 0 {
		return 0, true, errors.New("系统启动时长为空")
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	maximumSeconds := float64(math.MaxInt64) / float64(time.Second)
	if err != nil || seconds < 0 || seconds > maximumSeconds || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0, true, fmt.Errorf("系统启动时长不合法: %q", fields[0])
	}
	return time.Duration(seconds * float64(time.Second)), true, nil
}

func loadGadgetGuardState(path string) (gadgetGuardState, error) {
	if path == "" {
		return gadgetGuardState{}, nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return gadgetGuardState{}, nil
	}
	if err != nil {
		return gadgetGuardState{}, fmt.Errorf("读取 USB Gadget 启动保护状态: %w", err)
	}
	if !info.Mode().IsRegular() {
		return gadgetGuardState{}, errors.New("USB Gadget 启动保护状态不是普通文件")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return gadgetGuardState{}, fmt.Errorf("读取 USB Gadget 启动保护内容: %w", err)
	}
	value := strings.TrimSpace(string(content))
	// 兼容旧版本创建的空暂停文件；未知或损坏状态也必须按暂停处理。
	if value == "" || value == gadgetGuardPaused {
		return gadgetGuardState{exists: true, mode: gadgetGuardPaused}, nil
	}
	for _, prefix := range []string{gadgetGuardAttempt, gadgetGuardService, gadgetGuardResumeAfter} {
		if bootID, found := strings.CutPrefix(value, prefix); found {
			if _, err := validateGuardBootID(bootID); err != nil {
				return gadgetGuardState{}, err
			}
			return gadgetGuardState{exists: true, mode: prefix, bootID: bootID}, nil
		}
	}
	return gadgetGuardState{}, fmt.Errorf("USB Gadget 启动保护状态不合法: %q", value)
}

func validateGuardBootID(bootID string) (string, error) {
	if bootID == "" || len(bootID) > maximumGadgetBootIDSize {
		return "", errors.New("USB Gadget 启动保护中的 boot_id 为空或长度异常")
	}
	for _, value := range bootID {
		if (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') ||
			(value >= '0' && value <= '9') || value == '-' || value == '_' || value == '.' {
			continue
		}
		return "", errors.New("USB Gadget 启动保护中的 boot_id 包含非法字符")
	}
	return bootID, nil
}

func persistGadgetGuardState(path, value string) error {
	if path == "" {
		return nil
	}
	if value != gadgetGuardPaused {
		prefixValid := false
		for _, prefix := range []string{gadgetGuardAttempt, gadgetGuardService, gadgetGuardResumeAfter} {
			if bootID, found := strings.CutPrefix(value, prefix); found {
				if _, err := validateGuardBootID(bootID); err != nil {
					return err
				}
				prefixValid = true
				break
			}
		}
		if !prefixValid {
			return errors.New("拒绝保存未知的 USB Gadget 启动保护状态")
		}
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("创建 USB Gadget 启动保护目录: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".gadget-guard-*")
	if err != nil {
		return fmt.Errorf("创建 USB Gadget 启动保护临时文件: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("设置 USB Gadget 启动保护权限: %w", err)
	}
	if _, err := temporary.WriteString(value + "\n"); err != nil {
		temporary.Close()
		return fmt.Errorf("写入 USB Gadget 启动保护状态: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("同步 USB Gadget 启动保护状态: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭 USB Gadget 启动保护状态: %w", err)
	}
	if err := replaceFile(temporaryName, path); err != nil {
		return fmt.Errorf("保存 USB Gadget 启动保护状态: %w", err)
	}
	return nil
}

func replaceFile(source, destination string) error {
	renameErr := os.Rename(source, destination)
	if renameErr == nil {
		return nil
	}
	if runtime.GOOS != "windows" {
		// Linux 原子替换失败时保留旧保护状态，不能为了重试而制造无保护窗口。
		return renameErr
	}
	if removeErr := os.Remove(destination); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return removeErr
	}
	return os.Rename(source, destination)
}
