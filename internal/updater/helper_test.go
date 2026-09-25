package updater

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadInstallRequestAcceptsRCVersion(t *testing.T) {
	for _, test := range []struct {
		name    string
		version string
		wantErr bool
	}{
		{name: "rc", version: "0.1.31-rc.1"},
		{name: "stable", version: "0.1.31"},
		{name: "invalid suffix", version: "0.1.31-rc.0", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := InstallRequest{
				Version: test.version, PackageName: "teslabox-linux-arm64",
				SHA256: strings.Repeat("a", 64), Size: 4096,
			}
			content, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "request.json")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = readInstallRequest(path)
			if (err != nil) != test.wantErr {
				t.Fatalf("readInstallRequest() error = %v, want error %v", err, test.wantErr)
			}
		})
	}
}

func TestCleanupVersionBackupsKeepsOnlyLatestBackup(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{
		latestBackupName,
		"teslabox-0.1.0.backup",
		"teslabox-0.1.1.backup",
		"operator-notes.txt",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := cleanupVersionBackups(directory); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{latestBackupName, "operator-notes.txt"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Fatalf("%s should remain: %v", name, err)
		}
	}
	for _, name := range []string{"teslabox-0.1.0.backup", "teslabox-0.1.1.backup"} {
		if _, err := os.Stat(filepath.Join(directory, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed, stat error = %v", name, err)
		}
	}
}
