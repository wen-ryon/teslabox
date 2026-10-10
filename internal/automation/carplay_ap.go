package automation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
)

type carPlayAPStarter interface {
	StartCarPlayAP(context.Context, string) (deviceadmin.CarPlayAPAccess, error)
}

type carPlayAPInspector interface {
	CarPlayAPHealthy(context.Context, string) (bool, error)
}

func (s *Service) carPlayAccessPath(lease deviceadmin.CarPlayNetworkLease) string {
	return filepath.Join(s.config.CarPlayAccessDirectory, "access-"+lease.RuntimeAPUUID+".json")
}

func (s *Service) handleCarPlayAPStart(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	if !ownerPattern.MatchString(owner) || r.ContentLength > 0 || len(r.TransferEncoding) != 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "启动热点只接受有效租约来源"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.carPlayRecoveryRequired {
		s.writeOperationError(w, "CarPlay 恢复记录需要检查", carPlayLeaseBusy())
		return
	}
	if s.carPlayLease == nil || s.carPlayLease.Owner != owner || !s.carPlayLease.ExpiresAt.After(s.config.Now()) {
		s.writeOperationError(w, "CarPlay 租约无效或已过期", carPlayLeaseBusy())
		return
	}
	starter, supported := s.admin.(carPlayAPStarter)
	if !supported {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "网络管理器不支持 CarPlay 热点启动"})
		return
	}
	directory := s.config.CarPlayAccessDirectory
	if err := os.MkdirAll(directory, 0o700); err != nil {
		s.writeOperationError(w, "创建 CarPlay 私有目录失败", err)
		return
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		s.writeOperationError(w, "CarPlay 私有目录无效", errors.New("私有目录不能是符号链接或普通文件"))
		return
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		s.writeOperationError(w, "收紧 CarPlay 私有目录权限失败", err)
		return
	}
	operationCtx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	access, err := starter.StartCarPlayAP(operationCtx, owner)
	if err != nil {
		s.writeOperationError(w, "启动 CarPlay 热点失败", err)
		return
	}
	if access.RuntimeAPUUID != s.carPlayLease.RuntimeAPUUID || access.Interface != s.carPlayLease.Plan.APInterface || access.Channel != s.carPlayLease.Plan.Channel {
		s.writeOperationError(w, "CarPlay 热点身份与租约不一致", carPlayLeaseBusy())
		return
	}
	content, err := json.Marshal(access)
	if err != nil {
		s.writeOperationError(w, "写入 CarPlay 私有交接文件失败", errors.New("私有交接信息编码失败"))
		return
	}
	path := s.carPlayAccessPath(*s.carPlayLease)
	if err := writeRuntimeState(path, content); err != nil {
		s.writeOperationError(w, "写入 CarPlay 私有交接文件失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runtime_ap_uuid": access.RuntimeAPUUID,
		"interface": access.Interface, "channel": access.Channel, "credential_file": path})
}

func (s *Service) removeCarPlayAccessLocked(lease deviceadmin.CarPlayNetworkLease) error {
	path := s.carPlayAccessPath(lease)
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return syncStateDirectory(filepath.Dir(path))
}
