package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
	"github.com/wen-ryon/teslabox/internal/system"
)

func run(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "nmcli", args...).CombinedOutput()
}

func main() {
	if os.Geteuid() != 0 {
		panic("测试必须经已授权的 root 管理通道执行")
	}
	manager := deviceadmin.New(system.NewOSRunner(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	before, err := manager.APSettings(context.Background())
	if err != nil || !before.Available {
		panic(fmt.Sprintf("读取热点失败: %v", err))
	}
	properties := []string{
		"802-11-wireless.ssid", "802-11-wireless.band", "802-11-wireless.channel",
		"connection.autoconnect", "connection.autoconnect-retries", "ipv4.method",
		"802-11-wireless-security.key-mgmt", "802-11-wireless-security.pmf", "802-11-wireless-security.proto",
		"802-11-wireless-security.pairwise", "802-11-wireless-security.group",
		"802-11-wireless-security.auth-alg", "802-11-wireless-security.wps-method",
	}
	values := make(map[string]string)
	for _, property := range properties {
		output, readErr := run("--escape", "no", "-g", property, "connection", "show", "uuid", before.UUID)
		if readErr != nil {
			panic("无法备份属性: " + property)
		}
		value := strings.TrimSpace(string(output))
		if strings.Contains(value, " (") {
			value = strings.Fields(value)[0]
		}
		if property == "802-11-wireless.channel" && value == "0" {
			value = ""
		}
		values[property] = value
	}
	secret, err := run("--show-secrets", "-g", "802-11-wireless-security.psk", "connection", "show", "uuid", before.UUID)
	if err != nil {
		panic("无法核验密码是否保持，停止测试")
	}
	secretHash := sha256.Sum256(secret)
	var restored bool
	defer func() {
		args := []string{"connection", "modify", "uuid", before.UUID}
		for _, property := range properties {
			args = append(args, property, values[property])
		}
		if _, restoreErr := run(args...); restoreErr != nil {
			panic(fmt.Sprintf("恢复原热点属性失败: %v", restoreErr))
		}
		if before.Enabled {
			if _, restoreErr := run("--wait", "15", "connection", "up", "uuid", before.UUID, "ifname", before.Interface); restoreErr != nil {
				panic(fmt.Sprintf("重新拉起原热点失败: %v", restoreErr))
			}
		} else if _, downErr := run("--wait", "15", "connection", "down", "uuid", before.UUID); downErr != nil {
			// 原来未运行的热点允许仍为 inactive；其他错误不能吞掉。
			var exitError *exec.ExitError
			if !errors.As(downErr, &exitError) || exitError.ExitCode() != 10 {
				fmt.Fprintln(os.Stderr, "恢复未运行热点的停用状态:", downErr)
			}
		}
		currentSecret, readErr := run("--show-secrets", "-g", "802-11-wireless-security.psk", "connection", "show", "uuid", before.UUID)
		if readErr != nil || sha256.Sum256(currentSecret) != secretHash {
			panic("已有热点密码发生变化")
		}
		restored = true
		after, readErr := manager.APSettings(context.Background())
		encoded, _ := json.Marshal(map[string]any{"phase": "restored", "restored": restored, "password_unchanged": true, "settings": after, "error": fmt.Sprint(readErr)})
		fmt.Println(string(encoded))
	}()
	encoded, _ := json.Marshal(map[string]any{"phase": "before", "settings": before})
	fmt.Println(string(encoded))
	for _, mode := range []string{"wpa2", "wpa3", "wpa2-wpa3"} {
		started := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		updateErr := manager.UpdateAP(ctx, deviceadmin.APUpdate{
			Enabled: before.Enabled, SSID: before.SSID, Band: before.Band, Channel: before.Channel,
			AutoConnect: before.AutoConnect, SecurityMode: mode,
		})
		cancel()
		code := ""
		var operationError *deviceadmin.APOperationError
		if errors.As(updateErr, &operationError) {
			code = operationError.Code
		}
		settings, readErr := manager.APSettings(context.Background())
		row := map[string]any{"phase": "case", "mode": mode, "elapsed_ms": time.Since(started).Milliseconds(), "success": updateErr == nil,
			"reason_code": code, "error": fmt.Sprint(updateErr), "settings": settings, "read_error": fmt.Sprint(readErr)}
		if mode == "wpa2" && updateErr == nil && settings.SecurityMode != "wpa2" {
			panic("WPA2 配置读回不一致")
		}
		encoded, _ := json.Marshal(row)
		fmt.Println(string(encoded))
	}
}
