package hostmigration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestManagedFanAssetsMatchBootstrapSources(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		asset string
		path  string
	}{
		{"assets/teslabox-fan-early.sh", filepath.Join("..", "..", "deploy", "scripts", "teslabox-fan-early.sh")},
		{"assets/teslabox-fan-early.service", filepath.Join("..", "..", "deploy", "systemd", "teslabox-fan-early.service")},
	} {
		embedded, err := managedFanAssets.ReadFile(test.asset)
		if err != nil {
			t.Fatal(err)
		}
		deployment, err := os.ReadFile(test.path)
		if err != nil {
			t.Fatal(err)
		}
		if string(embedded) != string(deployment) {
			t.Fatalf("%s 与首装源文件不一致", test.asset)
		}
	}
}

func TestEnsureFanEarlyComponentsCreatesAndEnablesMissingFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	paths := FanComponentPaths{Script: filepath.Join(root, "teslabox-fan-early.sh"), Unit: filepath.Join(root, "teslabox-fan-early.service")}
	runner := &recordingRunner{}
	changed, err := EnsureFanEarlyComponents(context.Background(), paths, runner)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if len(runner.calls) != 2 || runner.calls[0] != "systemctl daemon-reload" || runner.calls[1] != "systemctl enable teslabox-fan-early.service" {
		t.Fatalf("runner calls = %#v", runner.calls)
	}
	for _, path := range []string{paths.Script, paths.Unit} {
		if info, statErr := os.Stat(path); statErr != nil || !info.Mode().IsRegular() {
			t.Fatalf("component %s: info=%v err=%v", path, info, statErr)
		}
	}
}

func TestEnsureFanEarlyComponentsPreservesExistingCustomization(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	paths := FanComponentPaths{Script: filepath.Join(root, "teslabox-fan-early.sh"), Unit: filepath.Join(root, "teslabox-fan-early.service")}
	for _, path := range []string{paths.Script, paths.Unit} {
		if err := os.WriteFile(path, []byte("custom\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner := &recordingRunner{}
	changed, err := EnsureFanEarlyComponents(context.Background(), paths, runner)
	if err != nil {
		t.Fatal(err)
	}
	if changed || len(runner.calls) != 1 || runner.calls[0] != "systemctl enable teslabox-fan-early.service" {
		t.Fatalf("changed=%v calls=%#v", changed, runner.calls)
	}
}
