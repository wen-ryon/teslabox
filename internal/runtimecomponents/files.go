package runtimecomponents

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

func secureDirectory(p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) == string(filepath.Separator) {
		return errors.New("组件目录无效")
	}
	for current := filepath.Clean(p); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("组件目录含链接或非目录")
			}
			if err = trustedDirectory(info); err != nil {
				return err
			}
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	if err := os.MkdirAll(p, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if runtime.GOOS == "linux" && info.Mode().Perm()&0077 != 0 {
		return errors.New("组件目录权限必须私有")
	}
	return trustedDirectory(info)
}
func checkFile(p string) error {
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("组件输入不能是链接或特殊文件")
	}
	return trustedFile(info)
}
