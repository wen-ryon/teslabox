//go:build linux

package carplaypairing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var operationID = regexp.MustCompile(`^[a-f0-9]{32}$`)

type serviceRequest struct {
	RequestID   string `json:"request_id"`
	PhoneID     string `json:"phone_id"`
	Action      string `json:"action"`
	ExpiresAtMS int64  `json:"expires_at_ms"`
}

type serviceProvisioning struct {
	ServiceActive   bool   `json:"-"`
	OnboardingReady bool   `json:"-"`
	RequestID       string `json:"request_id"`
	PhoneID         string `json:"phone_id"`
	State           string `json:"state"`
	Running         bool   `json:"running"`
}

type serviceClient struct{ runtime string }

// NewProvisioningManager 只有完成系统配对和服务侧新视频验收，才报告完整登记成功。
func NewProvisioningManager(binary, directory, runtime string) (*Manager, error) {
	pair, err := NativePairRunner(binary, directory)
	if err != nil || !filepath.IsAbs(runtime) {
		return nil, errors.New("首次登记需要有效原生程序和私有运行目录")
	}
	client := &serviceClient{runtime: runtime}
	m, err := NewManager(client.runner(pair))
	if err != nil {
		return nil, err
	}
	m.completedState = "video_verified"
	m.bindServiceRecovery(client)
	return m, nil
}

func privateInfo(info os.FileInfo, directory bool) error {
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 ||
		info.Mode()&os.ModeSymlink != 0 || directory != info.IsDir() || !directory && (!info.Mode().IsRegular() || owner.Nlink != 1) {
		return errors.New("首次登记运行资源不是当前用户私有持有")
	}
	return nil
}

func (c *serviceClient) directory() error {
	info, err := os.Lstat(c.runtime)
	if err != nil {
		return err
	}
	return privateInfo(info, true)
}

func (c *serviceClient) status() (serviceProvisioning, error) {
	var status struct {
		OnboardingReady bool                 `json:"onboarding_ready"`
		Provisioning    *serviceProvisioning `json:"provisioning"`
		Running         *bool                `json:"running"`
		CleanupComplete *bool                `json:"cleanup_complete"`
	}
	if err := c.directory(); err != nil {
		return serviceProvisioning{}, err
	}
	file, err := os.OpenFile(filepath.Join(c.runtime, "status.json"), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return serviceProvisioning{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return serviceProvisioning{}, err
	}
	if err := privateInfo(info, false); err != nil {
		return serviceProvisioning{}, err
	}
	if info.Size() > 16384 || time.Since(info.ModTime()) > 10*time.Second || time.Until(info.ModTime()) > time.Second {
		return serviceProvisioning{}, errors.New("登记服务状态过期或超过预算")
	}
	body, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || json.Unmarshal(body, &status) != nil || status.Provisioning == nil {
		return serviceProvisioning{}, errors.New("独立服务尚不支持首次登记")
	}
	if status.Running == nil || !*status.Running && (status.CleanupComplete == nil || !*status.CleanupComplete) {
		return serviceProvisioning{}, errors.New("独立服务清理状态尚未确认")
	}
	p := *status.Provisioning
	p.ServiceActive = *status.Running
	p.OnboardingReady = status.OnboardingReady && !*status.Running && status.CleanupComplete != nil && *status.CleanupComplete
	if p.State == "idle" && !p.Running && p.RequestID == "" && p.PhoneID == "" {
		return p, nil
	}
	if !operationID.MatchString(p.RequestID) || !phoneAddress.MatchString(p.PhoneID) {
		return p, errors.New("登记状态标识不合法")
	}
	switch p.State {
	case "paused", "enrolling":
		if !p.Running {
			return p, errors.New("活动登记状态不一致")
		}
	case "canceled", "expired", "failed", "phone_not_paired", "video_verified":
		if p.Running {
			return p, errors.New("登记终态尚未清理")
		}
	default:
		return p, errors.New("未知登记状态")
	}
	return p, nil
}

func (c *serviceClient) write(request serviceRequest) error {
	if err := c.directory(); err != nil {
		return err
	}
	if !operationID.MatchString(request.RequestID) || !phoneAddress.MatchString(request.PhoneID) || request.PhoneID == "00:00:00:00:00:00" ||
		request.ExpiresAtMS <= 0 || request.ExpiresAtMS > 9007199254740991 ||
		(request.Action != "pause" && request.Action != "enroll" && request.Action != "cancel") {
		return errors.New("登记请求标识不合法")
	}
	file, err := os.CreateTemp(c.runtime, ".provisioning-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err := json.NewEncoder(file).Encode(request); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(c.runtime, "provisioning-request.json")); err != nil {
		return err
	}
	directory, err := os.Open(c.runtime)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (c *serviceClient) wait(ctx context.Context, request serviceRequest, wanted string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, err := c.status()
		if err == nil && state.RequestID == request.RequestID && state.PhoneID == request.PhoneID {
			if wanted == "terminal" && !state.Running || state.State == wanted {
				return nil
			}
			if !state.Running {
				return errors.New("首次登记未取得所需会话证据")
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *serviceClient) claim() (*os.File, error) {
	if err := c.directory(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(c.runtime, "provisioning-client.lock"), os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if err := privateInfo(info, false); err != nil {
		file.Close()
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, ErrBusy
	}
	return file, nil
}

func (c *serviceClient) runner(pair PairRunner) PairRunner {
	return func(ctx context.Context, phone string, confirmation *Confirmation) (result error) {
		claim, err := c.claim()
		if err != nil {
			return err
		}
		defer claim.Close()
		initial, err := c.status()
		if err != nil {
			return err
		}
		if initial.Running {
			return ErrBusy
		}
		if !initial.ServiceActive && !initial.OnboardingReady {
			return errors.New("登记服务尚未运行")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 120*time.Second {
			return errors.New("登记需要限时上下文")
		}
		request := serviceRequest{RequestID: confirmation.Status().RequestID, PhoneID: phone, Action: "pause", ExpiresAtMS: deadline.UnixMilli()}
		// 写请求后即承担恢复责任，不能把等待暂停超时解释为尚未改动服务。
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 50*time.Second)
			defer cancel()
			if result != nil {
				confirmation.recordFailurePhase()
				confirmation.setPhase("restoring")
			}
			if err := c.release(cleanup, request); err != nil {
				result = ErrCleanupPending
			}
			if confirmation.handoff != nil {
				if err := confirmation.handoff.FinishCarPlayHandoff(cleanup, request.RequestID, result == nil); err != nil {
					result = ErrCleanupPending
				}
			}
		}()
		if err := c.write(request); err != nil {
			return err
		}
		if err := c.wait(ctx, request, "paused"); err != nil {
			return err
		}
		confirmation.setPhase("bluetooth_pairing")
		if err := pair(ctx, phone, confirmation); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if confirmation.handoff != nil {
			confirmation.setPhase("network_handoff")
			if err := confirmation.handoff.BeginCarPlayHandoff(ctx, request.RequestID, phone, confirmation.handoffPlan); err != nil {
				return err
			}
		}
		request.Action = "enroll"
		confirmation.setPhase("waiting_video")
		if err := c.write(request); err != nil {
			return err
		}
		return c.wait(ctx, request, "video_verified")
	}
}

func (c *serviceClient) release(ctx context.Context, request serviceRequest) error {
	request.Action = "cancel"
	if err := c.write(request); err != nil {
		return err
	}
	return c.wait(ctx, request, "terminal")
}

func (c *serviceClient) reconcile(ctx context.Context, id, phone string, expires time.Time) error {
	claim, err := c.claim()
	if err != nil {
		return err
	}
	defer claim.Close()
	return c.release(ctx, serviceRequest{RequestID: id, PhoneID: phone, ExpiresAtMS: expires.UnixMilli()})
}

func (m *Manager) bindServiceRecovery(client *serviceClient) {
	m.recoveredVideo = func(id string) bool {
		state, err := client.status()
		return err == nil && state.RequestID == id && state.State == "video_verified"
	}
	m.reconcile = func(ctx context.Context, id, phone string, expires time.Time) error {
		if err := client.reconcile(ctx, id, phone, expires); err != nil {
			return err
		}
		if m.handoff == nil {
			return nil
		}
		state, err := client.status()
		if err != nil {
			return err
		}
		return m.handoff.FinishCarPlayHandoff(ctx, id, state.State == "video_verified")
	}
}
