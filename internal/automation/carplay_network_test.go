package automation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
)

type fakeCarPlayPlanner struct {
	*fakeAPAdmin
	requests []deviceadmin.CarPlayNetworkRequest
}

func (a *fakeCarPlayPlanner) PlanCarPlayNetwork(_ context.Context, request deviceadmin.CarPlayNetworkRequest) (deviceadmin.CarPlayNetworkPlan, error) {
	a.requests = append(a.requests, request)
	return deviceadmin.CarPlayNetworkPlan{Channel: 149, PauseUpstream: true}, nil
}

func TestCarPlayPlanningRouteIsReadOnlyAndStrict(t *testing.T) {
	admin := &fakeCarPlayPlanner{fakeAPAdmin: &fakeAPAdmin{settings: defaultAPSettings(false, false)}}
	service := newTestService(t, admin, time.Now)
	for _, test := range []struct {
		body string
		code int
	}{
		{`{"channels":[149,153,157,161]}`, http.StatusOK},
		{`{"channels":[149],"disconnect":true}`, http.StatusBadRequest},
		{`{"channels":[149]} {}`, http.StatusBadRequest},
		{`{"channels":"149"}`, http.StatusBadRequest},
	} {
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/network/carplay/plan", strings.NewReader(test.body)))
		if response.Code != test.code {
			t.Fatalf("状态 = %d，响应 = %s", response.Code, response.Body.String())
		}
		if test.code == http.StatusOK && !strings.Contains(response.Body.String(), `"pause_upstream":true`) {
			t.Fatalf("缺少暂停决策: %s", response.Body.String())
		}
	}
	if len(admin.requests) != 1 || len(admin.requests[0].Channels) != 4 || admin.disconnectCalls != 0 || len(admin.updates) != 0 {
		t.Fatalf("规划触发了变更或无效请求被转发: %+v", admin)
	}
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "network.carplay.plan") {
		t.Fatalf("未报告规划能力: %s", response.Body.String())
	}
}

func TestCarPlayPlanningRequiresSupportedAdmin(t *testing.T) {
	service := newTestService(t, &fakeAPAdmin{}, time.Now)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/network/carplay/plan", strings.NewReader(`{"channels":[149]}`)))
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("不支持规划时状态 = %d", response.Code)
	}
	response = httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
	if strings.Contains(response.Body.String(), "network.carplay.plan") {
		t.Fatalf("报告了不存在的规划能力: %s", response.Body.String())
	}
}
