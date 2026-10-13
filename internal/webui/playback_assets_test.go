package webui

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlaybackAssetsServeActualJavaScript(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	for _, name := range []string{"playback-hls.js", "playback-trace.js", "playback-probe-plan.js", "playback-probe-matrix.js", "runtime-components.js", "page-display.js"} {
		expected, err := assets.ReadFile("assets/" + name)
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest("GET", "/"+name+"?v=integration", nil))
		if recorder.Code != 200 || !strings.Contains(recorder.Header().Get("Content-Type"), "javascript") || recorder.Body.String() != string(expected) {
			t.Fatalf("资源 %s 未返回实际脚本：状态 %d，类型 %s", name, recorder.Code, recorder.Header().Get("Content-Type"))
		}
	}
}
