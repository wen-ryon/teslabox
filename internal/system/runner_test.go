package system

import (
	"testing"
	"time"
)

func TestParseFFmpegProgress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		line string
		want time.Duration
		ok   bool
	}{
		{line: "out_time_us=12500000", want: 12500 * time.Millisecond, ok: true},
		{line: "progress=continue"},
		{line: "out_time_us=-1"},
		{line: "out_time_us=invalid"},
	}
	for _, test := range tests {
		got, ok := parseFFmpegProgress(test.line)
		if got != test.want || ok != test.ok {
			t.Errorf("parseFFmpegProgress(%q) = %s, %v; want %s, %v", test.line, got, ok, test.want, test.ok)
		}
	}
}
