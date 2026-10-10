package webui

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPhoneListOnlyExposesValidatedPublicFields(t *testing.T) {
	file := filepath.Join(t.TempDir(), "status.json")
	body := `{"running":false,"phones":{"selected":"02:00:00:00:00:01","last_successful":null,"phones":[{"id":"02:00:00:00:00:01","name":"测试 iPhone","controller":"private-controller"}],"legacy_pending":true},"receiver":{"token":"private-token"}}`
	if err := os.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	serveCarPlayPhones(response, file)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"id":"02:00:00:00:00:01"`) || !strings.Contains(response.Body.String(), "测试 iPhone") || strings.Contains(response.Body.String(), "private") || strings.Contains(response.Body.String(), "legacy") {
		t.Fatalf("手机列表边界错误: %d %s", response.Code, response.Body.String())
	}
	for _, value := range []string{
		`{"phones":{"selected":"192.168.88.151","phones":[]}}`,
		`{"phones":{"phones":[{"id":"02:00:00:00:00:01"},{"id":"02:00:00:00:00:01"}]}}`,
		`{"phones":{"phones":[{"id":"invalid"}]}}`,
	} {
		if err := os.WriteFile(file, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		response = httptest.NewRecorder()
		serveCarPlayPhones(response, file)
		if response.Code != 503 {
			t.Fatalf("接受了无效手机列表: %d %s", response.Code, response.Body.String())
		}
	}
}
