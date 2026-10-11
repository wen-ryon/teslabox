package mediabundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func versionFixture(t *testing.T, parent, value string) (string, string, []byte) {
	t.Helper()
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"bin/ffmpeg", "lib/libtest.so.1"} {
		if err := tw.WriteHeader(&tar.Header{Name: mediaPrefix + name, Mode: 0755, Size: int64(len(value)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: mediaPrefix + "lib/libtest.so", Mode: 0777, Linkname: "libtest.so.1", Typeflag: tar.TypeSymlink}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data.Bytes())
	digest := hex.EncodeToString(hash[:])
	path := filepath.Join(parent, digest+".tar.gz")
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path, digest, data.Bytes()
}
func mediaTransactionHost(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("媒体固定路径原子链接替换需要 Linux；由隔离 Linux 回归执行")
	}
}
func TestMediaVersionsRollbackAndLegacyRetention(t *testing.T) {
	mediaTransactionHost(t)
	parent := t.TempDir()
	ctx := context.Background()
	ok := func(context.Context, string) error { return nil }
	a, ah, ab := versionFixture(t, parent, "old-runtime")
	b, bh, _ := versionFixture(t, parent, "new-runtime")
	// 真实旧安装器先创建目录，再由独立版本安装迁移，不能删除旧目录。
	if err := installWithCheck(ctx, bytes.NewReader(ab), ah, parent, ok); err != nil {
		t.Fatal(err)
	}
	if err := installVersion(ctx, b, bh, parent, ok); err != nil {
		t.Fatal(err)
	}
	legacy, _ := filepath.Glob(filepath.Join(parent, "media-versions", "legacy-*"))
	if len(legacy) != 1 {
		t.Fatalf("旧媒体未保留: %v", legacy)
	}
	if got, _ := os.ReadFile(filepath.Join(legacy[0], "bin", "ffmpeg")); string(got) != "old-runtime" {
		t.Fatal("旧安装内容变化")
	}
	if err := installVersion(ctx, a, ah, parent, ok); err != nil {
		t.Fatal(err)
	}
	if err := installVersion(ctx, b, bh, parent, ok); err != nil {
		t.Fatal(err)
	}
	if err := installVersion(ctx, a, ah, parent, ok); err != nil {
		t.Fatal(err)
	}
	selected, _ := os.Readlink(filepath.Join(parent, "media"))
	if selected != filepath.Join(parent, "media-versions", ah) {
		t.Fatal("离线回退未选中旧媒体")
	}
	// 回退到旧 main 的实际旧安装事务，成功后版本目录也必须保留。
	if err := installWithCheck(ctx, bytes.NewReader(ab), ah, parent, ok); err != nil {
		t.Fatal(err)
	}
	active, err := os.Lstat(filepath.Join(parent, "media"))
	if err != nil || !active.IsDir() {
		t.Fatal("旧安装器未恢复实体目录", err)
	}
	for _, h := range []string{ah, bh} {
		if _, err := os.Stat(filepath.Join(parent, "media-versions", h, "bin", "ffmpeg")); err != nil {
			t.Fatal("回退丢失版本", err)
		}
	}
	if err := installVersion(ctx, b, bh, parent, ok); err != nil {
		t.Fatal("重新升级失败", err)
	}
}
func TestMediaFailedPreflightPreservesActive(t *testing.T) {
	mediaTransactionHost(t)
	parent := t.TempDir()
	ctx := context.Background()
	a, ah, _ := versionFixture(t, parent, "a")
	b, bh, _ := versionFixture(t, parent, "b")
	ok := func(context.Context, string) error { return nil }
	if err := installVersion(ctx, a, ah, parent, ok); err != nil {
		t.Fatal(err)
	}
	if err := installVersion(ctx, b, bh, parent, func(context.Context, string) error { return errors.New("failed") }); err == nil {
		t.Fatal("自检失败仍启用")
	}
	selected, _ := os.Readlink(filepath.Join(parent, "media"))
	if selected != filepath.Join(parent, "media-versions", ah) {
		t.Fatal("失败更改了当前媒体")
	}
	if _, err := os.Stat(filepath.Join(parent, "media-versions", bh)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("失败版本被提交", err)
	}
}
func TestMediaExistingVersionRejectsChangedLibraryAlias(t *testing.T) {
	mediaTransactionHost(t)
	parent := t.TempDir()
	a, ah, _ := versionFixture(t, parent, "a")
	ctx := context.Background()
	ok := func(context.Context, string) error { return nil }
	if err := installVersion(ctx, a, ah, parent, ok); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "media-versions", ah, "lib", "libtest.so")
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("different.so", alias); err != nil {
		t.Fatal(err)
	}
	if err := installVersion(ctx, a, ah, parent, ok); err == nil {
		t.Fatal("篡改库别名被接受")
	}
}
