package recordingcache

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSourceChangeRecordsExactMetadataDifference(t *testing.T) {
	for _, kind := range []string{"size", "mtime", "identity"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "identity" && runtime.GOOS == "windows" {
				t.Skip("Windows 的路径 FileInfo 延迟读取文件标识；身份替换由 Linux 板端验证")
			}
			source := sourceFile(t, []byte("original"))
			before, err := os.Stat(source.Path)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "size":
				err = os.WriteFile(source.Path, []byte("longer replacement"), 0o600)
			case "mtime":
				err = os.Chtimes(source.Path, before.ModTime(), before.ModTime().Add(2*time.Second))
			case "identity":
				other := source.Path + ".other"
				if err = os.WriteFile(other, []byte("original"), 0o600); err == nil {
					err = os.Rename(source.Path, source.Path+".old")
				}
				if err == nil {
					err = os.Rename(other, source.Path)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			err = sourceStage(source, "after_hash", unchanged(source.Path, before))
			change, ok := errors.AsType[*SourceChangeError](err)
			if !ok || !errors.Is(err, ErrSourceChanged) || change.Stage != "after_hash" || change.Version != source.Version {
				t.Fatalf("missing source evidence: %v", err)
			}
			if kind == "size" && !change.SizeChanged || kind == "mtime" && !change.ModTimeChanged || kind == "identity" && !change.IdentityChanged {
				t.Fatalf("wrong difference: %+v", change)
			}
			for _, key := range []string{"stage=after_hash", "snapshot=snapshot-1", "before=", "after=", "identity_changed=", "mtime_changed="} {
				if !strings.Contains(err.Error(), key) {
					t.Fatalf("missing %s: %v", key, err)
				}
			}
		})
	}
}

func TestChangedContentWithPreservedMetadataIsRejected(t *testing.T) {
	s := newTestStore(t, testConfig(t))
	source := sourceFile(t, []byte("AAAA"))
	info, err := os.Stat(source.Path)
	if err != nil {
		t.Fatal(err)
	}
	key, err := hashFile(context.Background(), source.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(source.Path, []byte("BBBB"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(source.Path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	err = s.copyFile(context.Background(), plannedFile{file: source, info: info, key: key}, nil)
	change, ok := errors.AsType[*SourceChangeError](err)
	if !ok || !change.ContentChanged || change.ExpectedDigest == change.ActualDigest || change.Stage != "copy_content" {
		t.Fatalf("content change accepted or unclassified: %v", err)
	}
	entries, err := os.ReadDir(s.objectDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("unverified copy was published: %v %v", entries, err)
	}
}
