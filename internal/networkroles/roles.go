// Package networkroles 在 NetworkManager 启动前准备原生双接口，不操作活动网络。
package networkroles

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var nativeName = regexp.MustCompile(`^(ap|sta)[0-9]+$`)
var temporaryKeyfileName = regexp.MustCompile(`\.[A-Za-z0-9]{6}$`)

func ignoredKeyfileName(name string) bool {
	// 与 NM 1.30 的 mkstemp 过滤规则一致，残留临时文件不是第二份活动配置。
	// https://github.com/NetworkManager/NetworkManager/blob/1.30.6/libnm-core/nm-keyfile.c
	return strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") || strings.HasSuffix(name, ".bak") ||
		temporaryKeyfileName.MatchString(name) || strings.HasSuffix(name, ".swp") || strings.HasSuffix(name, ".swpx") ||
		strings.HasSuffix(strings.ToLower(name), ".pem") || strings.HasSuffix(strings.ToLower(name), ".der")
}

func managerStopped(state string, queryErr error) bool {
	return queryErr == nil && (strings.TrimSpace(state) == "inactive" || strings.TrimSpace(state) == "failed")
}

func networkManagerProcessRunning(proc string) (bool, error) {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "" || strings.Trim(name, "0123456789") != "" {
			continue
		}
		comm, err := read(filepath.Join(proc, name, "comm"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(comm) == "NetworkManager" {
			return true, nil
		}
	}
	return false, nil
}

// Paths 便于在隔离目录测试，不从 HTTP 或设备配置接受任意路径。
type Paths struct{ Sys, Profiles, State, Runtime, NMConfig string }

// DefaultPaths 将状态、运行期角色和系统连接分别存放，备份目录必须只有 root 可读。
func DefaultPaths() Paths {
	return Paths{"/sys", "/etc/NetworkManager/system-connections", "/var/lib/teslabox-system/network-roles", "/run/teslabox/wireless-roles.env", "/run/NetworkManager/conf.d/90-teslabox-native-ap.conf"}
}

// Pair 记录经过驱动参数和同 PHY 验证的两个原生接口，不靠枚举顺序选择。
type Pair struct{ AP, Station, PHY, APMAC, StationMAC string }

func read(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(data) > 1024*1024 {
		return "", errors.New("网络角色文件超过大小限制")
	}
	return string(data), nil
}

// Detect 拒绝无法证明角色的旧命名、多卡和重复 MAC，不猜 port 或创建第三接口。
func Detect(sys string) (*Pair, error) {
	return detect(sys, filepath.EvalSymlinks)
}

func detect(sys string, resolve func(string) (string, error)) (*Pair, error) {
	entries, err := os.ReadDir(filepath.Join(sys, "class/net"))
	if err != nil {
		return nil, err
	}
	var candidates []string
	phys := map[string]bool{}
	for _, entry := range entries {
		base := filepath.Join(sys, "class/net", entry.Name())
		phy, e := resolve(filepath.Join(base, "phy80211"))
		if e != nil {
			continue
		}
		phys[phy] = true
		module, e := resolve(filepath.Join(base, "device/driver/module"))
		if e == nil && filepath.Base(module) == "8852be" {
			candidates = append(candidates, entry.Name())
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	if len(candidates) != 2 || len(phys) != 1 {
		return nil, errors.New("native_roles_ambiguous: 原生双接口或无线硬件归属不唯一，未修改配置")
	}
	for key, want := range map[string]string{"ifname": "ap%d", "if2name": "sta%d"} {
		value, err := read(filepath.Join(sys, "module/8852be/parameters", key))
		if err != nil || strings.TrimSpace(value) != want {
			return nil, errors.New("native_roles_cold_start_required: 需要使用原生命名参数冷启动，未重命名活动接口")
		}
	}
	pair := &Pair{}
	for _, name := range candidates {
		if !nativeName.MatchString(name) {
			return nil, errors.New("native_roles_name_invalid: 原生接口名称异常")
		}
		if strings.HasPrefix(name, "ap") {
			pair.AP = name
		} else {
			pair.Station = name
		}
	}
	if pair.AP == "" || pair.Station == "" {
		return nil, errors.New("native_roles_cold_start_required: 未发现原生 AP/STA 命名")
	}
	for name, target := range map[string]*string{pair.AP: &pair.APMAC, pair.Station: &pair.StationMAC} {
		value, err := read(filepath.Join(sys, "class/net", name, "address"))
		if err != nil {
			return nil, err
		}
		*target = strings.TrimSpace(strings.ToLower(value))
		if !regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`).MatchString(*target) {
			return nil, errors.New("native_roles_mac_invalid: 接口地址异常")
		}
	}
	if pair.APMAC == pair.StationMAC {
		return nil, errors.New("native_roles_mac_duplicate: 原生接口地址重复，未修改 MAC")
	}
	for phy := range phys {
		pair.PHY = phy
	}
	return pair, nil
}

// Keyfile 仅定位和修改指定 INI 字段，其余内容（包括密码和注释）逐字保留。
type Keyfile struct {
	lines   []string
	newline string
}

func parseKeyfile(text string) (*Keyfile, error) {
	if strings.ContainsRune(text, 0) {
		return nil, errors.New("连接配置含 NUL")
	}
	k := &Keyfile{newline: "\n"}
	if strings.Contains(text, "\r\n") {
		k.newline = "\r\n"
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}
	k.lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	sections, keys := map[string]bool{}, map[string]bool{}
	section := ""
	for _, line := range k.lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			if sections[section] {
				return nil, errors.New("连接配置含重复段，拒绝猜测")
			}
			sections[section] = true
		} else if line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, ";") {
			key, _, ok := strings.Cut(line, "=")
			if !ok {
				return nil, errors.New("连接配置格式异常")
			}
			id := section + "/" + strings.TrimSpace(key)
			if keys[id] {
				return nil, errors.New("连接配置含重复字段，拒绝猜测")
			}
			keys[id] = true
		}
	}
	return k, nil
}

func (k *Keyfile) get(section, key string) string {
	active := false
	for _, line := range k.lines {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "[") {
			active = s == "["+section+"]"
			continue
		}
		left, right, ok := strings.Cut(s, "=")
		if active && ok && strings.TrimSpace(left) == key {
			return right
		}
	}
	return ""
}

func (k *Keyfile) set(section, key, value string) {
	active := false
	end := -1
	for i, line := range k.lines {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "[") {
			if active {
				end = i
				break
			}
			active = s == "["+section+"]"
			continue
		}
		left, _, ok := strings.Cut(s, "=")
		if active && ok && strings.TrimSpace(left) == key {
			k.lines[i] = key + "=" + value
			return
		}
	}
	if active && end < 0 {
		end = len(k.lines)
	}
	if end < 0 {
		k.lines = append(k.lines, "["+section+"]", key+"="+value)
		return
	}
	k.lines = append(k.lines, "")
	copy(k.lines[end+1:], k.lines[end:])
	k.lines[end] = key + "=" + value
}

func (k *Keyfile) text() string { return strings.Join(k.lines, k.newline) + k.newline }

func wifiSection(k *Keyfile) string {
	for _, line := range k.lines {
		if strings.TrimSpace(line) == "[wifi]" {
			return "wifi"
		}
	}
	return "802-11-wireless"
}

// MigrateProfile 只迁移本卡的已知旧绑定或未绑定配置，遇到其他硬件绑定不覆盖。
func MigrateProfile(text string, pair Pair, apSSID string) (string, error) {
	k, err := parseKeyfile(text)
	if err != nil {
		return "", err
	}
	typ := k.get("connection", "type")
	if typ != "wifi" && typ != "802-11-wireless" {
		return text, nil
	}
	section := wifiSection(k)
	mode := k.get(section, "mode")
	binding := k.get("connection", "interface-name")
	mac := strings.ToLower(k.get(section, "mac-address"))
	if mac != "" && mac != pair.APMAC && mac != pair.StationMAC {
		return text, nil
	}
	if binding != "" && binding != "wlan0" && binding != "wlan1" && binding != pair.AP && binding != pair.Station {
		return text, nil
	}
	if mode == "ap" {
		if k.get("connection", "id") != "TeslaBox-5G" {
			return text, nil
		}
		if strings.EqualFold(k.get(section, "cloned-mac-address"), pair.StationMAC) {
			return "", errors.New("native_roles_mac_conflict: AP 固定克隆地址与 STA 重复，未修改配置")
		}
		k.set("connection", "interface-name", pair.AP)
		if mac == pair.StationMAC {
			k.set(section, "mac-address", pair.APMAC)
		}
	} else if mode == "" || mode == "infrastructure" {
		if strings.EqualFold(k.get(section, "cloned-mac-address"), pair.APMAC) {
			return "", errors.New("native_roles_mac_conflict: STA 固定克隆地址与 AP 重复，未修改配置")
		}
		k.set("connection", "interface-name", pair.Station)
		if mac == pair.APMAC {
			k.set(section, "mac-address", pair.StationMAC)
		}
		if apSSID != "" && k.get(section, "ssid") == apSSID {
			k.set("connection", "autoconnect", "false")
		}
	} else {
		return text, nil
	}
	return k.text(), nil
}

type change struct {
	Path   string `json:"path"`
	Before []byte `json:"before"`
	Exists bool   `json:"exists"`
	After  []byte `json:"after"`
	Mode   uint32 `json:"mode"`
}
type transaction struct {
	SchemaVersion int      `json:"schema_version"`
	Phase         string   `json:"phase"`
	Changes       []change `json:"changes,omitempty"`
	Undo          []change `json:"undo,omitempty"`
}

func regularRead(path string) ([]byte, bool, os.FileMode, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, 0o600, nil
	}
	if err != nil {
		return nil, false, 0, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return nil, false, 0, errors.New("拒绝非常规或过大的网络配置")
	}
	data, err := os.ReadFile(path)
	return data, true, info.Mode().Perm(), err
}

func atomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, _, _, err := regularRead(path); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".teslabox-roles-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err == nil {
		if dir, e := os.Open(filepath.Dir(path)); e == nil {
			_ = dir.Sync()
			_ = dir.Close()
		}
	}
	return err
}

func journal(paths Paths, t transaction) error {
	t.SchemaVersion = 1
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return atomic(filepath.Join(paths.State, "transaction.json"), data, 0o600)
}

func loadJournal(paths Paths) (transaction, error) {
	var t transaction
	data, exists, _, err := regularRead(filepath.Join(paths.State, "transaction.json"))
	if err != nil || !exists {
		return t, err
	}
	if err = json.Unmarshal(data, &t); err != nil {
		return t, err
	}
	if t.SchemaVersion != 1 {
		return t, errors.New("网络角色事务版本不支持，保留现场")
	}
	if t.Phase != "prepared" && t.Phase != "committed" && t.Phase != "rolled_back" {
		return t, errors.New("网络角色事务状态异常")
	}
	return t, nil
}

func restore(paths Paths, changes []change) error {
	// 先检查全部文件；不能部分恢复后才发现管理员已经改过另一份配置。
	for _, c := range changes {
		valid := c.Path == paths.Runtime || c.Path == paths.NMConfig || filepath.Dir(c.Path) == paths.Profiles
		if !valid {
			return errors.New("网络角色事务包含越界路径")
		}
		current, exists, _, err := regularRead(c.Path)
		if err != nil {
			return err
		}
		if !exists && !c.Exists {
			continue
		}
		if !bytes.Equal(current, c.After) && !bytes.Equal(current, c.Before) {
			return errors.New("网络角色回退冲突，保留管理员修改")
		}
	}
	for _, c := range changes {
		if c.Exists {
			if err := atomic(c.Path, c.Before, os.FileMode(c.Mode)); err != nil {
				return err
			}
		} else if err := os.Remove(c.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func recoverPrepared(paths Paths, t *transaction) error {
	if t.Phase != "prepared" {
		return nil
	}
	if err := restore(paths, t.Changes); err != nil {
		return err
	}
	t.Phase = "committed"
	t.Changes = nil
	return journal(paths, *t)
}

// Rollback 恢复原始连接配置事务；部署层还必须成套恢复命名参数和启动组件。
// 调用方必须持锁并确认 NM 未运行，因此不作为独立的线上回退命令公开。
func Rollback(paths Paths) error {
	t, err := loadJournal(paths)
	if err != nil || t.Phase == "" || t.Phase == "rolled_back" {
		return err
	}
	if err = recoverPrepared(paths, &t); err != nil {
		return err
	}
	if err = restore(paths, t.Undo); err != nil {
		return err
	}
	return journal(paths, transaction{Phase: "rolled_back"})
}

// Prepare 在全部计划验证后先写恢复日志；中断后重新执行先恢复，再从真实状态重算。
// 调用方必须持跨进程锁，并保证 NetworkManager 尚未启动。
func Prepare(paths Paths) error {
	return prepare(paths, Detect)
}

func prepare(paths Paths, detectPair func(string) (*Pair, error)) error {
	t, err := loadJournal(paths)
	if err != nil {
		return err
	}
	if err = recoverPrepared(paths, &t); err != nil {
		return err
	}
	pair, err := detectPair(paths.Sys)
	if err != nil {
		return err
	}
	changes := []change{}
	add := func(path string, after []byte, mode os.FileMode) error {
		before, exists, oldMode, err := regularRead(path)
		if err != nil {
			return err
		}
		if exists && bytes.Equal(before, after) {
			return nil
		}
		if exists {
			mode = oldMode
		}
		changes = append(changes, change{path, before, exists, after, uint32(mode)})
		return nil
	}
	if pair != nil {
		entries, err := os.ReadDir(paths.Profiles)
		if err != nil {
			return err
		}
		if len(entries) > 1024 {
			return errors.New("连接配置数量异常")
		}
		type profile struct{ path, text string }
		var profiles []profile
		ssid := ""
		apCount := 0
		for _, entry := range entries {
			if ignoredKeyfileName(entry.Name()) {
				continue
			}
			path := filepath.Join(paths.Profiles, entry.Name())
			data, _, _, err := regularRead(path)
			if err != nil {
				return err
			}
			k, err := parseKeyfile(string(data))
			if err != nil {
				return err
			}
			if k.get("connection", "id") == "TeslaBox-5G" && k.get(wifiSection(k), "mode") == "ap" {
				apCount++
				ssid = k.get(wifiSection(k), "ssid")
			}
			profiles = append(profiles, profile{path, string(data)})
		}
		if apCount != 1 {
			return errors.New("native_roles_ap_template_ambiguous: 缺少唯一 AP 模板，未修改配置")
		}
		for _, p := range profiles {
			updated, err := MigrateProfile(p.text, *pair, ssid)
			if err != nil {
				return err
			}
			k, err := parseKeyfile(updated)
			if err != nil {
				return err
			}
			if k.get("connection", "id") == "TeslaBox-5G" && k.get(wifiSection(k), "mode") == "ap" && k.get("connection", "interface-name") != pair.AP {
				return errors.New("native_roles_ap_binding_conflict: AP 模板绑定其他硬件，未覆盖")
			}
			if err = add(p.path, []byte(updated), 0o600); err != nil {
				return err
			}
		}
		if err = add(paths.Runtime, []byte("TESLABOX_AP_INTERFACE="+pair.AP+"\nTESLABOX_WIFI_INTERFACE="+pair.Station+"\n"), 0o600); err != nil {
			return err
		}
		// NM 的设备级 managed=no 可被明确接管覆盖，区别于严格 unmanaged-devices。
		if old, exists, _, e := regularRead(paths.NMConfig); e != nil {
			return e
		} else if exists && !bytes.HasPrefix(old, []byte("# TeslaBox managed native AP reservation\n")) {
			return errors.New("保留管理员自定义 AP 设备规则")
		}
		if err = add(paths.NMConfig, []byte("# TeslaBox managed native AP reservation\n[device-teslabox-native-ap]\nmatch-device=interface-name:"+pair.AP+"\nmanaged=0\n"), 0o600); err != nil {
			return err
		}
	} else {
		// 换回 MT7922 时撤销本模块自己的旧保留名，不改其 AP 创建和用户配置。
		if err = add(paths.Runtime, []byte{}, 0o600); err != nil {
			return err
		}
		if old, exists, _, e := regularRead(paths.NMConfig); e != nil {
			return e
		} else if exists && bytes.HasPrefix(old, []byte("# TeslaBox managed native AP reservation\n")) {
			if err = add(paths.NMConfig, []byte("# TeslaBox managed native AP reservation\n"), 0o600); err != nil {
				return err
			}
		}
	}
	if len(changes) == 0 {
		return nil
	}
	budget := 0
	for _, c := range changes {
		budget += len(c.Before) + len(c.After)
	}
	for _, c := range t.Undo {
		budget += len(c.Before) + len(c.After)
	}
	if budget > 512*1024 {
		return errors.New("角色迁移恢复日志超过预算，未修改任何连接")
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	undo := append([]change(nil), t.Undo...)
	for _, c := range changes {
		found := false
		for i, old := range undo {
			if old.Path != c.Path {
				continue
			}
			if c.Path != paths.Runtime && c.Path != paths.NMConfig && !bytes.Equal(c.Before, old.After) {
				return errors.New("原迁移配置已被修改，保留现场且不覆盖回退基线")
			}
			undo[i].After = c.After
			found = true
			break
		}
		if !found {
			undo = append(undo, c)
		}
	}
	t.Phase = "prepared"
	t.Changes = changes
	if err = journal(paths, t); err != nil {
		return err
	}
	for _, c := range changes {
		if err = atomic(c.Path, c.After, os.FileMode(c.Mode)); err != nil {
			return errors.Join(err, recoverPrepared(paths, &t))
		}
	}
	t.Phase = "committed"
	t.Undo = undo
	t.Changes = nil
	return journal(paths, t)
}
