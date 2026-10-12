package updater

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func transactionFixture(t *testing.T, stateExists bool) (string, []string, *updateTransaction) {
	t.Helper()
	root := t.TempDir()
	targets := []string{filepath.Join(root, "program"), filepath.Join(root, "export-state.json")}
	if err := os.WriteFile(targets[0], []byte("old executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if stateExists {
		if err := os.WriteFile(targets[1], []byte(`{"version":1,"settings":{"retain_count":3},"tasks":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(root, "transaction")
	tx, err := prepareTransaction(dir, targets)
	if err != nil {
		t.Fatal(err)
	}
	return dir, targets, tx
}

func TestTransactionRestoresProgramAndOldState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		exists bool
	}{{"existing", true}, {"absent", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, targets, tx := transactionFixture(t, tc.exists)
			if err := os.WriteFile(targets[0], []byte("new executable"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(targets[1], []byte(`{"version":2,"tasks":[]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := tx.restore(dir); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(targets[0])
			if err != nil || string(data) != "old executable" {
				t.Fatalf("program: %s %v", data, err)
			}
			data, err = os.ReadFile(targets[1])
			if tc.exists {
				var old struct {
					Version int `json:"version"`
				}
				if err != nil || json.Unmarshal(data, &old) != nil || old.Version != 1 {
					t.Fatalf("old reader cannot load: %s %v", data, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("original absence not restored: %v", err)
			}
			failed, err := os.ReadFile(filepath.Join(dir, "failed-1"))
			if err != nil || string(failed) != `{"version":2,"tasks":[]}` {
				t.Fatalf("failed evidence: %s %v", failed, err)
			}
			loaded, err := readTransaction(dir, targets)
			if err != nil || loaded.Phase != "rolled_back" {
				t.Fatalf("journal: %+v %v", loaded, err)
			}
		})
	}
}

func TestTransactionResumesPartialRestore(t *testing.T) {
	dir, targets, tx := transactionFixture(t, true)
	for i, path := range targets {
		if err := os.WriteFile(path, []byte("candidate"), 0o600); err != nil {
			t.Fatal(err)
		}
		state, data, err := snapshotFile(path, 128<<20)
		if err != nil {
			t.Fatal(err)
		}
		tx.Files[i].Failed = &state
		if err = durableFile(filepath.Join(dir, "failed-"+string(rune('0'+i))), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tx.Phase = "restoring"
	if err := tx.save(dir); err != nil {
		t.Fatal(err)
	}
	// 模拟状态已经恢复、程序尚未恢复时断电。
	data, err := os.ReadFile(filepath.Join(dir, "before-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err = durableFile(targets[1], data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := readTransaction(dir, targets)
	if err != nil {
		t.Fatal(err)
	}
	if err = loaded.restore(dir); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(targets[0])
	if string(data) != "old executable" {
		t.Fatalf("program: %s", data)
	}
}

func TestTransactionRefusesCorruptionAndConcurrentChange(t *testing.T) {
	for _, name := range []string{"corrupt backup", "unrecorded write", "changed configuration", "committed"} {
		t.Run(name, func(t *testing.T) {
			dir, targets, tx := transactionFixture(t, true)
			switch name {
			case "corrupt backup":
				if err := os.WriteFile(filepath.Join(dir, "before-1"), []byte("bad"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unrecorded write":
				state, _, _ := snapshotFile(targets[1], 16<<20)
				tx.Files[1].Failed = &state
				if err := os.WriteFile(targets[1], []byte("user update"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "changed configuration":
				_, err := readTransaction(dir, []string{targets[0], filepath.Join(filepath.Dir(targets[1]), "other")})
				if err == nil {
					t.Fatal("accepted changed target")
				}
				return
			case "committed":
				tx.Phase = "committed"
			}
			if err := tx.restore(dir); err == nil {
				t.Fatal("unsafe restore accepted")
			}
			if name == "unrecorded write" {
				data, _ := os.ReadFile(targets[1])
				if string(data) != "user update" {
					t.Fatal("overwrote concurrent data")
				}
			}
		})
	}
}

func TestTransactionRejectsLinkedPaths(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.WriteFile(real, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, _, err := snapshotFile(link, 16<<20); err == nil {
		t.Fatal("accepted symlink")
	}
	if err := durableFile(link, []byte("changed"), 0o600); err == nil {
		t.Fatal("replaced symlink")
	}
	data, _ := os.ReadFile(real)
	if string(data) != "preserve" {
		t.Fatal("changed link target")
	}
}

func TestTransactionResumesEveryUncommittedPhase(t *testing.T) {
	for _, phase := range []string{"prepared", "activating", "restoring"} {
		t.Run(phase, func(t *testing.T) {
			dir, targets, tx := transactionFixture(t, true)
			tx.Phase = phase
			if err := tx.save(dir); err != nil {
				t.Fatal(err)
			}
			if phase != "prepared" {
				if err := os.WriteFile(targets[0], []byte("candidate"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(targets[1], []byte(`{"version":2}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			loaded, err := readTransaction(dir, targets)
			if err != nil {
				t.Fatal(err)
			}
			if err = loaded.restore(dir); err != nil {
				t.Fatal(err)
			}
			program, _ := os.ReadFile(targets[0])
			if string(program) != "old executable" {
				t.Fatal("旧程序未恢复")
			}
			if err = loaded.restore(dir); err != nil {
				t.Fatal("恢复不幂等", err)
			}
		})
	}
}
