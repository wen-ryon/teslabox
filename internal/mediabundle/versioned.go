package mediabundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// EnsureVersionArchive 保留摘要版本和旧安装，固定 RPATH 只在自检通过后原子切换。
func EnsureVersionArchive(ctx context.Context, archive, digest, parent string) error {
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" || os.Geteuid() != 0 {
		return errors.New("媒体产品安装仅支持 root ARM64 Linux")
	}
	return installVersion(ctx, archive, digest, parent, validateRuntime)
}

func installVersion(ctx context.Context, archive, digest, parent string, preflight func(context.Context, string) error) error {
	if len(digest) != 64 || digest != strings.ToLower(digest) {
		return errors.New("媒体组件摘要无效")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return err
	}
	if !filepath.IsAbs(parent) || filepath.Clean(parent) != parent || filepath.Dir(parent) == parent {
		return errors.New("媒体组件目录无效")
	}
	versions := filepath.Join(parent, "media-versions")
	for path := versions; ; path = filepath.Dir(path) {
		info, e := os.Lstat(path)
		if e == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("媒体版本父目录不安全")
			}
			if e = trustedMediaMember(info); e != nil {
				return e
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if path == filepath.Dir(path) {
			break
		}
	}
	if err := os.MkdirAll(versions, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(versions, ".stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	input, err := os.Open(archive)
	if err != nil {
		return err
	}
	hash := sha256.New()
	err = extractArchive(io.TeeReader(mediaContextReader{ctx, input}, hash), stage)
	err = errors.Join(err, input.Close())
	if err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("媒体组件摘要不一致")
	}
	target := filepath.Join(versions, digest)
	if info, e := os.Lstat(target); e == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("媒体版本目录不安全")
		}
		if e = compareMediaTrees(ctx, stage, target); e != nil {
			return e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	} else {
		if err = preflight(ctx, stage); err != nil {
			return err
		}
		if err = syncMediaTree(stage); err != nil {
			return err
		}
		if err = os.Rename(stage, target); err != nil {
			return err
		}
		if err = syncMediaDirectory(versions); err != nil {
			return err
		}
	}
	if err = preflight(ctx, target); err != nil {
		return err
	}
	active := filepath.Join(parent, "media")
	if info, e := os.Lstat(active); e == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			selected, e := os.Readlink(active)
			if e != nil {
				return e
			}
			if !filepath.IsAbs(selected) || filepath.Dir(selected) != versions {
				return errors.New("当前媒体链接越界")
			}
			if selected == target {
				return nil
			}
		} else if !info.IsDir() {
			return errors.New("当前媒体目录不安全")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	temporary := filepath.Join(parent, fmt.Sprintf(".media-select-%d", time.Now().UnixNano()))
	if err = os.Symlink(target, temporary); err != nil {
		return err
	}
	defer os.Remove(temporary)
	legacy := ""
	if info, e := os.Lstat(active); e == nil && info.Mode()&os.ModeSymlink == 0 {
		legacy = filepath.Join(versions, fmt.Sprintf("legacy-%d", time.Now().UnixNano()))
		if err = os.Rename(active, legacy); err != nil {
			return err
		}
	}
	if err = os.Rename(temporary, active); err != nil {
		if legacy != "" {
			err = errors.Join(err, os.Rename(legacy, active))
		}
		return err
	}
	return syncMediaDirectory(parent)
}

func compareMediaTrees(ctx context.Context, expected, actual string) error {
	files := map[string]bool{}
	err := filepath.WalkDir(expected, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(expected, p)
		if err != nil {
			return err
		}
		files[rel] = true
		got, err := os.Lstat(filepath.Join(actual, rel))
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := trustedMediaMember(got); err != nil {
			return err
		}
		if got.Mode().Type() != info.Mode().Type() {
			return errors.New("媒体版本成员类型改变")
		}
		if info.IsDir() {
			if got.Mode().Perm() != info.Mode().Perm() {
				return errors.New("媒体版本目录权限改变")
			}
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			a, e := os.Readlink(p)
			if e != nil {
				return e
			}
			b, e := os.Readlink(filepath.Join(actual, rel))
			if e != nil {
				return e
			}
			if a != b {
				return errors.New("媒体版本库别名改变")
			}
			return nil
		}
		if got.Size() != info.Size() || got.Mode().Perm() != info.Mode().Perm() {
			return errors.New("媒体版本成员长度或权限改变")
		}
		a, err := mediaFileHash(ctx, p)
		if err != nil {
			return err
		}
		b, err := mediaFileHash(ctx, filepath.Join(actual, rel))
		if err != nil {
			return err
		}
		if a != b {
			return errors.New("媒体版本成员内容改变")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return filepath.WalkDir(actual, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(actual, p)
		if err != nil {
			return err
		}
		if !files[rel] {
			return errors.New("媒体版本包含额外成员")
		}
		return nil
	})
}
func mediaFileHash(ctx context.Context, p string) (string, error) {
	f, e := os.Open(p)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	_, e = io.Copy(h, mediaContextReader{ctx, f})
	return hex.EncodeToString(h.Sum(nil)), e
}

type mediaContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r mediaContextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}
func syncMediaTree(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return syncMediaDirectory(p)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		f, e := os.OpenFile(p, os.O_RDWR, 0)
		if e != nil {
			return e
		}
		return errors.Join(f.Sync(), f.Close())
	})
}
