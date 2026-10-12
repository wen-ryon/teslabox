package updater

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

func startJournal(config HelperConfig) (string, error) {
	dir := filepath.Join(config.BackupDir, "active")
	if commitPending(dir) {
		return "", errors.New("提交隔离未解除，禁止启动许可")
	}
	tx, err := readTransaction(dir, transactionTargets(config))
	if err != nil {
		return "", err
	}
	if tx.Phase != "ready" && tx.Phase != "rolled_back" {
		return "", errors.New("事务尚未到达安全启动点")
	}
	for i, file := range tx.Files {
		expected := file.Before
		if i == 0 && tx.Phase == "ready" {
			if tx.Candidate == nil {
				return "", errors.New("缺少已落盘候选快照")
			}
			expected = *tx.Candidate
		}
		limit := int64(16 << 20)
		if i == 0 {
			limit = 128 << 20
		}
		actual, _, err := snapshotFile(file.Target, limit)
		if err != nil {
			return "", err
		}
		if !sameSnapshot(expected, actual) {
			return "", errors.New("待启动程序或状态与许可不匹配")
		}
	}
	journal, _, err := snapshotFile(filepath.Join(dir, transactionName), 64<<10)
	return journal.SHA256, err
}

func authorizedStart(ctx context.Context, config HelperConfig, start func() error) error {
	journal, err := startJournal(config)
	if err != nil {
		return err
	}
	path := filepath.Join(config.BackupDir, "start.sock")
	if err = plainPath(path); err != nil {
		return err
	}
	if info, e := os.Lstat(path); e == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("启动许可路径被非 socket 文件占用")
		}
		if err = safeFileMetadata(info); err != nil {
			return err
		}
		probe, e := net.DialTimeout("unix", path, 200*time.Millisecond)
		if e == nil {
			probe.Close()
			return errors.New("启动许可路径已有活动监听")
		}
		if !errors.Is(e, syscall.ECONNREFUSED) {
			return e
		}
		current, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if !os.SameFile(info, current) {
			return errors.New("残留 socket 检查期间被替换")
		}
		if err = os.Remove(path); err != nil {
			return err
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	defer listener.Close()
	if err = os.Chmod(path, 0o600); err != nil {
		return err
	}
	stopClose := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stopClose()
	var consumed atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		if err = verifyPeer(conn); err != nil {
			return
		}
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
		// 不写磁盘许可：助手退出后，残留 socket 也无法再提供授权。
		if err := json.NewEncoder(conn).Encode(journal); err != nil {
			return
		}
		var ack bool
		if json.NewDecoder(io.LimitReader(conn, 64)).Decode(&ack) == nil && ack {
			consumed.Store(true)
		}
		_ = listener.Close()
	}()
	err = start()
	_ = listener.Close()
	<-done
	if err != nil {
		return err
	}
	if !consumed.Load() {
		return errors.New("启动守卫未消费本次许可，不能提交安装")
	}
	return nil
}

func claimAuthorizedStart(ctx context.Context, config HelperConfig) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(config.BackupDir, "start.sock"))
	if err != nil {
		return errors.New("安装锁被占用且没有有效启动许可")
	}
	defer conn.Close()
	if err = verifyPeer(conn.(*net.UnixConn)); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	var allowed string
	if err = json.NewDecoder(io.LimitReader(conn, 128)).Decode(&allowed); err != nil {
		return err
	}
	actual, err := startJournal(config)
	if err != nil {
		return err
	}
	if allowed == "" || allowed != actual {
		return errors.New("启动许可已过期或事务记录已变化")
	}
	return json.NewEncoder(conn).Encode(true)
}

func verifyPeer(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var credential *syscall.Ucred
	var peerErr error
	if err = raw.Control(func(fd uintptr) {
		credential, peerErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if peerErr != nil {
		return peerErr
	}
	if credential == nil || credential.Uid != uint32(os.Geteuid()) {
		return errors.New("启动许可对端身份不匹配")
	}
	return nil
}
