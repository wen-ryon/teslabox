package certdelivery

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

func privateDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("证书分发状态目录必须使用绝对路径")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return checkDirectory(path)
}

func readPrivate(path string, limit int64) ([]byte, error) {
	if err := checkDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || checkFile(before) != nil {
		return nil, errors.New("证书分发私有文件身份无效")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(before, actual) || checkFile(actual) != nil {
		return nil, errors.New("证书分发私有文件身份改变")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("证书分发私有文件大小无效")
	}
	return data, nil
}

func writePrivate(path string, data []byte) error {
	if err := privateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || checkFile(info) != nil {
			return errors.New("证书分发目标文件身份无效")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".delivery-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return syncDirectory(directory)
}
