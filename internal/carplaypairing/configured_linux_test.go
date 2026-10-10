//go:build linux

package carplaypairing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func configuredFixture(t *testing.T) (string, string) {
	t.Helper()
	root := privateDirectory(t)
	binary := filepath.Join(root, "teslabox-carplay")
	body := []byte{0x7f, 'E', 'L', 'F', 0}
	if err := os.WriteFile(binary, body, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	config, _ := json.Marshal(map[string]string{"root": root, "binary_sha256": hex.EncodeToString(sum[:])})
	path := filepath.Join(root, "service.json")
	if err := os.WriteFile(path, config, 0600); err != nil {
		t.Fatal(err)
	}
	return path, binary
}

func TestConfiguredManagerBecomesReadyAfterColdServiceStartup(t *testing.T) {
	path, _ := configuredFixture(t)
	runtime := privateDirectory(t)
	m, err := NewConfiguredProvisioningManager(path, runtime, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if m.Ready() {
		t.Fatal("服务未启动不能报告就绪")
	}
	writeServiceStatus(t, &serviceClient{runtime: runtime}, serviceProvisioning{State: "idle"})
	if !m.Ready() {
		t.Fatal("冷启动服务就绪后应自动可用")
	}
	body, _ := json.Marshal(map[string]any{"running": false, "cleanup_complete": true, "provisioning": serviceProvisioning{State: "idle"}})
	if err := os.WriteFile(filepath.Join(runtime, "status.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	if m.Ready() {
		t.Fatal("服务停止后的清理终态不能启动新登记")
	}
}

func TestConfiguredBinaryRejectsUnsafeMetadata(t *testing.T) {
	for _, change := range []string{"config_mode", "config_link", "config_hardlink", "binary_mode", "binary_link", "not_elf", "bad_digest"} {
		t.Run(change, func(t *testing.T) {
			path, binary := configuredFixture(t)
			switch change {
			case "config_mode":
				_ = os.Chmod(path, 0644)
			case "config_link":
				_ = os.Rename(path, path+".target")
				if err := os.Symlink(path+".target", path); err != nil {
					t.Fatal(err)
				}
			case "config_hardlink":
				if err := os.Link(path, path+".link"); err != nil {
					t.Fatal(err)
				}
			case "binary_mode":
				_ = os.Chmod(binary, 0770)
			case "binary_link":
				_ = os.Rename(binary, binary+".target")
				if err := os.Symlink(binary+".target", binary); err != nil {
					t.Fatal(err)
				}
			case "not_elf":
				_ = os.WriteFile(binary, []byte("script"), 0700)
			case "bad_digest":
				body, _ := json.Marshal(map[string]string{"root": filepath.Dir(binary), "binary_sha256": "bad"})
				_ = os.WriteFile(path, body, 0600)
			}
			if _, _, err := configuredBinary(path); err == nil {
				t.Fatal("危险配置被接受")
			}
		})
	}
}
