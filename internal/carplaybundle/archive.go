// Package carplaybundle 安装受主 ELF 签名保护的公开 CarPlay 运行时，私有身份独立保存。
package carplaybundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const maxBytes = 512 << 20
const maxFiles = 4096

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var versionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+-rc\.[1-9]\d*$`)

type member struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   int64  `json:"mode"`
	Role   string `json:"role"`
}

type manifest struct {
	Schema  int      `json:"schema"`
	Kind    string   `json:"package_kind"`
	Version string   `json:"version"`
	Files   []member `json:"files"`
}

func memberName(name string) error {
	lower := strings.ToLower(name)
	if name == "" || name == "." || path.Clean(name) != name || path.IsAbs(name) || strings.ContainsAny(name, `\:`) || strings.ContainsRune(name, 0) {
		return errors.New("CarPlay 成员路径不合法")
	}
	for _, component := range strings.Split(name, "/") {
		if component == ".." || component == "" {
			return errors.New("CarPlay 成员路径越界")
		}
	}
	for _, component := range strings.Split(lower, "/") {
		if component == "mfi-offline" || component == "reference-livi-private" {
			return errors.New("公开运行时包含私有目录")
		}
	}
	for _, suffix := range []string{".pem", ".key", ".pk8", ".p7b", ".p12", ".pfx"} {
		if strings.HasSuffix(lower, suffix) {
			return errors.New("公开运行时包含私有认证材料")
		}
	}
	switch path.Base(lower) {
	case "teslabox", "teslabox-update-helper", "latest.json", "index.json", "identity.json", "hardware.json", "service.json", "carplay-service.json", "active.json", "browser-token":
		return errors.New("内层包不能包含主程序、外层清单或设备状态")
	}
	return nil
}

func validateManifest(data []byte) (manifest, map[string]member, error) {
	var info manifest
	if len(data) > 512000 || json.Unmarshal(data, &info) != nil || info.Schema != 1 || info.Kind != "inner" || !versionPattern.MatchString(info.Version) || len(info.Files) == 0 || len(info.Files) >= maxFiles {
		return info, nil, errors.New("CarPlay 内层清单不合法")
	}
	roles := map[string]bool{"native": true, "node": true, "service": true, "unit": true, "license": true, "python": true, "node_module": true, "asset": true, "reference_source": true, "recovery": true}
	core := map[string]string{"native": "teslabox-carplay", "node": "runtime/bin/node", "service": "python/carplay-service.py", "unit": "systemd/teslabox-carplay-experiment.service"}
	counts := make(map[string]int)
	records := make(map[string]member)
	var total int64
	for _, item := range info.Files {
		if err := memberName(item.Path); err != nil {
			return info, nil, err
		}
		if _, duplicate := records[item.Path]; duplicate || item.Path == "PACKAGE.json" || !roles[item.Role] || item.Size < 0 || item.Size > maxBytes-total || !digestPattern.MatchString(item.SHA256) || item.Mode != 0600 && item.Mode != 0644 && item.Mode != 0755 {
			return info, nil, errors.New("CarPlay 成员清单重复或不合法")
		}
		if expected, found := core[item.Role]; found {
			mode := int64(0644)
			if item.Role == "native" || item.Role == "node" {
				mode = 0755
			}
			if item.Path != expected || item.Mode != mode {
				return info, nil, errors.New("CarPlay 核心成员布局不一致")
			}
		}
		total += item.Size
		counts[item.Role]++
		records[item.Path] = item
	}
	for role := range core {
		if counts[role] != 1 {
			return info, nil, errors.New("CarPlay 核心角色不完整")
		}
	}
	if counts["license"] == 0 {
		return info, nil, errors.New("CarPlay 运行时缺少许可来源")
	}
	return info, records, nil
}

func extractArchive(source io.Reader, destination string) (manifest, error) {
	var result manifest
	gz, err := gzip.NewReader(source)
	if err != nil {
		return result, err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	seen := make(map[string]member)
	var manifestData []byte
	var total int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return result, err
		}
		if err := memberName(header.Name); err != nil {
			return result, err
		}
		if _, found := seen[header.Name]; found || len(seen) >= maxFiles || header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA || header.Size < 0 || header.Size > maxBytes-total || header.Linkname != "" {
			return result, errors.New("CarPlay 归档存在重复、链接或超预算成员")
		}
		total += header.Size
		mode := header.Mode
		if mode != 0600 && mode != 0644 && mode != 0755 {
			return result, errors.New("CarPlay 归档权限不合法")
		}
		target := filepath.Join(destination, filepath.FromSlash(header.Name))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return result, err
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(mode))
		if err != nil {
			return result, err
		}
		// 主服务可能采用 UMask=0077；公开成员权限仍须与签名清单精确一致。
		if err := file.Chmod(os.FileMode(mode)); err != nil {
			file.Close()
			return result, err
		}
		hash := sha256.New()
		_, copyErr := io.CopyN(io.MultiWriter(file, hash), reader, header.Size)
		if copyErr == nil {
			copyErr = file.Sync()
		}
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return result, err
		}
		seen[header.Name] = member{Path: header.Name, Size: header.Size, Mode: mode, SHA256: hex.EncodeToString(hash.Sum(nil))}
		if header.Name == "PACKAGE.json" {
			if header.Size > 512000 {
				return result, errors.New("CarPlay 清单超过预算")
			}
			manifestData, err = os.ReadFile(target)
			if err != nil {
				return result, err
			}
		}
	}
	// 完整读取压缩流以验证尾部校验，不能在 tar 结束标志后忽略损坏。
	remaining, err := io.Copy(io.Discard, io.LimitReader(gz, maxBytes-total+1))
	if err != nil {
		return result, err
	}
	if remaining > maxBytes-total {
		return result, errors.New("CarPlay 压缩尾部超过预算")
	}
	result, records, err := validateManifest(manifestData)
	if err != nil {
		return result, err
	}
	if len(seen) != len(records)+1 {
		return result, errors.New("CarPlay 清单与归档成员不一致")
	}
	for name, item := range records {
		actual, found := seen[name]
		if !found || actual.Size != item.Size || actual.Mode != item.Mode || actual.SHA256 != item.SHA256 {
			return result, fmt.Errorf("CarPlay 成员完整性不符: %s", name)
		}
	}
	return result, nil
}

func verifyDirectory(directory string) (manifest, error) {
	meta, err := os.Lstat(filepath.Join(directory, "PACKAGE.json"))
	if err != nil || !meta.Mode().IsRegular() || meta.Size() > 512000 {
		return manifest{}, errors.New("CarPlay 已安装清单不合法")
	}
	if err := trustedMember(meta); err != nil {
		return manifest{}, err
	}
	data, err := os.ReadFile(filepath.Join(directory, "PACKAGE.json"))
	if err != nil {
		return manifest{}, err
	}
	info, records, err := validateManifest(data)
	if err != nil {
		return info, err
	}
	count := 0
	err = filepath.WalkDir(directory, func(file string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("CarPlay 版本目录不能包含链接")
		}
		if entry.IsDir() {
			meta, err := entry.Info()
			if err != nil {
				return err
			}
			return trustedParent(meta, false)
		}
		name, err := filepath.Rel(directory, file)
		if err != nil {
			return err
		}
		name = filepath.ToSlash(name)
		if name == "PACKAGE.json" {
			return nil
		}
		item, found := records[name]
		meta, err := entry.Info()
		if err != nil {
			return err
		}
		if !found || !meta.Mode().IsRegular() || meta.Size() != item.Size || int64(meta.Mode().Perm()) != item.Mode {
			return errors.New("CarPlay 已安装目录与清单不符")
		}
		if err := trustedMember(meta); err != nil {
			return err
		}
		stream, err := os.Open(file)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, readErr := io.Copy(hash, stream)
		closeErr := stream.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if hex.EncodeToString(hash.Sum(nil)) != item.SHA256 {
			return errors.New("CarPlay 已安装成员摘要不符")
		}
		count++
		return nil
	})
	if err == nil && count != len(records) {
		err = errors.New("CarPlay 已安装目录缺少成员")
	}
	return info, err
}

func verifyManifestBinding(source io.Reader, directory string) error {
	gz, err := gzip.NewReader(source)
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	var total int64
	for count := 0; count < maxFiles; count++ {
		header, err := reader.Next()
		if err != nil {
			return fmt.Errorf("读取已签名清单: %w", err)
		}
		if header.Size < 0 || header.Size > maxBytes-total {
			return errors.New("已签名清单查找超过预算")
		}
		total += header.Size
		if header.Name != "PACKAGE.json" {
			continue
		}
		if header.Size > 512000 || header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return errors.New("已签名清单格式不合法")
		}
		expected, err := io.ReadAll(io.LimitReader(reader, 512001))
		if err != nil {
			return err
		}
		actual, err := os.ReadFile(filepath.Join(directory, "PACKAGE.json"))
		if err != nil {
			return err
		}
		if !bytes.Equal(expected, actual) {
			return errors.New("已安装清单与签名归档不一致")
		}
		return nil
	}
	return errors.New("缺少已签名运行时清单")
}
