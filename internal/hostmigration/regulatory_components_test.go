package hostmigration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegulatoryAssetsMatchFirstInstall(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, dir string }{{"teslabox-wireless-region.sh", "scripts"}, {"teslabox-wireless-region.service", "systemd"}} {
		t.Run(tc.name, func(t *testing.T) {
			actual, err := managedRegulatoryAssets.ReadFile("assets/" + tc.name)
			if err != nil {
				t.Fatal(err)
			}
			first, err := os.ReadFile(filepath.Join("..", "..", "deploy", tc.dir, tc.name))
			if err != nil {
				t.Fatal(err)
			}
			if string(actual) != string(first) {
				t.Fatal("OTA and first-install assets differ")
			}
		})
	}
	unit, _ := managedRegulatoryAssets.ReadFile("assets/teslabox-wireless-region.service")
	if !strings.Contains(string(unit), "Before=NetworkManager.service teslabox-ap.service") {
		t.Fatal("region must precede network startup")
	}
	installer, err := os.ReadFile(filepath.Join("..", "..", "deploy", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	initialize := strings.Index(string(installer), "/usr/local/bin/teslabox wireless-region-init")
	startAP := strings.Index(string(installer), "systemctl restart teslabox-ap.service")
	if initialize < 0 || startAP < 0 || initialize > startAP {
		t.Fatal("first install starts AP before region initialization")
	}
}

func TestEnsureRegulatoryComponentsForOldOTAIsIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	paths := RegulatoryComponentPaths{Script: filepath.Join(dir, "region.sh"), Unit: filepath.Join(dir, "region.service")}
	r := &recordingRunner{}
	changed, err := EnsureRegulatoryComponents(t.Context(), paths, r)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	first, _ := os.ReadFile(paths.Script)
	changed, err = EnsureRegulatoryComponents(t.Context(), paths, r)
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	second, _ := os.ReadFile(paths.Script)
	if string(first) != string(second) {
		t.Fatal("repeat migration overwrote component")
	}
	if len(r.calls) != 3 || r.calls[0] != "systemctl daemon-reload" || r.calls[1] != "systemctl enable teslabox-wireless-region.service" {
		t.Fatalf("calls=%v", r.calls)
	}
	for _, call := range r.calls {
		if strings.Contains(call, "restart") || strings.Contains(call, "start ") {
			t.Fatal("OTA migration must not restart user network")
		}
	}
}

func TestEnsureRegulatoryComponentsKeepsAdministratorFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	paths := RegulatoryComponentPaths{Script: filepath.Join(dir, "region.sh"), Unit: filepath.Join(dir, "region.service")}
	custom := []byte("#!/bin/sh\n# custom country policy\n")
	if err := os.WriteFile(paths.Script, custom, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &recordingRunner{}
	if _, err := EnsureRegulatoryComponents(t.Context(), paths, r); err == nil {
		t.Fatal("custom component must be reported")
	}
	got, _ := os.ReadFile(paths.Script)
	if string(got) != string(custom) || len(r.calls) != 0 {
		t.Fatal("administrator script changed or was enabled")
	}
}
