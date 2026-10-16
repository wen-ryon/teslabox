// Package regulatory 统一保存无线使用地区，并将配置与内核实际监管状态分开展示。
package regulatory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wen-ryon/teslabox/internal/system"
)

const (
	// DefaultCountryFile 是启动组件和页面共同读取的唯一持久配置。
	DefaultCountryFile = "/etc/teslabox/wireless-country"
	// DefaultLockFile 防止启动组件与页面同时写入不同的运行监管域。
	DefaultLockFile    = "/run/lock/teslabox-wireless-region.lock"
	maximumConfigBytes = 32 * 1024
)

var (
	legacyCountry    = regexp.MustCompile(`(?m)^\s*(?:country|REGDOMAIN|TESLABOX_WIFI_COUNTRY)\s*=\s*["']?([A-Za-z0-9]{2})["']?\s*(?:#.*)?$`)
	legacyAssignment = regexp.MustCompile(`^\s*(?:country|REGDOMAIN|TESLABOX_WIFI_COUNTRY)\s*=`)
	moduleCountry    = regexp.MustCompile(`(?:^|\s)(?:cfg80211\.)?ieee80211_regdom\s*=\s*["']?([A-Za-z0-9]{2})["']?(?:\s|$)`)
	regCountry       = regexp.MustCompile(`^country ([A-Z0-9]{2}):`)
)

// Paths 让迁移测试只使用临时目录，真实路径不由页面输入决定。
type Paths struct {
	CountryFile        string
	LockFile           string
	LegacyFiles        []string
	ModuleDirectories  []string
	KernelCommandLine  string
	ModuleParameter    string
	EnvironmentCountry string
}

// DefaultPaths 兼容已有系统国家码，不用语言、时区或公网 IP 推断所在地。
func DefaultPaths() Paths {
	return Paths{
		CountryFile: DefaultCountryFile, LockFile: DefaultLockFile,
		LegacyFiles:       []string{"/etc/default/crda", "/etc/wpa_supplicant/wpa_supplicant.conf", "/etc/wpa_supplicant.conf", "/etc/teslabox/teslabox.env"},
		ModuleDirectories: []string{"/etc/modprobe.d", "/run/modprobe.d", "/usr/lib/modprobe.d", "/lib/modprobe.d"},
		KernelCommandLine: "/proc/cmdline", ModuleParameter: "/sys/module/cfg80211/parameters/ieee80211_regdom",
		EnvironmentCountry: os.Getenv("TESLABOX_WIFI_COUNTRY"),
	}
}

// PHYDomain 保留自管理网卡的实际国家码，不能用全局国家码替代它。
type PHYDomain struct {
	PHY         string `json:"phy"`
	Country     string `json:"country"`
	SelfManaged bool   `json:"self_managed"`
}

// Status 的 applied 只表示监管域已核对，不代表热点广播、手机握手或 DHCP 已验收。
type Status struct {
	ConfiguredCountry string      `json:"configured_country"`
	EffectiveCountry  string      `json:"effective_country"`
	PHYDomains        []PHYDomain `json:"phy_domains"`
	State             string      `json:"state"`
	ReasonCode        string      `json:"reason_code,omitempty"`
	Message           string      `json:"message"`
	Saved             bool        `json:"saved"`
	LastError         string      `json:"last_error,omitempty"`
}

// Manager 串行化本进程的配置操作，并与系统启动脚本共用文件锁。
type Manager struct {
	runner    system.Runner
	paths     Paths
	mu        sync.Mutex
	lastError string
}

// New 不在构造阶段写系统文件，迁移由明确的启动入口执行。
func New(runner system.Runner, paths Paths) *Manager {
	return &Manager{runner: runner, paths: paths}
}

// ValidCountry 只接受 ISO 国家码及明确选择的受限世界域，不接受任意命令参数。
func ValidCountry(value string) bool {
	const codes = " AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW "
	return value == "00" || (len(value) == 2 && strings.Contains(codes, " "+value+" "))
}

// Initialize 仅在没有持久配置时迁移已有国家码，真正未配置的国内版设备才默认 CN。
func (m *Manager) Initialize(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := acquire(ctx, m.paths.LockFile)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := readCountry(m.paths.CountryFile); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	country, err := m.existingCountry(ctx)
	if err != nil {
		return err
	}
	if country == "" {
		country = "CN"
	}
	return writeCountry(m.paths.CountryFile, country, true)
}

// Apply 只应用已持久化的国家码，不扫描周边热点或放宽驱动的硬件限制。
func (m *Manager) Apply(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := acquire(ctx, m.paths.LockFile)
	if err != nil {
		return err
	}
	defer unlock()
	return m.applyLocked(ctx)
}

// Status 即使配置或命令异常也保留可展示的原因，不把未知状态包装为已生效。
func (m *Manager) Status(ctx context.Context) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked(ctx)
}

// Set 保存用户显式选择；运行应用失败时返回 saved 状态，便于页面明确提示待生效。
func (m *Manager) Set(ctx context.Context, country string) (Status, error) {
	country = strings.ToUpper(strings.TrimSpace(country))
	if !ValidCountry(country) {
		return Status{}, fmt.Errorf("国家/地区代码无效，请填写 CN、US 等两位国家码")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := acquire(ctx, m.paths.LockFile)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	if err := writeCountry(m.paths.CountryFile, country, false); err != nil {
		return Status{}, err
	}
	_ = m.applyLocked(ctx)
	return m.statusLocked(ctx), nil
}

func (m *Manager) applyLocked(ctx context.Context) error {
	country, err := readCountry(m.paths.CountryFile)
	if err == nil {
		operationCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		_, err = m.runner.Run(operationCtx, "iw", "reg", "set", country)
		if err == nil {
			for range 8 {
				status := m.statusLocked(operationCtx)
				if status.EffectiveCountry == country {
					m.lastError = ""
					return nil
				}
				select {
				case <-operationCtx.Done():
					err = operationCtx.Err()
				case <-time.After(100 * time.Millisecond):
				}
				if err != nil {
					break
				}
			}
			if err == nil {
				err = errors.New("内核尚未确认所选监管域")
			}
		}
	}
	if err != nil {
		m.lastError = err.Error()
	}
	return err
}

func (m *Manager) statusLocked(ctx context.Context) Status {
	status := Status{PHYDomains: []PHYDomain{}, State: "unavailable", Message: "无法确认实际无线监管域", LastError: m.lastError}
	country, err := readCountry(m.paths.CountryFile)
	configErr := err
	if configErr != nil {
		status.ReasonCode = "regulatory_config_invalid"
		status.Message = "无线地区尚未配置或配置无效"
	} else {
		status.ConfiguredCountry, status.Saved = country, true
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	output, err := m.runner.Run(probeCtx, "iw", "reg", "get")
	if err != nil {
		status.ReasonCode = "regulatory_probe_failed"
		return status
	}
	status.EffectiveCountry, status.PHYDomains = ParseDomains(string(output))
	if configErr != nil {
		return status
	}
	if status.EffectiveCountry != country {
		status.State = "pending"
		status.ReasonCode = "regulatory_not_applied"
		status.Message = "地区已保存，但实际全局监管域尚未匹配"
		return status
	}
	status.State, status.Message = "applied", "所选监管域已生效；可用信道和功率仍受各网卡、固件及监管规则限制"
	for _, phy := range status.PHYDomains {
		if phy.Country != country {
			status.State = "restricted"
			status.ReasonCode = "regulatory_phy_mismatch"
			status.Message = "全局地区已匹配，但部分网卡报告不同监管域，请按其实际允许信道使用"
			break
		}
	}
	if m.lastError != "" {
		status.State = "pending"
		status.ReasonCode = "regulatory_apply_failed"
		status.Message = "地区已保存，但本次应用失败，尚不能确认生效"
	}
	return status
}

// ParseDomains 同时解析全局和各 PHY 的监管域，不把首个 PHY 误当作全局。
func ParseDomains(output string) (string, []PHYDomain) {
	global := ""
	phys := []PHYDomain{}
	section := ""
	selfManaged := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "global" {
			section = "global"
			continue
		}
		if strings.HasPrefix(line, "phy#") {
			section = strings.Fields(line)[0]
			selfManaged = strings.Contains(line, "self-managed")
			continue
		}
		match := regCountry.FindStringSubmatch(line)
		if len(match) != 2 {
			continue
		}
		if section == "global" {
			global = match[1]
		} else if strings.HasPrefix(section, "phy#") {
			phys = append(phys, PHYDomain{PHY: section, Country: match[1], SelfManaged: selfManaged})
		}
	}
	return global, phys
}

func (m *Manager) existingCountry(ctx context.Context) (string, error) {
	countries := map[string]bool{}
	add := func(value string) error {
		value = strings.ToUpper(strings.TrimSpace(value))
		if !ValidCountry(value) {
			return errors.New("已有无线地区配置无效，拒绝以 CN 覆盖")
		}
		countries[value] = true
		return nil
	}
	if m.paths.EnvironmentCountry != "" {
		if err := add(m.paths.EnvironmentCountry); err != nil {
			return "", err
		}
	}
	files := append([]string(nil), m.paths.LegacyFiles...)
	for _, directory := range m.paths.ModuleDirectories {
		entries, err := os.ReadDir(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".conf") {
				files = append(files, filepath.Join(directory, entry.Name()))
			}
		}
	}
	for _, file := range files {
		content, err := readSmall(file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("检查既有地区配置: %w", err)
		}
		for _, line := range strings.Split(string(content), "\n") {
			line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
			matches := legacyCountry.FindAllStringSubmatch(line, -1)
			if legacyAssignment.MatchString(line) && len(matches) == 0 {
				value := strings.Trim(strings.TrimSpace(strings.SplitN(line, "=", 2)[1]), "\"'")
				if value == "" {
					continue
				}
				return "", errors.New("已有无线地区配置格式无效，拒绝以 CN 覆盖")
			}
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "options" && fields[1] == "cfg80211" {
				matches = append(matches, moduleCountry.FindAllStringSubmatch(line, -1)...)
				if strings.Contains(line, "ieee80211_regdom") && len(matches) == 0 {
					return "", errors.New("已有模块国家码无效，拒绝覆盖")
				}
			} else if strings.Contains(line, "ieee80211_regdom") {
				continue
			}
			for _, match := range matches {
				if err := add(match[1]); err != nil {
					return "", err
				}
			}
		}
	}
	if m.paths.KernelCommandLine != "" {
		content, err := readSmall(m.paths.KernelCommandLine)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		for _, match := range moduleCountry.FindAllStringSubmatch(string(content), -1) {
			if err := add(match[1]); err != nil {
				return "", err
			}
		}
	}
	if len(countries) > 1 {
		return "", errors.New("多处既有无线地区配置冲突，请在页面明确选择，未覆盖任何已有文件")
	}
	for country := range countries {
		return country, nil
	}
	if m.paths.ModuleParameter != "" {
		content, err := readSmall(m.paths.ModuleParameter)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if value := strings.ToUpper(strings.TrimSpace(string(content))); value != "" && value != "00" {
			return value, add(value)
		}
	}
	output, err := m.runner.Run(ctx, "iw", "reg", "get")
	if err == nil {
		if country, _ := ParseDomains(string(output)); country != "" && country != "00" {
			return country, add(country)
		}
	}
	return "", nil
}

func readSmall(name string) ([]byte, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maximumConfigBytes {
		return nil, errors.New("地区配置不是有界普通文件")
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maximumConfigBytes+1))
	if len(content) > maximumConfigBytes {
		return nil, errors.New("地区配置过大")
	}
	return content, err
}

func readCountry(name string) (string, error) {
	content, err := readSmall(name)
	if err != nil {
		return "", err
	}
	country := strings.TrimSpace(string(content))
	if !ValidCountry(country) {
		return "", errors.New("已有地区文件无效，未覆盖")
	}
	return country, nil
}

func writeCountry(name, country string, createOnly bool) error {
	if !filepath.IsAbs(name) || !ValidCountry(country) {
		return errors.New("地区配置路径或国家码无效")
	}
	if _, err := os.Lstat(name); err == nil {
		if createOnly {
			_, err := readCountry(name)
			return err
		}
		if _, err := readSmall(name); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(name), ".wireless-country-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(country + "\n"); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if createOnly {
		if err := os.Link(temporary.Name(), name); err != nil {
			if errors.Is(err, os.ErrExist) {
				_, err = readCountry(name)
			}
			if err != nil {
				return err
			}
		}
		return syncDirectory(filepath.Dir(name))
	}
	if err := os.Rename(temporary.Name(), name); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(name))
}

// DiagnosticSummary 输出有限的配置和实际域，供完整诊断报告使用，不读取 WiFi 密码。
func (m *Manager) DiagnosticSummary(ctx context.Context) string {
	status := m.Status(ctx)
	lines := []string{fmt.Sprintf("configured=%s effective=%s state=%s reason=%s", status.ConfiguredCountry, status.EffectiveCountry, status.State, status.ReasonCode)}
	for _, phy := range status.PHYDomains {
		lines = append(lines, fmt.Sprintf("%s country=%s self_managed=%t", phy.PHY, phy.Country, phy.SelfManaged))
	}
	sort.Strings(lines[1:])
	return strings.Join(lines, "\n")
}
