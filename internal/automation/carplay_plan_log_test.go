package automation

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCarPlayPlanLogDoesNotRepeatOnRenewal(t *testing.T) {
	t.Parallel()
	admin := &fakeCarPlayCoordinator{fakeAPAdmin: &fakeAPAdmin{}}
	service := newCarPlayLeaseService(t, admin, time.Now)
	var output bytes.Buffer
	service.config.Logger = slog.New(slog.NewJSONHandler(&output, nil))
	for i := 0; i < 2; i++ {
		response := leaseRequest(service, http.MethodPut, "carplay-service", `{"channels":[149],"ttl_seconds":60}`)
		if response.Code != http.StatusOK {
			t.Fatalf("lease request failed: %d %s", response.Code, response.Body.String())
		}
	}
	text := output.String()
	if strings.Count(text, "CarPlay 网络租约已申请") != 1 || !strings.Contains(text, `"pause_upstream":true`) ||
		!strings.Contains(text, `"allowed_channels":[149]`) || !strings.Contains(text, `"channel":149`) {
		t.Fatalf("missing plan decision or repeated renewal log: %s", text)
	}
	if strings.Contains(text, admin.held.Plan.UpstreamUUID) || strings.Contains(text, admin.held.RuntimeAPUUID) {
		t.Fatal("plan log included network identities")
	}
}
