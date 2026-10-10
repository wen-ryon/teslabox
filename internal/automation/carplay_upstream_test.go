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

type fakeCarPlayUpstream struct {
	*fakeCarPlayCoordinator
	calls  int
	resets int
}

func (a *fakeCarPlayUpstream) ResetCarPlayPAN(_ context.Context, _, _ string, _ func(*deviceadmin.CarPlayNetworkLease) error) error {
	a.resets++
	return nil
}

func TestCarPlayPANResetRequiresLiveOwnedLease(t *testing.T) {
	admin := &fakeCarPlayUpstream{fakeCarPlayCoordinator: &fakeCarPlayCoordinator{fakeAPAdmin: &fakeAPAdmin{}}}
	service := newCarPlayLeaseService(t, admin.fakeCarPlayCoordinator, time.Now)
	service.admin = admin
	request := func(owner string) int {
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/network/carplay/leases/"+owner+"/upstream", strings.NewReader(`{"phone":"AA:BB:CC:DD:EE:FF","reset_pan":true}`)))
		return response.Code
	}
	if request("carplay") == http.StatusOK || admin.resets != 0 {
		t.Fatal("无租约允许清理")
	}
	if response := leaseRequest(service, http.MethodPut, "carplay", `{"channels":[1,6,11]}`); response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	if request("other") == http.StatusOK || admin.resets != 0 {
		t.Fatal("其他来源允许清理")
	}
	if request("carplay") != http.StatusOK || admin.resets != 1 || admin.calls != 0 {
		t.Fatal("清理请求被当成供网拨号")
	}
	service.carPlayLease.ExpiresAt = time.Now().Add(-time.Minute)
	if request("carplay") == http.StatusOK || admin.resets != 1 {
		t.Fatal("过期允许清理")
	}
}

func (a *fakeCarPlayUpstream) EnsureCarPlayUpstream(_ context.Context, _, _ string, _ func(*deviceadmin.CarPlayNetworkLease) error) (deviceadmin.CarPlayUpstreamState, error) {
	a.calls++
	return deviceadmin.CarPlayUpstreamState{Source: "existing_upstream", Interface: "eth0", Internet: true}, nil
}

func TestCarPlayUpstreamRequiresLiveOwnedLease(t *testing.T) {
	admin := &fakeCarPlayUpstream{fakeCarPlayCoordinator: &fakeCarPlayCoordinator{fakeAPAdmin: &fakeAPAdmin{}}}
	service := newCarPlayLeaseService(t, admin.fakeCarPlayCoordinator, time.Now)
	service.admin = admin
	request := func(owner string) int {
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/network/carplay/leases/"+owner+"/upstream", strings.NewReader(`{"phone":"AA:BB:CC:DD:EE:FF"}`)))
		return response.Code
	}
	if request("carplay") == http.StatusOK || admin.calls != 0 {
		t.Fatal("无租约仍可启用供网")
	}
	if response := leaseRequest(service, http.MethodPut, "carplay", `{"channels":[1,6,11]}`); response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	if request("other") == http.StatusOK || admin.calls != 0 {
		t.Fatal("其他来源修改了供网")
	}
	if request("carplay") != http.StatusOK || admin.calls != 1 {
		t.Fatal("租约来源无法检查供网")
	}
	service.carPlayLease.ExpiresAt = time.Now().Add(-time.Minute)
	if request("carplay") == http.StatusOK || admin.calls != 1 {
		t.Fatal("过期租约仍可启用供网")
	}
}
