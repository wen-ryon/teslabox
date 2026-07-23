package updater

import (
	"os"
	"path/filepath"
	"testing"
)

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
