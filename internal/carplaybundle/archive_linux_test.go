//go:build linux

package carplaybundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestExternalProductRuntimePreflight(t *testing.T) {
	root := os.Getenv("TESLABOX_TEST_RUNTIME_ROOT")
	if root == "" {
		t.Skip("完整公开候选的目标板预检需要显式隔离目录")
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		t.Fatal("预检必须使用明确绝对路径")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	started := time.Now()
	if err := preflightRuntime(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Name() == "__pycache__" {
			t.Errorf("预检污染公开版本目录: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("完整公开依赖预检耗时 %s", time.Since(started))
}

func TestStageArchiveReusesOnlyIntactVersion(t *testing.T) {
	data := sampleArchive(t, nil, nil)
	sum := sha256.Sum256(data)
	parent := t.TempDir()
	first, err := stageArchive(bytes.NewReader(data), hex.EncodeToString(sum[:]), parent)
	if err != nil {
		t.Fatal(err)
	}
	second, err := stageArchive(bytes.NewReader(data), hex.EncodeToString(sum[:]), parent)
	if err != nil || second != first {
		t.Fatalf("reuse: %s %v", second, err)
	}
	if err := os.WriteFile(filepath.Join(first, "runtime/bin/node"), []byte("broken"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := stageArchive(bytes.NewReader(data), hex.EncodeToString(sum[:]), parent); err == nil {
		t.Fatal("corrupt installed version accepted")
	}
}

func TestStageArchiveRejectsManifestAndMemberChangedTogether(t *testing.T) {
	data := sampleArchive(t, nil, nil)
	sum := sha256.Sum256(data)
	parent := t.TempDir()
	version, err := stageArchive(bytes.NewReader(data), hex.EncodeToString(sum[:]), parent)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(version, "PACKAGE.json")
	stored, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var info manifest
	if err := json.Unmarshal(stored, &info); err != nil {
		t.Fatal(err)
	}
	changed := []byte("new-node")
	newSum := sha256.Sum256(changed)
	for index := range info.Files {
		if info.Files[index].Role == "node" {
			info.Files[index].SHA256 = hex.EncodeToString(newSum[:])
			info.Files[index].Size = int64(len(changed))
		}
	}
	stored, err = json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, stored, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(version, "runtime/bin/node"), changed, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := stageArchive(bytes.NewReader(data), hex.EncodeToString(sum[:]), parent); err == nil {
		t.Fatal("unbound installed manifest accepted")
	}
}

func TestStageArchiveKeepsDeclaredModesWithPrivateUmask(t *testing.T) {
	previous := syscall.Umask(0077)
	defer syscall.Umask(previous)
	data := sampleArchive(t, nil, nil)
	sum := sha256.Sum256(data)
	version, err := stageArchive(bytes.NewReader(data), hex.EncodeToString(sum[:]), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{"teslabox-carplay": 0755, "python/carplay-service.py": 0644} {
		info, err := os.Stat(filepath.Join(version, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("mode=%o", info.Mode().Perm())
		}
	}
}

func TestStageArchiveRejectsOtherWritableParentBeforeExtracting(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0777); err != nil {
		t.Fatal(err)
	}
	data := sampleArchive(t, nil, nil)
	sum := sha256.Sum256(data)
	if _, err := stageArchive(bytes.NewReader(data), hex.EncodeToString(sum[:]), parent); err == nil {
		t.Fatal("其他用户可写入的版本目录被接受")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("不可信根已写入成员: %v", err)
	}
}

func TestStageArchiveRejectsStickyInstallationRoot(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, os.ModeSticky|0777); err != nil {
		t.Fatal(err)
	}
	data := sampleArchive(t, nil, nil)
	sum := sha256.Sum256(data)
	if _, err := stageArchive(bytes.NewReader(data), hex.EncodeToString(sum[:]), parent); err == nil {
		t.Fatal("实际安装根借用粘滞位放行")
	}
}

func TestStageArchiveRejectsWritableExistingVersionOrLinkedMember(t *testing.T) {
	data := sampleArchive(t, nil, nil)
	sum := sha256.Sum256(data)
	expected := hex.EncodeToString(sum[:])
	parent := t.TempDir()
	version, err := stageArchive(bytes.NewReader(data), expected, parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(version, os.ModeSticky|0777); err != nil {
		t.Fatal(err)
	}
	if _, err := stageArchive(bytes.NewReader(data), expected, parent); err == nil {
		t.Fatal("可被其他用户写入的已有版本被接受")
	}
	if err := os.Chmod(version, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(version, "runtime/bin/node"), filepath.Join(parent, "outside-node")); err != nil {
		t.Fatal(err)
	}
	if _, err := stageArchive(bytes.NewReader(data), expected, parent); err == nil {
		t.Fatal("外部硬链接仍可更改已校验成员")
	}
}

func TestStageArchiveRejectsOtherOwnedVersionInRootInstaller(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("所有者替换反例需要目标板 root 隔离验证")
	}
	data := sampleArchive(t, nil, nil)
	sum := sha256.Sum256(data)
	expected := hex.EncodeToString(sum[:])
	parent := t.TempDir()
	version, err := stageArchive(bytes.NewReader(data), expected, parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(version, 65534, -1); err != nil {
		t.Fatal(err)
	}
	defer os.Chown(version, 0, -1)
	if _, err := stageArchive(bytes.NewReader(data), expected, parent); err == nil {
		t.Fatal("root 安装接受了其他用户拥有的版本")
	}
}
