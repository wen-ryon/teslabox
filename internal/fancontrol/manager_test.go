package fancontrol

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTemperatureRequest(t *testing.T) {
	settings := DefaultSettings()
	tests := []struct {
		name              string
		temperature, want int
	}{{"低于首档", 40, 0}, {"首档", 50, 35}, {"档位插值", 55, 45}, {"最高档", 90, 100}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := temperatureRequest(settings, tt.temperature); got != tt.want {
				t.Fatalf("temperatureRequest()=%d, want %d", got, tt.want)
			}
		})
	}
}

func TestValidateRejectsInvalidManualLevels(t *testing.T) {
	settings := DefaultSettings()
	settings.TemperatureMode = ModeManual
	settings.TemperatureLevels[1].TemperatureC = settings.TemperatureLevels[0].TemperatureC
	if err := validate(settings); err == nil {
		t.Fatal("validate() 应拒绝重复温度")
	}
}

func TestLimitedOutputAlwaysAppliesMaximum(t *testing.T) {
	if got := limitedOutput(true, 100, 50); got != 50 {
		t.Fatalf("limitedOutput()=%d, want 50", got)
	}
	if got := limitedOutput(true, 70, 50); got != 35 {
		t.Fatalf("limitedOutput()=%d, want 35", got)
	}
	if got := limitedOutput(false, 100, 100); got != 0 {
		t.Fatalf("disabled limitedOutput()=%d, want 0", got)
	}
}

func TestHardwareDutyPercentSupportsActiveLowFanPort(t *testing.T) {
	if got := hardwareDutyPercent(25, true); got != 75 {
		t.Fatalf("active-low duty=%d, want 75", got)
	}
	if got := hardwareDutyPercent(25, false); got != 25 {
		t.Fatalf("normal duty=%d, want 25", got)
	}
}

func TestWritePWMKeepsActiveLowZeroPercentOff(t *testing.T) {
	chip := t.TempDir()
	pwm := filepath.Join(chip, "pwm0")
	if err := os.Mkdir(pwm, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"duty_cycle", "enable"} {
		if err := os.WriteFile(filepath.Join(pwm, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager := &Manager{config: Config{PWMChipPath: chip, PeriodNS: 1000, ActiveLow: true}}
	if err := manager.writePWM(0); err != nil {
		t.Fatal(err)
	}
	duty, err := os.ReadFile(filepath.Join(pwm, "duty_cycle"))
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := os.ReadFile(filepath.Join(pwm, "enable"))
	if err != nil {
		t.Fatal(err)
	}
	if string(duty) != "1000" || string(enabled) != "1" {
		t.Fatalf("duty=%q enabled=%q, want 1000 and 1", duty, enabled)
	}
}

func TestInitializePWMLeavesMatchingEnabledChannelUntouched(t *testing.T) {
	chip := t.TempDir()
	pwm := filepath.Join(chip, "pwm0")
	if err := os.Mkdir(pwm, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"period": "1000\n", "duty_cycle": "650", "enable": "1"} {
		if err := os.WriteFile(filepath.Join(pwm, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager := &Manager{config: Config{PWMChipPath: chip, PeriodNS: 1000}}
	if err := manager.initializePWM(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"period": "1000\n", "duty_cycle": "650", "enable": "1"} {
		got, err := os.ReadFile(filepath.Join(pwm, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s=%q, want %q", name, got, want)
		}
	}
}

func TestInitializePWMConfiguresFreshZeroPeriodChannel(t *testing.T) {
	chip := t.TempDir()
	pwm := filepath.Join(chip, "pwm0")
	if err := os.Mkdir(pwm, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"period": "0", "duty_cycle": "0", "enable": "0"} {
		if err := os.WriteFile(filepath.Join(pwm, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager := &Manager{config: Config{PWMChipPath: chip, PeriodNS: 10000}}
	if err := manager.initializePWM(); err != nil {
		t.Fatal(err)
	}
	period, err := os.ReadFile(filepath.Join(pwm, "period"))
	if err != nil {
		t.Fatal(err)
	}
	if string(period) != "10000" {
		t.Fatalf("period=%q, want 10000", period)
	}
}

func TestNewImmediatelyStopsActiveLowFan(t *testing.T) {
	chip := t.TempDir()
	pwm := filepath.Join(chip, "pwm0")
	if err := os.Mkdir(pwm, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"period": "1000", "duty_cycle": "0", "enable": "0"} {
		if err := os.WriteFile(filepath.Join(pwm, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := New(Config{
		StatePath: filepath.Join(t.TempDir(), "fan.json"), PWMChipPath: chip,
		TemperaturePath: filepath.Join(t.TempDir(), "temp"), PeriodNS: 1000, ActiveLow: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !manager.available {
		t.Fatalf("manager unavailable: %s", manager.reason)
	}
	duty, err := os.ReadFile(filepath.Join(pwm, "duty_cycle"))
	if err != nil {
		t.Fatal(err)
	}
	if string(duty) != "1000" {
		t.Fatalf("duty_cycle=%q, want 1000 for active-low stop", duty)
	}
	enabled, err := os.ReadFile(filepath.Join(pwm, "enable"))
	if err != nil {
		t.Fatal(err)
	}
	if string(enabled) != "1" {
		t.Fatalf("enable=%q, want 1", enabled)
	}
}
