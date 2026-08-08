//go:build releasebundle

package mediabundle

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestEmbeddedReleaseArchive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 普通用户不能创建媒体包中的动态库符号链接")
	}
	archive, expectedHash, available, err := openEmbeddedArchive()
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Fatal("正式构建没有内置媒体运行时")
	}
	defer archive.Close()
	if len(expectedHash) != 64 {
		t.Fatalf("内置摘要长度 = %d", len(expectedHash))
	}
	directory := t.TempDir()
	if err := extractArchive(archive, directory); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{
		"bin/ffmpeg",
		"bin/ffprobe",
		"share/fonts/NotoSansCJK-Regular.ttc",
	} {
		info, err := os.Stat(filepath.Join(directory, filepath.FromSlash(relative)))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			t.Fatalf("内置媒体文件 %s 无效", relative)
		}
	}
}
