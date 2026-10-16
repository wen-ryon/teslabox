package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWirelessRegionAssetIsServed(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/wireless-region.js", nil))
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") || !strings.Contains(w.Body.String(), "TeslaBoxWirelessRegion") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
