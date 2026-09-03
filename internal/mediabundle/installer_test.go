package mediabundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
)

func TestValidateFsckVersionResultAcceptsExfatprogsVersionExit(t *testing.T) {
	if err := validateFsckVersionResult("exfatprogs version : 1.2.0\n", 16); err != nil {
		t.Fatalf("validateFsckVersionResult() error = %v", err)
	}
}

func TestValidateFsckVersionResultRejectsUnexpectedExit(t *testing.T) {
	if err := validateFsckVersionResult("exfatprogs version : 1.2.0\n", 1); err == nil {
		t.Fatal("validateFsckVersionResult() 未拒绝异常退出码")
	}
}

func TestValidateFsckVersionResultRejectsWrongVersion(t *testing.T) {
	if err := validateFsckVersionResult("exfatprogs version : 1.1.0\n", 16); err == nil {
		t.Fatal("validateFsckVersionResult() 未拒绝错误版本")
	}
}

func TestExtractArchiveRejectsPathEscape(t *testing.T) {
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "opt/teslabox/media/../../../etc/passwd", Mode: 0o644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractArchive(bytes.NewReader(buffer.Bytes()), t.TempDir()); err == nil {
		t.Fatal("extractArchive() 未拒绝路径逃逸")
	}
}

func TestExtractArchiveRejectsSymbolicLink(t *testing.T) {
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "opt/teslabox/media/bin/ffmpeg", Typeflag: tar.TypeSymlink, Linkname: "/bin/sh"}); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractArchive(bytes.NewReader(buffer.Bytes()), t.TempDir()); err == nil {
		t.Fatal("extractArchive() 未拒绝符号链接")
	}
}
