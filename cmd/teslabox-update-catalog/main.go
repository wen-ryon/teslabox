package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wen-ryon/teslabox/internal/updater"
)

type manifestPaths []string

func (paths *manifestPaths) String() string {
	return strings.Join(*paths, ",")
}

func (paths *manifestPaths) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("历史清单路径不能为空")
	}
	*paths = append(*paths, value)
	return nil
}

func main() {
	privateKeyPath := flag.String("private-key", "", "Base64 Ed25519 私钥文件")
	output := flag.String("output", "history.json", "签名历史目录输出路径")
	channel := flag.String("channel", "stable", "更新通道")
	board := flag.String("board", "radxa-rock-3a", "目标板型")
	var paths manifestPaths
	flag.Var(&paths, "manifest", "一个已签名更新清单路径；可重复指定")
	flag.Parse()
	if err := createCatalog(*privateKeyPath, *output, *channel, *board, paths); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func createCatalog(privateKeyPath, output, channel, board string, paths manifestPaths) error {
	if len(paths) == 0 {
		return errors.New("至少需要一个 -manifest 历史清单")
	}
	privateKey, err := readPrivateKey(privateKeyPath)
	if err != nil {
		return err
	}
	manifests := make([]updater.Manifest, 0, len(paths))
	for _, manifestPath := range paths {
		manifest, err := readManifest(manifestPath)
		if err != nil {
			return err
		}
		if err := manifest.Verify(privateKey.Public().(ed25519.PublicKey), strings.TrimSpace(channel), strings.TrimSpace(board), "arm64", 128*1024*1024); err != nil {
			return fmt.Errorf("校验 %s: %w", manifestPath, err)
		}
		manifests = append(manifests, manifest)
	}
	catalog := updater.Catalog{
		SchemaVersion: 1,
		Channel:       strings.TrimSpace(channel),
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Manifests:     manifests,
	}
	if err := catalog.Sign(privateKey); err != nil {
		return err
	}
	if err := catalog.Verify(privateKey.Public().(ed25519.PublicKey), catalog.Channel, strings.TrimSpace(board), "arm64", 128*1024*1024); err != nil {
		return fmt.Errorf("历史版本目录自检失败: %w", err)
	}
	content, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Dir(output)
	if directory != "." {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return fmt.Errorf("创建输出目录: %w", err)
		}
	}
	if err := os.WriteFile(output, append(content, '\n'), 0o644); err != nil {
		return fmt.Errorf("写入历史版本目录: %w", err)
	}
	fmt.Printf("已生成 %s，包含 %d 个签名历史版本。\n", output, len(catalog.Manifests))
	return nil
}

func readManifest(path string) (updater.Manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return updater.Manifest{}, fmt.Errorf("读取历史清单 %s: %w", path, err)
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err != nil {
		return updater.Manifest{}, fmt.Errorf("读取历史清单 %s: %w", path, err)
	}
	if len(content) > 64*1024 {
		return updater.Manifest{}, fmt.Errorf("历史清单 %s 超过大小限制", path)
	}
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	decoder.DisallowUnknownFields()
	var manifest updater.Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return updater.Manifest{}, fmt.Errorf("解析历史清单 %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return updater.Manifest{}, fmt.Errorf("历史清单 %s 包含多余内容", path)
	}
	return manifest, nil
}

func readPrivateKey(path string) (ed25519.PrivateKey, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("必须指定 -private-key")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取发布私钥: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(content)))
	if err != nil || len(decoded) != ed25519.PrivateKeySize {
		return nil, errors.New("发布私钥必须是 Base64 Ed25519 私钥")
	}
	return ed25519.PrivateKey(decoded), nil
}
