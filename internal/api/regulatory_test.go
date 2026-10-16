package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wen-ryon/teslabox/internal/regulatory"
)

type regionDeviceAdmin struct {
	fakeDeviceAdmin
	country string
	changed bool
}

func (a *regionDeviceAdmin) WirelessRegion(context.Context) (regulatory.Status, error) {
	return regulatory.Status{ConfiguredCountry: "CN", EffectiveCountry: "00", State: "pending", Saved: true}, nil
}
func (a *regionDeviceAdmin) SetWirelessRegion(_ context.Context, country string) (regulatory.Status, error) {
	a.changed = true
	a.country = country
	return regulatory.Status{ConfiguredCountry: country, EffectiveCountry: "00", Saved: true, State: "pending", ReasonCode: "regulatory_apply_failed"}, nil
}

func TestRegulatoryRoutesDoNotEquateSavedWithEffective(t *testing.T) {
	t.Parallel()
	a := &regionDeviceAdmin{}
	h := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithDeviceAdmin(a))
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/v1/network/regulatory", nil))
	if get.Code != 200 || !strings.Contains(get.Body.String(), `"effective_country":"00"`) {
		t.Fatalf("%d %s", get.Code, get.Body)
	}
	post := httptest.NewRecorder()
	h.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/api/v1/network/regulatory", strings.NewReader(`{"country":"JP","confirm_network_change":true}`)))
	if post.Code != 200 || !a.changed || a.country != "JP" || !strings.Contains(post.Body.String(), `"state":"pending"`) {
		t.Fatalf("%d %s", post.Code, post.Body)
	}
}

func TestRegulatoryChangeRequiresExplicitNetworkConfirmation(t *testing.T) {
	t.Parallel()
	a := &regionDeviceAdmin{}
	h := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithDeviceAdmin(a))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/network/regulatory", strings.NewReader(`{"country":"CN"}`)))
	if w.Code != http.StatusBadRequest || a.changed {
		t.Fatalf("%d changed=%v", w.Code, a.changed)
	}
}
