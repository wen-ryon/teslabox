package automation

import (
	"bytes"
	"context"
	"strings"
	"testing"

	buildversion "github.com/wen-ryon/teslabox/internal/version"
)

func TestParseRadioConfig(t *testing.T) {
	for _, test := range []struct {
		band, channel string
		wantBand      string
		wantChannel   int
		wantError     bool
	}{
		{band: "2.4g", channel: "6", wantBand: "bg", wantChannel: 6},
		{band: "5GHz", channel: "auto", wantBand: "a"},
		{band: "6g", channel: "1", wantError: true},
		{band: "5g", channel: "bad", wantError: true},
	} {
		band, channel, err := parseRadioConfig(test.band, test.channel)
		if (err != nil) != test.wantError || band != test.wantBand || channel != test.wantChannel {
			t.Errorf("parseRadioConfig(%q, %q) = (%q, %d, %v)", test.band, test.channel, band, channel, err)
		}
	}
}

func TestRunCLIVersionDoesNotRequireService(t *testing.T) {
	oldVersion, oldCommit := buildversion.Version, buildversion.Commit
	buildversion.Version, buildversion.Commit = "0.1.31", "abc123"
	t.Cleanup(func() {
		buildversion.Version, buildversion.Commit = oldVersion, oldCommit
	})

	for _, argument := range []string{"-v", "--version", "version"} {
		t.Run(argument, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := RunCLI(context.Background(), "/missing/service.sock", []string{argument}, &stdout, &stderr); code != 0 {
				t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
			}
			if output := strings.TrimSpace(stdout.String()); output != "TeslaBox 0.1.31 (commit abc123)" {
				t.Fatalf("version output = %q", output)
			}
		})
	}
}
