//go:build linux

package carplaypairing

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func privateDirectory(t *testing.T) string {
	t.Helper()
	// 使用短路径，避免 Unix socket 路径长度影响权限和生命周期验证。
	directory, err := os.MkdirTemp("", "tbcp-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(directory) })
	return directory
}

func TestLocalChannelExchangesActualUnixStreamAndRemovesOnlyOwnDirectory(t *testing.T) {
	directory := privateDirectory(t)
	protected := filepath.Join(directory, "unrelated")
	if err := os.WriteFile(protected, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(protected) })
	c, err := NewLocalChannel(context.Background(), directory, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	info, err := os.Lstat(c.SocketPath())
	if err != nil || info.Mode().Perm() != 0600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatal("实际 socket 权限不是私有")
	}
	conn, err := net.Dial("unix", c.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	id := c.Status().RequestID
	if err := json.NewEncoder(conn).Encode(map[string]any{"request_id": id, "passkey": 42}); err != nil {
		t.Fatal(err)
	}
	waitState(t, c.Confirmation, "waiting_confirmation")
	if err := c.Confirm(id, "000042"); err != nil {
		t.Fatal(err)
	}
	var reply map[string]string
	if err := json.NewDecoder(conn).Decode(&reply); err != nil || reply["digits"] != "000042" {
		t.Fatal("实际 Unix 数字确认失败")
	}
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("Unix 接收线程未退出")
	}
	if _, err := os.Lstat(filepath.Dir(c.SocketPath())); !os.IsNotExist(err) {
		t.Fatal("本轮目录未清理")
	}
	if data, err := os.ReadFile(protected); err != nil || string(data) != "keep" {
		t.Fatal("清理影响了无关文件")
	}
}

func TestLocalChannelRejectsPublicDirectoryAndSymlink(t *testing.T) {
	directory := privateDirectory(t)
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if c, err := NewLocalChannel(context.Background(), directory, time.Second); err == nil {
		c.Close()
		t.Fatal("公开目录被接受")
	}
	_ = os.Chmod(directory, 0700)
	link := directory + "-link"
	if err := os.Symlink(directory, link); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(link)
	if c, err := NewLocalChannel(context.Background(), link, time.Second); err == nil {
		c.Close()
		t.Fatal("链接目录被接受")
	}
}

func TestLocalChannelCancelClosesIdleAcceptAndInFlightRead(t *testing.T) {
	for _, connect := range []bool{false, true} {
		directory := privateDirectory(t)
		c, err := NewLocalChannel(context.Background(), directory, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if connect {
			conn, err := net.Dial("unix", c.SocketPath())
			if err != nil {
				c.Close()
				t.Fatal(err)
			}
			defer conn.Close()
		}
		c.Close()
		if c.Status().State != "canceled" {
			t.Fatal("取消后仍有有效确认窗口")
		}
		if _, err := os.Lstat(filepath.Dir(c.SocketPath())); !os.IsNotExist(err) {
			t.Fatal("取消后本轮目录遗留")
		}
	}
}

func TestLocalChannelDeadlineStopsIdleAcceptWithoutCallerClose(t *testing.T) {
	directory := privateDirectory(t)
	c, err := NewLocalChannel(context.Background(), directory, 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("没有客户端时确认窗口到期未结束 accept")
	}
	if c.Status().State != "canceled" {
		t.Fatal("到期后仍可确认")
	}
	if _, err := os.Lstat(filepath.Dir(c.SocketPath())); !os.IsNotExist(err) {
		t.Fatal("到期后本轮目录遗留")
	}
}
