package updater

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// HelperConfig 固定安装助手允许访问的目录、目标程序和健康检查地址。
type HelperConfig struct {
	RequestPath string
	ResultPath  string
	DownloadDir string
	BackupDir   string
	TargetPath  string
	Service     string
	HealthURL   string
	Timeout     time.Duration
}

const latestBackupName = "teslabox.previous.backup"

// RunHelper 重新验证本地发布物后原子替换程序，并在健康检查失败时自动回滚。
func RunHelper(ctx context.Context, config HelperConfig) error {
	request, err := readInstallRequest(config.RequestPath)
	if err != nil {
		return err
	}
	if config.Timeout <= 0 {
		config.Timeout = 45 * time.Second
	}
	packagePath, err := trustedPackagePath(config.DownloadDir, request.PackageName)
	if err != nil {
		return finishInstall(config.ResultPath, request.Version, "failed", err)
	}
	if err := verifyLocalPackage(packagePath, request); err != nil {
		return finishInstall(config.ResultPath, request.Version, "failed", err)
	}
	if err := os.MkdirAll(config.BackupDir, 0o700); err != nil {
		return finishInstall(config.ResultPath, request.Version, "failed", fmt.Errorf("创建备份目录: %w", err))
	}
	backupPath := filepath.Join(config.BackupDir, latestBackupName)
	if err := copyFileAtomic(config.TargetPath, backupPath, 0o700); err != nil {
		return finishInstall(config.ResultPath, request.Version, "failed", fmt.Errorf("备份当前程序: %w", err))
	}
	if err := cleanupVersionBackups(config.BackupDir); err != nil {
		return finishInstall(config.ResultPath, request.Version, "failed", fmt.Errorf("清理过期更新备份: %w", err))
	}
	if err := copyFileAtomic(packagePath, config.TargetPath, 0o755); err != nil {
		return finishInstall(config.ResultPath, request.Version, "failed", fmt.Errorf("安装新程序: %w", err))
	}
	installCtx, cancel := context.WithTimeout(ctx, config.Timeout)
	defer cancel()
	if err := restartAndWaitHealthy(installCtx, config.Service, config.HealthURL); err != nil {
		rollbackErr := copyFileAtomic(backupPath, config.TargetPath, 0o755)
		if rollbackErr == nil {
			rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), config.Timeout)
			rollbackErr = restartAndWaitHealthy(rollbackCtx, config.Service, config.HealthURL)
			rollbackCancel()
		}
		message := fmt.Errorf("新版健康检查失败并已回滚: %w", err)
		if rollbackErr != nil {
			message = errors.Join(message, fmt.Errorf("回滚失败: %w", rollbackErr))
		}
		return finishInstall(config.ResultPath, request.Version, "failed", message)
	}
	if err := finishInstall(config.ResultPath, request.Version, "success", nil); err != nil {
		return err
	}
	if !request.RebootAfterInstall {
		return nil
	}
	rebootCtx, rebootCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer rebootCancel()
	if err := exec.CommandContext(rebootCtx, "systemctl", "reboot").Run(); err != nil {
		return fmt.Errorf("提交更新后的自动重启: %w", err)
	}
	return nil
}

func readInstallRequest(path string) (InstallRequest, error) {
	file, err := os.Open(path)
	if err != nil {
		return InstallRequest{}, fmt.Errorf("读取安装请求: %w", err)
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, 16*1024+1))
	if err != nil {
		return InstallRequest{}, fmt.Errorf("读取安装请求: %w", err)
	}
	if len(content) > 16*1024 {
		return InstallRequest{}, errors.New("安装请求超过大小限制")
	}
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	decoder.DisallowUnknownFields()
	var request InstallRequest
	if err := decoder.Decode(&request); err != nil {
		return InstallRequest{}, fmt.Errorf("解析安装请求: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return InstallRequest{}, errors.New("安装请求包含多余内容")
	}
	if _, err := parseReleaseVersion(request.Version); err != nil {
		return InstallRequest{}, errors.New("安装版本号无效")
	}
	if request.PackageName == "" || filepath.Base(request.PackageName) != request.PackageName || strings.ContainsAny(request.PackageName, `/\\`) {
		return InstallRequest{}, errors.New("安装包文件名无效")
	}
	if len(request.SHA256) != sha256.Size*2 || request.SHA256 != strings.ToLower(request.SHA256) {
		return InstallRequest{}, errors.New("安装包摘要无效")
	}
	if _, err := hex.DecodeString(request.SHA256); err != nil || request.Size <= 0 {
		return InstallRequest{}, errors.New("安装包校验信息无效")
	}
	return request, nil
}

func trustedPackagePath(root, name string) (string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) {
		return "", errors.New("更新目录无效")
	}
	path := filepath.Join(root, name)
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("读取更新包: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("更新包必须是普通文件")
	}
	return path, nil
}

func verifyLocalPackage(path string, request InstallRequest) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != request.Size {
		return errors.New("安装包长度校验失败")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return fmt.Errorf("计算安装包摘要: %w", err)
	}
	if hex.EncodeToString(hash.Sum(nil)) != request.SHA256 {
		return errors.New("安装包 SHA-256 校验失败")
	}
	elfFile, err := elf.Open(path)
	if err != nil {
		return errors.New("安装包不是有效的 Linux ELF 程序")
	}
	defer elfFile.Close()
	if elfFile.Machine != elf.EM_AARCH64 || (elfFile.Type != elf.ET_EXEC && elfFile.Type != elf.ET_DYN) {
		return errors.New("安装包不是可执行的 ARM64 程序")
	}
	return nil
}

func copyFileAtomic(source, target string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".teslabox-install-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, input); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, target)
}

func cleanupVersionBackups(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == latestBackupName || !strings.HasPrefix(name, "teslabox-") || !strings.HasSuffix(name, ".backup") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := os.Remove(filepath.Join(directory, name)); err != nil {
			return err
		}
	}
	return nil
}

func restartAndWaitHealthy(ctx context.Context, service, healthURL string) error {
	if service != "teslabox.service" {
		return errors.New("不允许重启未知服务")
	}
	if healthURL != "http://127.0.0.1/healthz" {
		return errors.New("健康检查地址无效")
	}
	if output, err := exec.CommandContext(ctx, "systemctl", "restart", service).CombinedOutput(); err != nil {
		return fmt.Errorf("重启服务: %w: %s", err, strings.TrimSpace(string(output)))
	}
	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err == nil {
			response, requestErr := client.Do(request)
			if requestErr == nil {
				response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("等待新版健康检查超时")
		case <-ticker.C:
		}
	}
}

func finishInstall(path, version, status string, cause error) error {
	result := InstallResult{Version: version, Status: status, UpdatedAt: time.Now().Format(time.RFC3339)}
	if cause != nil {
		result.Message = cause.Error()
	}
	if err := writeJSONAtomic(path, result, 0o600); err != nil {
		return errors.Join(cause, fmt.Errorf("保存安装结果: %w", err))
	}
	return cause
}
