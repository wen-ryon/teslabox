package automation

import (
	"context"
	"net/http"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
)

type carPlayNetworkPlanner interface {
	PlanCarPlayNetwork(context.Context, deviceadmin.CarPlayNetworkRequest) (deviceadmin.CarPlayNetworkPlan, error)
}

func (s *Service) handleCarPlayPlan(w http.ResponseWriter, r *http.Request) {
	planner, ok := s.admin.(carPlayNetworkPlanner)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "网络管理器不支持 CarPlay 网络规划"})
		return
	}
	var input deviceadmin.CarPlayNetworkRequest
	if !decodeRequest(w, r, &input) {
		return
	}
	plan, err := planner.PlanCarPlayNetwork(r.Context(), input)
	if err != nil {
		s.writeOperationError(w, "规划 CarPlay 网络失败", err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}
