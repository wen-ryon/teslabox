package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunHelperGuardAppliesToKernelAndOrdinaryVersions(t *testing.T) {
	for _, version := range []string{KernelUpgradeVersion, "0.1.31"} {
		t.Run(version, func(t *testing.T) {
			root := t.TempDir()
			data := capabilityELF(t, ProgramTransactionCapability, false, "")
			sum := sha256.Sum256(data)
			request := InstallRequest{Version: version, PackageName: "candidate", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			for name, content := range map[string][]byte{"candidate": data, "request.json": body, "installed": []byte("preserve")} {
				if err := os.WriteFile(filepath.Join(root, name), content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			config := HelperConfig{RequestPath: filepath.Join(root, "request.json"), ResultPath: filepath.Join(root, "result.json"),
				DownloadDir: root, BackupDir: filepath.Join(root, "backups"), TargetPath: filepath.Join(root, "installed"), Service: "unprotected.service"}
			err = RunHelper(t.Context(), config)
			if err == nil || !strings.Contains(err.Error(), "安装恢复守卫") {
				t.Fatalf("必须在安装前拒绝无保护服务: %v", err)
			}
			if _, err := os.Stat(config.BackupDir); !os.IsNotExist(err) {
				t.Fatal("守卫失败不应创建备份或进入内核事务")
			}
			got, err := os.ReadFile(config.TargetPath)
			if err != nil || string(got) != "preserve" {
				t.Fatal("守卫失败不得替换主程序")
			}
		})
	}
}
