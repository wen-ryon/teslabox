package gadget

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/wen-ryon/teslabox/internal/system"
)

var conflictingGadgetServices = []string{
	"radxa-adbd.service",
	"radxa-usbnet.service",
}

// ServiceState 描述可能争用 ROCK 3A OTG 控制器的原厂服务状态。
type ServiceState struct {
	Name          string `json:"name"`
	LoadState     string `json:"load_state"`
	ActiveState   string `json:"active_state"`
	SubState      string `json:"sub_state"`
	UnitFileState string `json:"unit_file_state"`
}

// InspectConflictingServices 只读取固定原厂 Gadget 服务，供启动保护和诊断页面共用。
func InspectConflictingServices(ctx context.Context, runner system.Runner) ([]ServiceState, error) {
	if runner == nil {
		return nil, errors.New("系统命令执行器不能为空")
	}
	args := []string{
		"show",
		"--property=Id",
		"--property=LoadState",
		"--property=ActiveState",
		"--property=SubState",
		"--property=UnitFileState",
	}
	args = append(args, conflictingGadgetServices...)
	output, err := runner.Run(ctx, "systemctl", args...)
	if err != nil {
		return nil, fmt.Errorf("读取原厂 USB Gadget 服务: %w", err)
	}
	return parseServiceStates(output), nil
}

// DisableConflictingServices 在 TeslaBox 绑定 UDC 前永久屏蔽已安装的原厂 Gadget 服务。
func DisableConflictingServices(ctx context.Context, runner system.Runner) ([]string, error) {
	states, err := InspectConflictingServices(ctx, runner)
	if err != nil {
		return nil, err
	}
	protected := make([]string, 0, len(states))
	for _, state := range states {
		if state.LoadState == "not-found" || state.LoadState == "unknown" {
			continue
		}
		if state.ActiveState == "inactive" && strings.HasPrefix(state.UnitFileState, "masked") {
			continue
		}
		if _, err := runner.Run(ctx, "systemctl", "disable", "--now", state.Name); err != nil {
			return protected, fmt.Errorf("停用原厂 USB Gadget 服务 %s: %w", state.Name, err)
		}
		if _, err := runner.Run(ctx, "systemctl", "mask", state.Name); err != nil {
			return protected, fmt.Errorf("屏蔽原厂 USB Gadget 服务 %s: %w", state.Name, err)
		}
		protected = append(protected, state.Name)
	}
	return protected, nil
}

func parseServiceStates(output []byte) []ServiceState {
	normalized := strings.ReplaceAll(strings.TrimSpace(string(output)), "\r\n", "\n")
	byName := make(map[string]ServiceState, len(conflictingGadgetServices))
	for _, block := range strings.Split(normalized, "\n\n") {
		state := ServiceState{}
		for _, line := range strings.Split(block, "\n") {
			key, value, found := strings.Cut(strings.TrimSpace(line), "=")
			if !found {
				continue
			}
			switch key {
			case "Id":
				state.Name = value
			case "LoadState":
				state.LoadState = value
			case "ActiveState":
				state.ActiveState = value
			case "SubState":
				state.SubState = value
			case "UnitFileState":
				state.UnitFileState = value
			}
		}
		if slices.Contains(conflictingGadgetServices, state.Name) {
			byName[state.Name] = state
		}
	}
	states := make([]ServiceState, 0, len(conflictingGadgetServices))
	for _, name := range conflictingGadgetServices {
		state, exists := byName[name]
		if !exists {
			state = ServiceState{Name: name, LoadState: "unknown"}
		}
		states = append(states, state)
	}
	return states
}
