package updater

import (
	"bytes"
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
	"sync"
	"time"
)

// HelperConfig 固定安装助手允许访问的目录、目标程序和健康检查地址。
type HelperConfig struct {
	RequestPath     string
	ResultPath      string
	DownloadDir     string
	BackupDir       string
	TargetPath      string
	ExportStatePath string
	Service         string
	HealthURL       string
	Timeout         time.Duration
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
	if err := plainPath(config.BackupDir); err != nil {
		return finishInstall(config.ResultPath, request.Version, "failed", err)
	}
	if err := durableMkdirAll(config.BackupDir, 0o700); err != nil {
		return finishInstall(config.ResultPath, request.Version, "failed", fmt.Errorf("创建备份目录: %w", err))
	}
	unlock, err := lockInstallation(config.BackupDir)
	if err != nil {
		return finishInstall(config.ResultPath, request.Version, "failed", err)
	}
	release := sync.OnceFunc(unlock)
	defer release()
	ops := serviceOperations(config)
	ops.resumeUnchanged = func(ctx context.Context) error {
		release()
		return restartAndWaitHealthy(ctx, config.Service, config.HealthURL, config.TargetPath, false)
	}
	if err := transactionalInstall(ctx, config, request, packagePath, ops); err != nil {
		return finishInstall(config.ResultPath, request.Version, "failed", err)
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
	if info.Size() != request.Size || request.Size > 128<<20 || request.Size <= 0 {
		return errors.New("安装包长度校验失败")
	}
	data, err := io.ReadAll(io.LimitReader(file, (128<<20)+1))
	if err != nil {
		return err
	}
	hash := sha256.Sum256(data)
	if int64(len(data)) != request.Size || hex.EncodeToString(hash[:]) != request.SHA256 {
		return errors.New("安装包 SHA-256 校验失败")
	}
	// 摘要、ELF 类型和只读能力声明必须来自同一份已认证字节，不能重新按路径打开。
	elfFile, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return errors.New("安装包不是有效的 Linux ELF 程序")
	}
	defer elfFile.Close()
	if elfFile.Machine != elf.EM_AARCH64 || (elfFile.Type != elf.ET_EXEC && elfFile.Type != elf.ET_DYN) {
		return errors.New("安装包不是可执行的 ARM64 程序")
	}
	rodata := elfFile.Section(".rodata")
	if rodata == nil || rodata.Size > 128<<20 || rodata.Type != elf.SHT_PROGBITS || rodata.Flags&elf.SHF_ALLOC == 0 || rodata.Flags&elf.SHF_WRITE != 0 {
		return errors.New("目标程序缺少可信的更新保护声明，替换前拒绝安装")
	}
	declaration, err := rodata.Data()
	if err != nil {
		return err
	}
	if bytes.Count(declaration, []byte(ProgramTransactionCapability)) != 1 {
		return errors.New("目标程序不支持状态回滚保护，替换前拒绝安装")
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

func restartAndWaitHealthy(ctx context.Context, service, healthURL, target string, requireProcessHeader bool) error {
	if service != "teslabox.service" {
		return errors.New("不允许重启未知服务")
	}
	if healthURL != "http://127.0.0.1/healthz" {
		return errors.New("健康检查地址无效")
	}
	if output, err := exec.CommandContext(ctx, "systemctl", "start", service).CombinedOutput(); err != nil {
		return fmt.Errorf("重启服务: %w: %s", err, strings.TrimSpace(string(output)))
	}
	pid, err := verifyRunningProgram(ctx, service, target)
	if err != nil {
		return err
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
				if response.StatusCode == http.StatusOK && (!requireProcessHeader || response.Header.Get("X-TeslaBox-Process") == pid) {
					actual, err := verifyRunningProgram(ctx, service, target)
					if err != nil {
						return err
					}
					if actual != pid {
						return errors.New("健康检查期间主进程已变化")
					}
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

func verifyRunningProgram(ctx context.Context, service, target string) (string, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", service, "--property=MainPID", "--value").Output()
	if err != nil {
		return "", err
	}
	pid := strings.TrimSpace(string(out))
	if pid == "" || pid == "0" {
		return "", errors.New("服务没有主进程")
	}
	for _, c := range pid {
		if c < '0' || c > '9' {
			return "", errors.New("服务进程标识无效")
		}
	}
	executable, err := os.Stat(filepath.Join("/proc", pid, "exe"))
	if err != nil {
		return "", err
	}
	installed, err := os.Lstat(target)
	if err != nil {
		return "", err
	}
	if !installed.Mode().IsRegular() || !os.SameFile(executable, installed) {
		return "", errors.New("运行进程不是本次安装的程序")
	}
	return pid, nil
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
