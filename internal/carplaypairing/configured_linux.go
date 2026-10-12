//go:build linux

package carplaypairing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"

	"golang.org/x/sys/unix"
)

var binaryDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func configuredBinary(settingsPath string) (string, string, error) {
	file, err := os.OpenFile(settingsPath, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", "", err
	}
	if err := privateInfo(info, false); err != nil {
		return "", "", err
	}
	if info.Size() > 16384 {
		return "", "", errors.New("独立服务配置超过预算")
	}
	body, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil {
		return "", "", err
	}
	var config struct {
		Root   string `json:"root"`
		SHA256 string `json:"binary_sha256"`
	}
	if json.Unmarshal(body, &config) != nil || !filepath.IsAbs(config.Root) || filepath.Clean(config.Root) != config.Root || !binaryDigest.MatchString(config.SHA256) {
		return "", "", errors.New("独立服务配对配置不可用")
	}
	path := filepath.Join(config.Root, "teslabox-carplay")
	binary, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", "", err
	}
	defer binary.Close()
	meta, err := binary.Stat()
	if err != nil {
		return "", "", err
	}
	owner, ok := meta.Sys().(*syscall.Stat_t)
	if !ok || !meta.Mode().IsRegular() || meta.Mode().Perm()&0022 != 0 || meta.Mode().Perm()&0100 == 0 || owner.Uid != uint32(os.Geteuid()) || meta.Size() > 64<<20 {
		return "", "", errors.New("独立服务配对程序身份不可用")
	}
	var magic [4]byte
	if _, err := binary.ReadAt(magic[:], 0); err != nil || magic != [4]byte{0x7f, 'E', 'L', 'F'} {
		return "", "", errors.New("独立服务配对程序不是 ELF")
	}
	return path, config.SHA256, nil
}

// NewConfiguredProvisioningManager 延后检查服务能力，冷启动尚未就绪时不要求重启录像主程序。
func NewConfiguredProvisioningManager(settingsPath, directory, runtime string) (*Manager, error) {
	if !filepath.IsAbs(settingsPath) || !filepath.IsAbs(directory) || !filepath.IsAbs(runtime) {
		return nil, errors.New("首次登记配置必须使用本机绝对路径")
	}
	client := &serviceClient{runtime: runtime}
	pair := func(ctx context.Context, phone string, confirmation *Confirmation) error {
		if confirmation.incoming {
			return verifyIncomingPhone(ctx, phone)
		}
		binary, digest, err := configuredBinary(settingsPath)
		if err != nil {
			return err
		}
		runner := nativePairRunner(binary, directory, exec.CommandContext, digest)
		return runner(ctx, phone, confirmation)
	}
	m, err := NewManager(client.runner(pair))
	if err != nil {
		return nil, err
	}
	m.completedState = "video_verified"
	m.bindServiceRecovery(client)
	m.scanner = scanBlueZ
	m.incomingReady = func() bool {
		status, err := client.status()
		return err == nil && status.OnboardingReady && m.Ready()
	}
	m.ready = func() bool {
		if status, err := client.status(); err != nil || !status.ServiceActive && !status.OnboardingReady {
			return false
		}
		_, _, err := configuredBinary(settingsPath)
		return err == nil
	}
	return m, nil
}
