package hostmigration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedAPAssetsMatchBootstrapSources(t *testing.T) {
	t.Parallel()
	tests := []struct {
		asset string
		path  string
	}{
		{asset: "assets/teslabox-ap-setup.sh", path: filepath.Join("..", "..", "deploy", "scripts", "teslabox-ap-setup.sh")},
		{asset: "assets/teslabox-ap.service", path: filepath.Join("..", "..", "deploy", "systemd", "teslabox-ap.service")},
	}
	for _, test := range tests {
		embedded, err := managedAPAssets.ReadFile(test.asset)
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

func TestReplaceKnownComponentOnlyReplacesRecognizedRelease(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "teslabox-ap-setup.sh")
	legacy := []byte("#!/bin/sh\nold release\n")
	expected := []byte("#!/bin/sh\nsafe release\n")
	if err := os.WriteFile(path, legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(legacy)
	allowed := map[string]struct{}{hex.EncodeToString(digest[:]): {}}

	changed, err := replaceKnownComponent(path, expected, 0o755, allowed)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("replaceKnownComponent() changed = false")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(expected) {
		t.Fatalf("content = %q", content)
	}
}

func TestUpgradeLegacyAPComponentsStopsExitedUnitWithoutStartingAP(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	scriptPath := filepath.Join(root, "teslabox-ap-setup.sh")
	unitPath := filepath.Join(root, "teslabox-ap.service")
	script, err := managedAPAssets.ReadFile("assets/teslabox-ap-setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	legacyUnit := []byte("[Unit]\nDescription=TeslaBox AP virtual interface setup\nWants=NetworkManager.service\nAfter=NetworkManager.service\n\n[Service]\nType=oneshot\nRemainAfterExit=yes\nEnvironmentFile=-/etc/teslabox/teslabox.env\nExecStart=/usr/local/libexec/teslabox-ap-setup.sh\n\n[Install]\nWantedBy=multi-user.target\n")
	if err := os.WriteFile(scriptPath, script, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, legacyUnit, 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{output: "exited\n"}

	result, err := UpgradeLegacyAPComponents(context.Background(), APComponentPaths{
		Script: scriptPath,
		Unit:   unitPath,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Script || !result.Unit {
		t.Fatalf("result = %#v", result)
	}
	if len(runner.calls) != 3 ||
		runner.calls[0] != "systemctl show teslabox-ap.service --property=SubState --value" ||
		runner.calls[1] != "systemctl stop teslabox-ap.service" ||
		runner.calls[2] != "systemctl daemon-reload" {
		t.Fatalf("runner calls = %#v", runner.calls)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, " start ") || strings.Contains(call, " restart ") {
			t.Fatalf("迁移不应启动热点: %#v", runner.calls)
		}
	}
}

func TestUpgradeLegacyAPComponentsSchedulesProbeAfterKnownScriptReplacement(t *testing.T) {
	root := t.TempDir()
	scriptPath := filepath.Join(root, "teslabox-ap-setup.sh")
	unitPath := filepath.Join(root, "teslabox-ap.service")
	legacyScript := []byte("#!/bin/sh\n# published release fixture\n")
	currentUnit, err := managedAPAssets.ReadFile("assets/teslabox-ap.service")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, legacyScript, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, currentUnit, 0o644); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(legacyScript)
	hash := hex.EncodeToString(digest[:])
	legacyAPScriptHashes[hash] = struct{}{}
	defer delete(legacyAPScriptHashes, hash)

	runner := &recordingRunner{}
	result, err := UpgradeLegacyAPComponents(context.Background(), APComponentPaths{
		Script: scriptPath,
		Unit:   unitPath,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Script || result.Unit {
		t.Fatalf("result = %#v", result)
	}
	if len(runner.calls) != 1 || runner.calls[0] != "systemctl --no-block start teslabox-ap.service" {
		t.Fatalf("runner calls = %#v", runner.calls)
	}
}

func TestManagedAPScriptPreparesDisabledProfileWithoutStartingHotspot(t *testing.T) {
	t.Parallel()
	script, err := managedAPAssets.ReadFile("assets/teslabox-ap-setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	content := string(script)
	prepareGuard := strings.Index(content, `if [ "$AP_AUTO_START" != "true" ]; then`)
	connectionUp := strings.LastIndex(content, `/usr/bin/nmcli --wait 20 connection up`)
	if !strings.Contains(content, "no|false|0) AP_AUTO_START=false") ||
		prepareGuard < 0 ||
		connectionUp < 0 ||
		prepareGuard > connectionUp {
		t.Fatalf("关闭状态必须完成能力探测和接口准备后，在激活热点前退出")
	}
}

func TestPublishedV0112Test1APScriptRemainsMigratable(t *testing.T) {
	t.Parallel()
	const publishedHash = "22d4b3ef82bc4541b0e9f3ee59460d6697435502474ec9da213588a6204f9b7f"
	if _, ok := legacyAPScriptHashes[publishedHash]; !ok {
		t.Fatalf("缺少已发布 v0.1.12-test.1 AP 脚本摘要")
	}
}

func TestUpgradeLegacyAPComponentsPreservesCustomizedFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	scriptPath := filepath.Join(root, "teslabox-ap-setup.sh")
	unitPath := filepath.Join(root, "teslabox-ap.service")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n# administrator customization\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte("[Service]\nExecStart=/opt/custom-ap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}

	result, err := UpgradeLegacyAPComponents(context.Background(), APComponentPaths{
		Script: scriptPath,
		Unit:   unitPath,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Script || result.Unit || len(runner.calls) != 0 {
		t.Fatalf("result = %#v, calls = %#v", result, runner.calls)
	}
}
