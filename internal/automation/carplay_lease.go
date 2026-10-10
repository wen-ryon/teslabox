package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
)

type carPlayNetworkCoordinator interface {
	AcquireCarPlayNetwork(context.Context, string, deviceadmin.CarPlayNetworkRequest, time.Time, func(*deviceadmin.CarPlayNetworkLease) error) (deviceadmin.CarPlayNetworkLease, error)
	ReleaseCarPlayNetwork(context.Context, string, func(*deviceadmin.CarPlayNetworkLease) error) error
	RestoreCarPlayNetworkHold(deviceadmin.CarPlayNetworkLease) error
}

type carPlayPersistedState struct {
	Version int                              `json:"version"`
	Lease   *deviceadmin.CarPlayNetworkLease `json:"lease"`
}

type carPlayRecoveryGuard interface {
	GuardCarPlayRecovery()
}

func (s *Service) restoreCarPlayProtection() error {
	if err := s.restoreCarPlayHold(); err != nil {
		guard, ok := s.admin.(carPlayRecoveryGuard)
		if !ok {
			return err
		}
		guard.GuardCarPlayRecovery()
		s.carPlayRecoveryRequired = true
		// 解析错误可能包含文件内容，不把不可信恢复记录写入业务日志。
		s.config.Logger.Error("CarPlay 恢复记录无法确认，网络变更已隔离，其他服务继续启动", "state_path", s.carPlayStatePath())
	}
	return nil
}

func carPlayLeaseBusy() error {
	return &deviceadmin.APOperationError{Code: "carplay_network_reserved",
		Message: "CarPlay 网络租约尚未释放", Suggestion: "请先结束当前 CarPlay 会话，再申请其他网络租约。"}
}

func (s *Service) carPlayStatePath() string {
	return s.config.CarPlayStatePath
}

func (s *Service) checkpointCarPlayLocked(lease *deviceadmin.CarPlayNetworkLease) error {
	content, err := json.Marshal(carPlayPersistedState{Version: 1, Lease: lease})
	if err != nil {
		return err
	}
	if err := writeRuntimeState(s.carPlayStatePath(), content); err != nil {
		return fmt.Errorf("保存 CarPlay 网络恢复记录: %w", err)
	}
	s.carPlayLease = nil
	if lease != nil {
		snapshot := *lease
		s.carPlayLease = &snapshot
	}
	return nil
}

func (s *Service) restoreCarPlayHold() error {
	path := s.carPlayStatePath()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 16384 {
		return errors.New("CarPlay 恢复记录不是受限大小的普通文件")
	}
	if err := validatePrivateStateFile(info); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 16385))
	decoder.DisallowUnknownFields()
	var state carPlayPersistedState
	if err := decoder.Decode(&state); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("CarPlay 恢复记录包含多余内容")
	}
	if state.Version != 1 {
		return errors.New("CarPlay 恢复记录版本不支持")
	}
	if state.Lease == nil {
		return nil
	}
	coordinator, supported := s.admin.(carPlayNetworkCoordinator)
	if !supported {
		return errors.New("当前网络管理器不能恢复 CarPlay 租约")
	}
	if err := coordinator.RestoreCarPlayNetworkHold(*state.Lease); err != nil {
		return err
	}
	s.carPlayLease = state.Lease
	return nil
}

func (s *Service) handleCarPlayState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := map[string]any{"lease": s.carPlayLease}
	if s.carPlayRecoveryRequired {
		result["recovery_required"] = true
		result["code"] = "carplay_recovery_required"
		result["error"] = "CarPlay 恢复记录无法确认，请检查原记录后重启服务"
	}
	if inspector, ok := s.admin.(carPlayAPInspector); ok && s.carPlayLease != nil {
		healthy, err := inspector.CarPlayAPHealthy(r.Context(), s.carPlayLease.Owner)
		if err != nil {
			s.writeOperationError(w, "检查 CarPlay 热点失败", err)
			return
		}
		result["ap_healthy"] = healthy
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Service) handleCarPlayAcquire(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	if !ownerPattern.MatchString(owner) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "租约名称无效"})
		return
	}
	var input struct {
		Channels   []int `json:"channels"`
		TTLSeconds int   `json:"ttl_seconds"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	if input.TTLSeconds == 0 {
		input.TTLSeconds = int(defaultLeaseTTL / time.Second)
	}
	if input.TTLSeconds < 15 || input.TTLSeconds > 3600 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "租约有效期必须在 15 秒到 1 小时之间"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.carPlayRecoveryRequired {
		s.writeOperationError(w, "CarPlay 恢复记录需要检查", carPlayLeaseBusy())
		return
	}
	coordinator, supported := s.admin.(carPlayNetworkCoordinator)
	if !supported {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "网络管理器不支持 CarPlay 网络租约"})
		return
	}
	if s.carPlayLease != nil && !s.carPlayLease.ExpiresAt.After(s.config.Now()) {
		if err := s.releaseCarPlayLocked(r.Context(), s.carPlayLease.Owner); err != nil {
			s.writeOperationError(w, "回收上轮 CarPlay 租约失败", err)
			return
		}
	}
	s.purgeExpiredLocked(s.config.Now())
	if len(s.leases) != 0 || s.managed {
		s.writeOperationError(w, "其他自动化正在使用热点", carPlayLeaseBusy())
		return
	}
	expiresAt := s.config.Now().Add(time.Duration(input.TTLSeconds) * time.Second)
	newLease := s.carPlayLease == nil
	lease, err := coordinator.AcquireCarPlayNetwork(r.Context(), owner,
		deviceadmin.CarPlayNetworkRequest{Channels: input.Channels}, expiresAt, s.checkpointCarPlayLocked)
	if err != nil {
		s.writeOperationError(w, "申请 CarPlay 网络租约失败", err)
		return
	}
	// 仅记录新租约的规划，续租不刷屏；不输出热点凭据和手机身份。
	if newLease {
		s.config.Logger.Info("CarPlay 网络租约已申请",
			"channel", lease.Plan.Channel, "allowed_channels", input.Channels,
			"concurrency_mode", lease.Plan.ConcurrencyMode,
			"upstream_present", lease.Plan.UpstreamUUID != "", "pause_upstream", lease.Plan.PauseUpstream)
	}
	writeJSON(w, http.StatusOK, lease)
}

func (s *Service) releaseCarPlayLocked(ctx context.Context, owner string) error {
	if s.carPlayRecoveryRequired {
		return carPlayLeaseBusy()
	}
	if s.carPlayLease == nil {
		return nil
	}
	if s.carPlayLease.Owner != owner {
		return carPlayLeaseBusy()
	}
	if err := s.removeCarPlayAccessLocked(*s.carPlayLease); err != nil {
		return err
	}
	coordinator, supported := s.admin.(carPlayNetworkCoordinator)
	if !supported {
		return errors.New("网络管理器不支持 CarPlay 租约恢复")
	}
	return coordinator.ReleaseCarPlayNetwork(ctx, owner, s.checkpointCarPlayLocked)
}

func (s *Service) handleCarPlayRelease(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	if !ownerPattern.MatchString(owner) || r.ContentLength > 0 || len(r.TransferEncoding) != 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "释放租约只接受有效来源，不接受请求内容"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	operationCtx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	if err := s.releaseCarPlayLocked(operationCtx, owner); err != nil {
		s.writeOperationError(w, "恢复 CarPlay 网络失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"released": true})
}
