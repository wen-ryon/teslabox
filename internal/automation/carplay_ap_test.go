package automation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
)

func (a *fakeCarPlayCoordinator) StartCarPlayAP(_ context.Context, owner string) (deviceadmin.CarPlayAPAccess, error) {
	if a.held == nil || owner != a.held.Owner {
		return deviceadmin.CarPlayAPAccess{}, carPlayLeaseBusy()
	}
	return deviceadmin.CarPlayAPAccess{RuntimeAPUUID: a.held.RuntimeAPUUID, Interface: a.held.Plan.APInterface,
		Channel: a.held.Plan.Channel, SSID: "TeslaBox-private-test", Password: "private-test-password", MACAddress: "AA:BB:CC:DD:EE:FF", Addresses: []string{"192.168.88.1/24"}}, nil
}

func TestCarPlayAPPrivateHandoffAndRelease(t *testing.T) {
	admin := &fakeCarPlayCoordinator{fakeAPAdmin: &fakeAPAdmin{}}
	service := newCarPlayLeaseService(t, admin, time.Now)
	if response := leaseRequest(service, http.MethodPut, "carplay", `{"channels":[149]}`); response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	for _, owner := range []string{"other", "carplay"} {
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/network/carplay/leases/"+owner+"/ap", nil))
		if owner == "other" {
			if response.Code == http.StatusOK {
				t.Fatal("其他来源启动了热点")
			}
			continue
		}
		if response.Code != http.StatusOK {
			t.Fatal(response.Body.String())
		}
		if strings.Contains(response.Body.String(), "private-test") || strings.Contains(response.Body.String(), "password") {
			t.Fatal("CLI 响应泄露凭据")
		}
	}
	path := service.carPlayAccessPath(*service.carPlayLease)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var access deviceadmin.CarPlayAPAccess
	if err := json.Unmarshal(content, &access); err != nil || access.Password != "private-test-password" {
		t.Fatal("私有文件缺少凭据")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateStateFile(info); err != nil {
		t.Fatal(err)
	}
	if response := leaseRequest(service, http.MethodDelete, "carplay", ""); response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("释放后仍留下凭据文件")
	}
}
