//go:build linux

package networkroles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// RunDefault 在独立 CLI 中执行，启动顺序不依赖主服务或其他进程内状态。
func RunDefault(ctx context.Context, action string, out io.Writer) error {
	if action == "env" {
		pair, err := Detect("/sys")
		if err != nil {
			return err
		}
		if pair != nil {
			_, err = fmt.Fprintf(out, "TESLABOX_AP_INTERFACE=%s\nTESLABOX_WIFI_INTERFACE=%s\n", pair.AP, pair.Station)
		}
		return err
	}
	if action != "prepare" {
		return errors.New("网络角色操作无效")
	}
	if os.Geteuid() != 0 {
		return errors.New("网络角色准备需要 root")
	}
	// 私有 root 目录避免 /run/lock 可写目录中的预置文件或符号链接替换锁身份。
	lockDir := "/run/teslabox-network-roles"
	if err := os.Mkdir(lockDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(lockDir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ok || stat.Uid != 0 {
		return errors.New("网络角色锁目录权限异常")
	}
	lock, err := os.OpenFile(filepath.Join(lockDir, "prepare.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("网络角色准备正在执行")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	// activating 也不能修改；不能把 is-active 的非零返回笼统当成已停止。
	state, queryErr := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", "--property=ActiveState", "--value", "NetworkManager.service").Output()
	if !managerStopped(string(state), queryErr) {
		return errors.New("NetworkManager 未确认停止，拒绝修改活动网络；请在维护窗口或下一次冷启动执行")
	}
	// 防止有人绕过 systemd 手动启动 NM，此时不能因为 unit=inactive 就改连接文件。
	running, err := networkManagerProcessRunning("/proc")
	if err != nil || running {
		return errors.New("检测到网络管理进程或无法确认进程状态，未修改连接配置")
	}
	return Prepare(DefaultPaths())
}
