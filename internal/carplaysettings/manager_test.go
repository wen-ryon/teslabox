package carplaysettings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAutoPlayDefaultsOffAndPersists(t *testing.T) {
	for _, name := range []string{"new", "legacy"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if name == "legacy" {
				if err := os.WriteFile(path, []byte(`{"revision":3,"network":"auto","quality":720,"fps":30}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			manager, err := New(path)
			if err != nil {
				t.Fatal(err)
			}
			if manager.Settings().AutoPlay {
				t.Fatal("升级意外开启自动播放")
			}
			for _, enabled := range []bool{true, false} {
				next := manager.Settings()
				next.AutoPlay = enabled
				if _, err := manager.Save(next); err != nil {
					t.Fatal(err)
				}
				manager, err = New(path)
				if err != nil || manager.Settings().AutoPlay != enabled {
					t.Fatalf("自动播放未持久化: %v", err)
				}
			}
		})
	}
}

func TestDashboardLayoutMigratesPersistsAndConstrainsDisplay(t *testing.T) {
	for _, test := range []struct {
		name                 string
		quality, input, want int
	}{
		{"480p", 480, 616, 564}, {"720p", 720, 926, 876}, {"1080p", 1080, 1388, 1344},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, err := New(filepath.Join(t.TempDir(), "settings.json"))
			if err != nil {
				t.Fatal(err)
			}
			if m.Settings().DashboardLayout != "auto" {
				t.Fatal("默认布局改变")
			}
			next := m.Settings()
			next.Quality, next.DashboardLayout = test.quality, "vertical"
			saved, err := m.Save(next)
			if err != nil {
				t.Fatal(err)
			}
			m, err = New(m.path)
			if err != nil || m.Settings().DashboardLayout != "vertical" {
				t.Fatalf("布局未持久化: %v", err)
			}
			got, err := m.ApplyDisplay(saved.Revision, test.input, test.quality)
			if err != nil || got.Width != test.want || got.Height != test.quality {
				t.Fatalf("布局尺寸: %+v %v", got, err)
			}
			content, err := os.ReadFile(m.path + ".display")
			if err != nil {
				t.Fatal(err)
			}
			var disk Display
			if json.Unmarshal(content, &disk) != nil || disk != got {
				t.Fatal("实际协商尺寸未保存")
			}
			next = m.Settings()
			next.DashboardLayout = "auto"
			if saved, err = m.Save(next); err != nil {
				t.Fatal(err)
			}
			got, err = m.ApplyDisplay(saved.Revision, test.input, test.quality)
			if err != nil || got.Width != test.input {
				t.Fatalf("跟随屏幕未恢复: %+v %v", got, err)
			}
			next = m.Settings()
			next.DashboardLayout = "horizontal"
			if _, err = m.Save(next); err == nil {
				t.Fatal("接受了非产品选项")
			}
		})
	}
}

func TestLegacySettingsKeepCarPlayEnabledAndInternetPriority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"revision":7,"network":"wifi","quality":720,"fps":30,"scale":100}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := New(path)
	if err != nil || !m.Settings().Enabled || m.Settings().RecoveryPriority != "internet" {
		t.Fatalf("升级改变原有行为: %v", err)
	}
	next := m.Settings()
	next.Enabled, next.RecoveryPriority = false, "carplay"
	if _, err := m.Save(next); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(content, &saved); err != nil {
		t.Fatal(err)
	}
	if _, exists := saved["scale"]; exists {
		t.Fatal("已删除的缩放值仍被保存")
	}
	reopened, err := New(path)
	if err != nil || reopened.Settings().Enabled || reopened.Settings().RecoveryPriority != "carplay" {
		t.Fatalf("关闭或优先级未持久化: %v", err)
	}
	next = reopened.Settings()
	next.RecoveryPriority = "unknown"
	if _, err := reopened.Save(next); err == nil {
		t.Fatal("未知策略被保存")
	}
}

func TestSavePersistsRevisionAndRejectsStaleOrInvalidSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	m, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	old := m.Settings()
	next := old
	next.Quality = 1080
	next.FPS = 60
	saved, err := m.Save(next)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 2 {
		t.Fatal(saved)
	}
	if _, err = m.Save(old); err == nil {
		t.Fatal("旧版本覆盖未被拒绝")
	}
	loaded, err := New(path)
	if err != nil || loaded.Settings() != saved {
		t.Fatalf("持久化失败: %v", err)
	}
	bad := saved
	bad.FPS = 45
	if _, err = m.Save(bad); err == nil {
		t.Fatal("无效帧率未被拒绝")
	}
	if m.Settings() != saved {
		t.Fatal("保存失败改变了内存策略")
	}
}

func TestDetectedDisplayRejectsOldRevisionAndOutOfBounds(t *testing.T) {
	m, err := New(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	display, err := m.ApplyDisplay(1, 1280, 532)
	if err != nil || display.Height != 532 || display.FPS != 30 {
		t.Fatalf("比例尺寸保存失败: %v", err)
	}
	for _, size := range [][2]int{{1920, 1080}, {1281, 720}, {1280, 200}} {
		if _, err = m.ApplyDisplay(1, size[0], size[1]); err == nil {
			t.Fatal("超出当前画质尺寸未被拒绝")
		}
	}
	next := m.Settings()
	next.FPS = 60
	if _, err = m.Save(next); err != nil {
		t.Fatal(err)
	}
	if _, err = m.ApplyDisplay(1, 1280, 720); err == nil {
		t.Fatal("旧版本检测未被拒绝")
	}
	if got, err := m.ApplyDisplay(2, 1280, 720); err != nil || got.FPS != 60 {
		t.Fatalf("新帧率未应用: %v", err)
	}
}

func TestPhoneSelectionPersistsAndRejectsAddressInsteadOfIdentity(t *testing.T) {
	manager, err := New(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	next := manager.Settings()
	if next.PhoneID != "" {
		t.Fatal("首次安装不应猜测默认手机")
	}
	next.PhoneID = "02:00:00:00:00:01"
	if _, err := manager.Save(next); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(manager.path)
	if err != nil || reopened.Settings().PhoneID != next.PhoneID {
		t.Fatalf("手机选择未持久化: %v", err)
	}
	for _, invalid := range []string{"192.168.88.151", "02:00:00:00:00:0a", "*"} {
		draft := reopened.Settings()
		draft.PhoneID = invalid
		if _, err := reopened.Save(draft); err == nil {
			t.Fatalf("接受了无效手机身份 %q", invalid)
		}
	}
	draft := reopened.Settings()
	draft.PhoneID = ""
	if _, err := reopened.Save(draft); err != nil {
		t.Fatal(err)
	}
}
