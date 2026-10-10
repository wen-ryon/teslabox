//go:build linux

package carplaypairing

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// LocalChannel 把每轮配对隔离到私有目录，退出只删除本轮 socket 和空目录。
type LocalChannel struct {
	*Confirmation
	path     string
	listener *net.UnixListener
	done     chan struct{}
}

// NewLocalChannel 只接受当前用户拥有的私有目录和同用户本机连接，不开放网络确认端口。
func NewLocalChannel(parent context.Context, directory string, window time.Duration) (*LocalChannel, error) {
	confirmation, err := NewConfirmation(parent, window)
	if err != nil {
		return nil, err
	}
	channel, err := listenConfirmation(directory, confirmation)
	if err != nil {
		confirmation.cancel()
	}
	return channel, err
}

func listenConfirmation(directory string, confirmation *Confirmation) (*LocalChannel, error) {
	if !filepath.IsAbs(directory) {
		return nil, errors.New("配对通道目录必须是绝对路径")
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("配对通道目录必须由当前用户私有持有")
	}
	// Go 临时目录后缀最多十位，Unix socket 还需留终止字节。
	if len(filepath.Join(directory, "pair-0000000000", "confirm.sock")) >= 108 {
		return nil, errors.New("配对通道目录过深，Unix socket 路径超过限制")
	}
	jobDir, err := os.MkdirTemp(directory, "pair-")
	if err != nil {
		confirmation.cancel()
		return nil, err
	}
	path := filepath.Join(jobDir, "confirm.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		confirmation.cancel()
		_ = os.Remove(jobDir)
		return nil, err
	}
	listener.SetUnlinkOnClose(true)
	if err := os.Chmod(path, 0600); err != nil {
		confirmation.cancel()
		_ = listener.Close()
		_ = os.Remove(jobDir)
		return nil, err
	}
	c := &LocalChannel{Confirmation: confirmation, path: path, listener: listener, done: make(chan struct{})}
	// 接收线程归此轮任务所有；上下文取消关闭 accept，Serve 负责关闭已接收连接。
	go c.accept()
	return c, nil
}

// SocketPath 仅供本机接收端参数使用，不包含配对数字。
func (c *LocalChannel) SocketPath() string { return c.path }

// Done 允许任务协调器等待清理完成后再创建下一轮，防止确认线程遗留。
func (c *LocalChannel) Done() <-chan struct{} { return c.done }

// Close 结束本轮本机通道，不向其他进程或蓝牙适配器发送任何命令。
func (c *LocalChannel) Close() {
	_ = c.Cancel(c.Status().RequestID)
	_ = c.listener.Close()
	<-c.done
}

func (c *LocalChannel) accept() {
	stop := context.AfterFunc(c.ctx, func() { _ = c.listener.Close() })
	defer func() {
		stop()
		_ = c.listener.Close()
		_ = os.Remove(filepath.Dir(c.path))
		close(c.done)
	}()
	for {
		conn, err := c.listener.AcceptUnix()
		if err != nil {
			c.mu.Lock()
			c.expireLocked()
			if c.status.State == "waiting_native" {
				c.status.State = "failed"
			}
			c.mu.Unlock()
			c.cancel()
			return
		}
		if err := sameUser(conn); err != nil {
			_ = conn.Close()
			continue
		}
		_ = c.Serve(conn)
		return
	}
}

func sameUser(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var credentials *unix.Ucred
	var credentialErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, credentialErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if credentialErr != nil {
		return credentialErr
	}
	if credentials == nil || credentials.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("配对通道不是同用户本机连接")
	}
	return nil
}
