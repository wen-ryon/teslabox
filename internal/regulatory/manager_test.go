package regulatory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type testRunner struct {
	mu        sync.Mutex
	country   string
	output    string
	fail      bool
	calls     []string
	ignoreSet bool
}

func (r *testRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if len(args) == 3 && args[1] == "set" {
		if r.fail {
			return nil, errors.New("permission denied")
		}
		if !r.ignoreSet {
			r.country = args[2]
		}
		return nil, nil
	}
	if r.output != "" {
		return []byte(r.output), nil
	}
	return []byte("global\ncountry " + r.country + ": DFS-UNSET\n"), nil
}

func testPaths(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	return Paths{CountryFile: filepath.Join(root, "config", "wireless-country"), LockFile: filepath.Join(root, "run", "region.lock")}
}

func TestInitializePreservesExistingCountry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, saved, legacy, live, want string
		wantError                       bool
	}{
		{name: "domestic default", live: "00", want: "CN"},
		{name: "saved region", saved: "DE\n", legacy: "country=US\n", live: "CN", want: "DE"},
		{name: "explicit saved world", saved: "00\n", live: "00", want: "00"},
		{name: "legacy supplicant", legacy: "country=JP\n", live: "00", want: "JP"},
		{name: "legacy crda", legacy: "REGDOMAIN=\"US\"\n", live: "00", want: "US"},
		{name: "existing active region", live: "GB", want: "GB"},
		{name: "invalid saved region", saved: "not-a-country\n", live: "00", wantError: true},
		{name: "invalid existing region", legacy: "country=ZZ\n", live: "00", wantError: true},
		{name: "malformed existing region", legacy: "country=CHINA\n", live: "00", wantError: true},
		{name: "empty legacy value is unconfigured", legacy: "REGDOMAIN=\"\"\n", live: "00", want: "CN"},
		{name: "explicit legacy world", legacy: "country=00\n", live: "CN", want: "00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := testPaths(t)
			if tc.saved != "" {
				os.MkdirAll(filepath.Dir(paths.CountryFile), 0o755)
				if err := os.WriteFile(paths.CountryFile, []byte(tc.saved), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.legacy != "" {
				legacy := filepath.Join(t.TempDir(), "legacy.conf")
				os.WriteFile(legacy, []byte(tc.legacy), 0o600)
				paths.LegacyFiles = []string{legacy}
			}
			manager := New(&testRunner{country: tc.live}, paths)
			err := manager.Initialize(t.Context())
			if tc.wantError {
				if err == nil {
					t.Fatal("invalid existing config must not default CN")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := readCountry(paths.CountryFile)
			if err != nil || got != tc.want {
				t.Fatalf("country=%s error=%v", got, err)
			}
			before, _ := os.ReadFile(paths.CountryFile)
			if err := manager.Initialize(t.Context()); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(paths.CountryFile)
			if string(before) != string(after) {
				t.Fatal("repeated initialization overwrote config")
			}
		})
	}
}

func TestInitializeModuleOptionsAndConflict(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, options, env string
		wantError          bool
	}{
		{name: "existing module option", options: "options cfg80211 ieee80211_regdom=US\n"},
		{name: "module option whitespace", options: "options   cfg80211\tieee80211_regdom=US\n"},
		{name: "conflicting existing configurations", options: "options cfg80211 ieee80211_regdom=US\n", env: "CN", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := testPaths(t)
			dir := t.TempDir()
			paths.ModuleDirectories = []string{dir}
			paths.EnvironmentCountry = tc.env
			os.WriteFile(filepath.Join(dir, "wifi.conf"), []byte(tc.options), 0o644)
			m := New(&testRunner{country: "00"}, paths)
			err := m.Initialize(t.Context())
			if tc.wantError {
				if err == nil {
					t.Fatal("expected conflict")
				}
				if _, err := os.Stat(paths.CountryFile); !os.IsNotExist(err) {
					t.Fatal("conflict created default")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, _ := readCountry(paths.CountryFile)
			if got != "US" {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestInitializeImportsEnvironmentAndBootConfiguration(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"environment", "kernel command line", "module parameter"} {
		t.Run(source, func(t *testing.T) {
			paths := testPaths(t)
			switch source {
			case "environment":
				paths.EnvironmentCountry = "JP"
			case "kernel command line", "module parameter":
				file := filepath.Join(t.TempDir(), "source")
				content := "JP\n"
				if source == "kernel command line" {
					content = "console=ttyS2 cfg80211.ieee80211_regdom=JP rootwait\n"
					paths.KernelCommandLine = file
				} else {
					paths.ModuleParameter = file
				}
				if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := New(&testRunner{country: "00"}, paths).Initialize(t.Context()); err != nil {
				t.Fatal(err)
			}
			// 模拟下次 OTA 启动，不再依赖当时的环境或运行域。
			paths.EnvironmentCountry = "CN"
			if err := New(&testRunner{country: "00"}, paths).Initialize(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got, err := readCountry(paths.CountryFile); err != nil || got != "JP" {
				t.Fatalf("country=%q error=%v", got, err)
			}
		})
	}
}

func TestCanceledInitializationDoesNotCreateDefault(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := New(&testRunner{country: "00"}, paths).Initialize(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if _, err := os.Stat(paths.CountryFile); !os.IsNotExist(err) {
		t.Fatal("canceled initialization created configuration")
	}
}

func TestSetReportsSavedAndEffectiveSeparately(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		fail, ignore bool
		want         string
	}{
		{name: "applied", want: "applied"}, {name: "application failed", fail: true, want: "pending"},
		{name: "kernel did not confirm", ignore: true, want: "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := testPaths(t)
			r := &testRunner{country: "00", fail: tc.fail, ignoreSet: tc.ignore}
			m := New(r, paths)
			status, err := m.Set(t.Context(), "cn")
			if err != nil {
				t.Fatal(err)
			}
			if !status.Saved || status.ConfiguredCountry != "CN" || status.State != tc.want {
				t.Fatalf("%+v", status)
			}
			if tc.want != "applied" && status.EffectiveCountry == "CN" {
				t.Fatal("saved config used as actual country")
			}
		})
	}
}

func TestParseDomainsDoesNotUseGlobalAsPHY(t *testing.T) {
	t.Parallel()
	global, phys := ParseDomains("global\ncountry CN: DFS-FCC\nphy#0 (self-managed)\ncountry US: DFS-FCC\nphy#1\ncountry CN: DFS-FCC\n")
	if global != "CN" || len(phys) != 2 || !phys[0].SelfManaged || phys[0].Country != "US" {
		t.Fatalf("%s %+v", global, phys)
	}
	paths := testPaths(t)
	m := New(&testRunner{output: "global\ncountry CN: DFS-FCC\nphy#0 (self-managed)\ncountry US: DFS-FCC\n"}, paths)
	if err := writeCountry(paths.CountryFile, "CN", false); err != nil {
		t.Fatal(err)
	}
	if status := m.Status(t.Context()); status.State != "restricted" {
		t.Fatalf("%+v", status)
	}
}

func TestSetRejectsInvalidCountriesBeforeWritingOrExecuting(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"ZZ", "EU", "CN;reboot", "CN\nUS", "/CN", "C", "CHINA"} {
		t.Run(value, func(t *testing.T) {
			paths := testPaths(t)
			r := &testRunner{country: "00"}
			m := New(r, paths)
			if _, err := m.Set(t.Context(), value); err == nil {
				t.Fatal("expected invalid country")
			}
			if len(r.calls) != 0 {
				t.Fatalf("unexpected command: %v", r.calls)
			}
			if _, err := os.Stat(paths.CountryFile); !os.IsNotExist(err) {
				t.Fatal("unexpected config write")
			}
		})
	}
}

func TestSetRefusesSymlinkCountryFile(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	os.MkdirAll(filepath.Dir(paths.CountryFile), 0o755)
	target := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(target, []byte("US\n"), 0o644)
	if err := os.Symlink(target, paths.CountryFile); err != nil {
		t.Skip("symlink unavailable", err)
	}
	m := New(&testRunner{country: "US"}, paths)
	if _, err := m.Set(t.Context(), "CN"); err == nil {
		t.Fatal("symlink accepted")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "US\n" {
		t.Fatal("outside file overwritten")
	}
}
