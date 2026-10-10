package webui

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCarPlayNetworkStatusReportsRealOfflineAndExcludesReceiverDetails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(path, []byte(`{"running":true,"upstream":{"source":"wifi","internet":false},"receiver":{"token":"private-fixture"},"offline_compatibility":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	serveCarPlayNetwork(response, path)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"internet":false`) || strings.Contains(response.Body.String(), "private-fixture") || strings.Contains(response.Body.String(), "offline_compatibility") {
		t.Fatalf("离线真实状态边界错误: %d %s", response.Code, response.Body.String())
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	serveCarPlayNetwork(response, path)
	if response.Code != 503 {
		t.Fatalf("过期状态被当成实时状态: %d", response.Code)
	}
}
