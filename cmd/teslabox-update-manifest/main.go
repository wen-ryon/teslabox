package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wen-ryon/teslabox/internal/updater"
)

func main() {
	generate := flag.Bool("generate-key", false, "生成一对发布密钥")
	privateKeyPath := flag.String("private-key", "", "Base64 Ed25519 私钥文件")
	publicKeyPath := flag.String("public-key", "", "生成模式下的公钥输出文件")
	packagePath := flag.String("package", "", "ARM64 主程序路径")
	packageURL := flag.String("package-url", "", "Gitee Release 中的 HTTPS 下载地址")
	version := flag.String("version", "", "稳定版本号，例如 0.2.0")
	output := flag.String("output", "latest.json", "签名清单输出路径")
	notesFile := flag.String("notes-file", "", "UTF-8 发布说明文件")
	channel := flag.String("channel", "stable", "更新通道")
	board := flag.String("board", "radxa-rock-3a", "目标板型")
	minimumVersion := flag.String("min-version", "0.1.0", "允许直接升级的最低版本")
	flag.Parse()
	if *generate {
		if err := generateKeys(*privateKeyPath, *publicKeyPath); err != nil {
			exitError(err)
		}
		return
	}
	if err := createManifest(*privateKeyPath, *packagePath, *packageURL, *version, *output, *notesFile, *channel, *board, *minimumVersion); err != nil {
		exitError(err)
	}
}

func generateKeys(privatePath, publicPath string) error {
	if strings.TrimSpace(privatePath) == "" || strings.TrimSpace(publicPath) == "" {
		return fmt.Errorf("生成密钥时必须指定 -private-key 和 -public-key")
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("生成 Ed25519 密钥: %w", err)
	}
	if err := writeSecret(privatePath, base64.StdEncoding.EncodeToString(privateKey)); err != nil {
		return err
	}
	if err := writeSecret(publicPath, base64.StdEncoding.EncodeToString(publicKey)); err != nil {
		return err
	}
	fmt.Printf("发布私钥已写入 %s，请离线保管且不要提交到仓库。\n", privatePath)
	fmt.Printf("设备公钥已写入 %s，可配置为 TESLABOX_UPDATE_PUBLIC_KEY。\n", publicPath)
	return nil
}

func createManifest(privatePath, packagePath, packageURL, version, output, notesFile, channel, board, minimumVersion string) error {
	privateKey, err := readPrivateKey(privatePath)
	if err != nil {
		return err
	}
	packageContent, err := os.ReadFile(packagePath)
	if err != nil {
		return fmt.Errorf("读取发布程序: %w", err)
	}
	notes := ""
	if notesFile != "" {
		content, err := os.ReadFile(notesFile)
		if err != nil {
			return fmt.Errorf("读取发布说明: %w", err)
		}
		if len(content) > 48*1024 {
			return fmt.Errorf("发布说明不能超过 48 KiB")
		}
		notes = strings.TrimSpace(string(content))
	}
	hash := sha256.Sum256(packageContent)
	manifest := updater.Manifest{
		SchemaVersion: 1, Version: strings.TrimSpace(version), Channel: strings.TrimSpace(channel),
		PublishedAt: time.Now().Format(time.RFC3339), Board: strings.TrimSpace(board), Arch: "arm64",
		MinVersion: strings.TrimSpace(minimumVersion), PackageURL: strings.TrimSpace(packageURL),
		SHA256: hex.EncodeToString(hash[:]), Size: int64(len(packageContent)), ReleaseNotes: notes,
	}
	if err := manifest.Sign(privateKey); err != nil {
		return err
	}
	if err := manifest.Verify(privateKey.Public().(ed25519.PublicKey), manifest.Channel, manifest.Board, manifest.Arch, 128*1024*1024); err != nil {
		return fmt.Errorf("发布清单自检失败: %w", err)
	}
	content, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil && filepath.Dir(output) != "." {
		return err
	}
	if err := os.WriteFile(output, append(content, '\n'), 0o644); err != nil {
		return fmt.Errorf("写入发布清单: %w", err)
	}
	fmt.Printf("已生成 %s，SHA-256 %s。\n", output, manifest.SHA256)
	return nil
}

func readPrivateKey(path string) (ed25519.PrivateKey, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取发布私钥: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(content)))
	if err != nil || len(decoded) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("发布私钥必须是 Base64 Ed25519 私钥")
	}
	return ed25519.PrivateKey(decoded), nil
}

func writeSecret(path, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil && filepath.Dir(path) != "." {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("创建密钥文件 %s: %w", path, err)
	}
	defer file.Close()
	if _, err := fmt.Fprintln(file, value); err != nil {
		return err
	}
	return file.Sync()
}

func exitError(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
