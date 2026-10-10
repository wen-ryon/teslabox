package automation

import (
	"context"
	"net/http"
	"time"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
)

type carPlayUpstreamCoordinator interface {
	EnsureCarPlayUpstream(context.Context, string, string, func(*deviceadmin.CarPlayNetworkLease) error) (deviceadmin.CarPlayUpstreamState, error)
}

type carPlayPANResetCoordinator interface {
	ResetCarPlayPAN(context.Context, string, string, func(*deviceadmin.CarPlayNetworkLease) error) error
}

func (s *Service) handleCarPlayUpstream(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	if !ownerPattern.MatchString(owner) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "租约名称无效"})
		return
	}
	var input struct {
		Phone    string `json:"phone"`
		ResetPAN bool   `json:"reset_pan"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.carPlayRecoveryRequired || s.carPlayLease == nil || s.carPlayLease.Owner != owner || !s.carPlayLease.ExpiresAt.After(s.config.Now()) {
		s.writeOperationError(w, "CarPlay 租约无效或已过期", carPlayLeaseBusy())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if input.ResetPAN {
		coordinator, supported := s.admin.(carPlayPANResetCoordinator)
		if !supported {
			writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "网络管理器不支持 CarPlay 供网恢复"})
			return
		}
		if err := coordinator.ResetCarPlayPAN(ctx, owner, input.Phone, s.checkpointCarPlayLocked); err != nil {
			s.writeOperationError(w, "释放 CarPlay 蓝牙供网失败", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"pan_reset": true})
		return
	}
	coordinator, supported := s.admin.(carPlayUpstreamCoordinator)
	if !supported {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "网络管理器不支持 CarPlay 蓝牙供网"})
		return
	}
	state, err := coordinator.EnsureCarPlayUpstream(ctx, owner, input.Phone, s.checkpointCarPlayLocked)
	if err != nil {
		s.writeOperationError(w, "检查 CarPlay 上游失败", err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}
