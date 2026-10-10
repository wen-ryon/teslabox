package carplaybundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func sampleArchive(t *testing.T, mutate func(*manifest), extra *tar.Header) []byte {
	t.Helper()
	contents := map[string][]byte{
		"teslabox-carplay": []byte("native"), "runtime/bin/node": []byte("node"),
		"python/carplay-service.py": []byte("service"), "systemd/teslabox-carplay-experiment.service": []byte("unit"),
		"licenses/COPYING": []byte("license"),
	}
	info := manifest{Schema: 1, Kind: "inner", Version: "0.2.0-rc.1"}
	for role, name := range map[string]string{"native": "teslabox-carplay", "node": "runtime/bin/node", "service": "python/carplay-service.py", "unit": "systemd/teslabox-carplay-experiment.service", "license": "licenses/COPYING"} {
		digest := sha256.Sum256(contents[name])
		mode := int64(0644)
		if role == "native" || role == "node" {
			mode = 0755
		}
		info.Files = append(info.Files, member{Path: name, Size: int64(len(contents[name])), Mode: mode, Role: role, SHA256: hex.EncodeToString(digest[:])})
	}
	if mutate != nil {
		mutate(&info)
	}
	var result bytes.Buffer
	gz := gzip.NewWriter(&result)
	tw := tar.NewWriter(gz)
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "PACKAGE.json", Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	for name, data := range contents {
		mode := int64(0644)
		if name == "teslabox-carplay" || name == "runtime/bin/node" {
			mode = 0755
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if extra != nil {
		if err := tw.WriteHeader(extra); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func TestExtractArchiveRejectsUnsafeOrMismatchedPackages(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*manifest)
		extra  *tar.Header
	}{
		{name: "wrong-envelope", mutate: func(m *manifest) { m.Kind = "full" }},
		{name: "wrong-member-hash", mutate: func(m *manifest) { m.Files[0].SHA256 = string(bytes.Repeat([]byte("a"), 64)) }},
		{name: "main-role", mutate: func(m *manifest) { m.Files[0].Role = "main" }},
		{name: "traversal", extra: &tar.Header{Name: "../escaped", Typeflag: tar.TypeReg, Mode: 0644}},
		{name: "private-key", extra: &tar.Header{Name: "MFI/IDENTITY.PK8", Typeflag: tar.TypeReg, Mode: 0600}},
		{name: "symlink", extra: &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc", Mode: 0755}},
		{name: "duplicate", extra: &tar.Header{Name: "PACKAGE.json", Typeflag: tar.TypeReg, Mode: 0644}},
		{name: "unlisted", extra: &tar.Header{Name: "extra", Typeflag: tar.TypeReg, Mode: 0644}},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := sampleArchive(t, test.mutate, test.extra)
			if _, err := extractArchive(bytes.NewReader(data), t.TempDir()); err == nil {
				t.Fatal("unsafe package accepted")
			}
		})
	}
}

func TestExtractArchiveAcceptsCompletePackage(t *testing.T) {
	info, err := extractArchive(bytes.NewReader(sampleArchive(t, nil, nil)), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "0.2.0-rc.1" {
		t.Fatal(info.Version)
	}
}

func TestStageArchiveChecksWholeArchiveDigest(t *testing.T) {
	data := sampleArchive(t, nil, nil)
	if _, err := stageArchive(bytes.NewReader(data), string(bytes.Repeat([]byte("a"), 64)), t.TempDir()); err == nil {
		t.Fatal("bad digest accepted")
	}
}

func TestMemberNameRejectsPrivateAndOuterFiles(t *testing.T) {
	for _, name := range []string{"service.json", "active.json", "keys/a.pem", "identity.pk8", "teslabox", "teslabox-update-helper", "latest.json", "reference-livi-private/data", "build/mfi-offline/x", "./x", "a\\b", "a/../b"} {
		t.Run(name, func(t *testing.T) {
			if memberName(name) == nil {
				t.Fatal("unsafe name accepted")
			}
		})
	}
}

func TestStageArchiveRejectsLinkedParent(t *testing.T) {
	parent := t.TempDir()
	linked := filepath.Join(parent, "linked")
	if err := os.Symlink(t.TempDir(), linked); err != nil {
		t.Skip(err)
	}
	data := sampleArchive(t, nil, nil)
	sum := sha256.Sum256(data)
	if _, err := stageArchive(bytes.NewReader(data), hex.EncodeToString(sum[:]), linked); err == nil {
		t.Fatal("linked parent accepted")
	}
}
