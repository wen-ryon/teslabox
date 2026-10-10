//go:build linux

package carplaypairing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// NativePairRunner 使用实际 Rust 数字确认入口，只管理本轮配对进程，不控制录像或 AP。
func NativePairRunner(binary, directory string) (PairRunner, error) {
	if !filepath.IsAbs(binary) || !filepath.IsAbs(directory) {
		return nil, errors.New("配对程序和通道目录必须为绝对路径")
	}
	return nativePairRunner(binary, directory, exec.CommandContext), nil
}

func nativePairRunner(binary, directory string, command func(context.Context, string, ...string) *exec.Cmd, expectedDigest ...string) PairRunner {
	return func(ctx context.Context, phone string, confirmation *Confirmation) error {
		if !phoneAddress.MatchString(phone) || confirmation == nil || ctx.Err() != nil {
			return errors.New("本机配对任务参数无效或已取消")
		}
		deadline, ok := ctx.Deadline()
		remaining := time.Until(deadline)
		if !ok || remaining <= 0 || remaining > 120*time.Second {
			return errors.New("本机配对任务需要明确的短测截止时间")
		}
		// 持有已验证的 ELF 文件描述符，避免路径验证后被替换为其他程序。
		file, err := os.OpenFile(binary, os.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return errors.New("本机配对程序不可用")
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return errors.New("本机配对程序身份不可用")
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		var magic [4]byte
		if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Mode().Perm()&0100 == 0 ||
			owner.Uid != uint32(os.Geteuid()) {
			return errors.New("本机配对程序必须由当前用户独占写入")
		}
		if _, err := file.ReadAt(magic[:], 0); err != nil || !bytes.Equal(magic[:], []byte{0x7f, 'E', 'L', 'F'}) {
			return errors.New("本机配对程序必须是已部署的 ELF")
		}
		if len(expectedDigest) != 0 {
			if len(expectedDigest) != 1 || !binaryDigest.MatchString(expectedDigest[0]) {
				return errors.New("配对程序期望摘要不合法")
			}
			digest := sha256.New()
			if _, err := io.Copy(digest, file); err != nil || hex.EncodeToString(digest.Sum(nil)) != expectedDigest[0] {
				return errors.New("配对程序摘要改变，不能开始配对")
			}
		}
		channel, err := listenConfirmation(directory, confirmation)
		if err != nil {
			return err
		}
		defer channel.Close()
		seconds := int((remaining + time.Second - 1) / time.Second)
		cmd := command(ctx, "/proc/self/fd/3", "bluetooth-pair-ui", "--adapter", "hci0", "--peer", phone,
			"--timeout-secs", strconv.Itoa(seconds), "--confirmation-socket", channel.SocketPath(),
			"--request-id", confirmation.Status().RequestID)
		cmd.ExtraFiles = []*os.File{file}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, io.Discard, io.Discard
		// SIGTERM 让 Rust 先取消自有 Pair 请求和 Agent，超出清理预算才由 Go 杀进程。
		cmd.Cancel = func() error {
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		cmd.WaitDelay = 10 * time.Second
		if err := cmd.Run(); err != nil {
			return errors.New("本机蓝牙配对未完成")
		}
		// 原生入口在退出成功前重新查询 BlueZ Paired；成功不代表 CarPlay 已登记。
		return ctx.Err()
	}
}
