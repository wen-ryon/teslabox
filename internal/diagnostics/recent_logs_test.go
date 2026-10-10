package diagnostics

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBundleNetworkBudgetKeepsLatestFailure(t *testing.T) {
	t.Parallel()
	var input strings.Builder
	for i := 0; i < 499; i++ {
		fmt.Fprintf(&input, "2026-10-07T04:%03d NetworkManager old-record-%d %s\n", i, i, strings.Repeat("旧网络记录", 40))
	}
	input.WriteString("2026-10-07T08:41:30 NetworkManager latest-disconnect-reason password=private-value\n")
	bundle, err := New(&fakeRunner{output: input.String()}).Bundle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	report := string(bundle.Content)
	start := strings.Index(report, "===== 当前启动日志: network =====")
	if start < 0 {
		t.Fatal("missing network section")
	}
	section := report[start:]
	if end := strings.Index(section[len("===== 当前启动日志: network ====="):], "\n====="); end >= 0 {
		section = section[:len("===== 当前启动日志: network =====")+end]
	}
	if !strings.Contains(section, "latest-disconnect-reason") || strings.Contains(section, "old-record-0 ") ||
		!strings.Contains(section, "较旧日志") || strings.Contains(section, "private-value") {
		t.Fatal("latest failure lost, old records preferred, or secret leaked")
	}
	if len(section) > 64<<10 || len(bundle.Content) > maximumBundle || !utf8.Valid(bundle.Content) {
		t.Fatal("report budget or UTF-8 boundary violated")
	}
}

func TestNetworkCategoryBudgetKeepsLatestFailure(t *testing.T) {
	t.Parallel()
	var input strings.Builder
	for i := 0; i < 499; i++ {
		fmt.Fprintf(&input, "NetworkManager old-record-%d %s\n", i, strings.Repeat("旧网络记录", 150))
	}
	input.WriteString("NetworkManager latest-disconnect-reason password=private-value\n")
	bundle, err := New(&fakeRunner{output: input.String()}).BundleForSource(t.Context(), SourceNetwork)
	if err != nil {
		t.Fatal(err)
	}
	report := string(bundle.Content)
	if strings.Count(report, "latest-disconnect-reason") != 2 || strings.Contains(report, "old-record-0 ") ||
		strings.Count(report, "较旧网络日志") != 2 || strings.Contains(report, "private-value") {
		t.Fatal("category dropped latest failure, retained oldest records, or leaked secret")
	}
	if len(bundle.Content) > 2*(128<<10)+1024 || !utf8.Valid(bundle.Content) {
		t.Fatal("category budget or UTF-8 boundary violated")
	}
}
