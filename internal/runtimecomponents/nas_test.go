package runtimecomponents

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nasFixture(t *testing.T, value, version, invalid string) (Component, string) {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	prefix := "rclone-v" + version + "-linux-arm64/"
	names := []string{prefix, prefix + "rclone", prefix + "README.txt", prefix + "README.html", prefix + "rclone.1", prefix + "git-log.txt"}
	if invalid == "duplicate" {
		names[5] = names[4]
	}
	if invalid == "escape" {
		names[5] = "../escape"
	}
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		if strings.HasSuffix(name, "/") {
			header.SetMode(os.ModeDir | 0755)
		} else {
			header.SetMode(0644)
		}
		if invalid == "link" && name == prefix+"rclone" {
			header.SetMode(os.ModeSymlink | 0777)
		}
		f, err := z.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(name, "/") {
			if _, err = f.Write([]byte(value)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	c := componentFor(b.Bytes(), "https://example.invalid/fixed.zip")
	c.Version = version
	p := filepath.Join(t.TempDir(), "input.zip")
	if err := os.WriteFile(p, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return c, p
}
func TestNASVersionReuseRollbackAndPreflightFailure(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "versions")
	ctx := context.Background()
	ok := func(context.Context, string, string) error { return nil }
	a, ap := nasFixture(t, "old", "1.75.1", "")
	b, bp := nasFixture(t, "new", "1.75.2", "")
	for _, v := range []struct {
		c Component
		p string
	}{{a, ap}, {b, bp}, {a, ap}} {
		if err := installNAS(ctx, v.c, v.p, parent, ok); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []Component{a, b} {
		if _, err := os.Stat(NASExecutable(parent, c)); err != nil {
			t.Fatal("丢失旧工具", err)
		}
	}
	newer, np := nasFixture(t, "bad", "1.75.3", "")
	if err := installNAS(ctx, newer, np, parent, func(context.Context, string, string) error { return errors.New("failed") }); err == nil {
		t.Fatal("自检失败仍提交")
	}
	if _, err := os.Stat(NASExecutable(parent, newer)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(NASExecutable(parent, a)); err != nil || string(data) != "old" {
		t.Fatal("原工具被覆盖", err)
	}
	if err := os.WriteFile(NASExecutable(parent, a), []byte("xxx"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := installNAS(ctx, a, ap, parent, ok); err == nil {
		t.Fatal("已安装内容篡改被接受")
	}
}
func TestNASRejectsUnsafeZipBeforePreflight(t *testing.T) {
	for _, bad := range []string{"duplicate", "escape", "link"} {
		t.Run(bad, func(t *testing.T) {
			c, p := nasFixture(t, "x", "1.75.1", bad)
			called := false
			err := installNAS(context.Background(), c, p, filepath.Join(t.TempDir(), "versions"), func(context.Context, string, string) error { called = true; return nil })
			if err == nil || called {
				t.Fatalf("err=%v preflight=%v", err, called)
			}
		})
	}
}
func TestNASCancelledInstallDoesNotCommit(t *testing.T) {
	c, p := nasFixture(t, "x", "1.75.1", "")
	parent := filepath.Join(t.TempDir(), "versions")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := installNAS(ctx, c, p, parent, func(context.Context, string, string) error { return nil }); err == nil {
		t.Fatal("取消仍成功")
	}
	if _, err := os.Stat(NASExecutable(parent, c)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
