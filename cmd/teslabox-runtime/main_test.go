package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wen-ryon/teslabox/internal/runtimecomponents"
)

func TestReleaseValidationRequiresAllBoundInputs(t *testing.T) {
	root := t.TempDir()
	// 空输入只检查描述；一旦提供归档，就必须完整验证三项。
	d := runtimecomponents.Descriptor{Schema: 1}
	for _, id := range []string{"media", "carplay", "nas"} {
		format := "tar.gz"
		if id == "nas" {
			format = "zip"
		}
		d.Components = append(d.Components, runtimecomponents.Component{ID: id, Version: "1.0.0", URL: "https://example.invalid/fixed", Size: 1, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Format: format})
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := filepath.Join(root, "descriptor.json")
	if err := os.WriteFile(descriptor, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"-validate-descriptor", descriptor}); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(root, "main")
	fake := append([]byte{127, 69, 76, 70}, make([]byte, 60)...)
	fake = append(fake, b...)
	if err := os.WriteFile(main, fake, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"-validate-descriptor", descriptor, "-bound-main", main}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(main, bytes.Repeat([]byte{0}, 128), 0600); err != nil {
		t.Fatal(err)
	}
	cases := [][]string{
		{"-validate-descriptor", descriptor, "-bound-main", main},
		{"-validate-descriptor", descriptor, "-seed-dir", root},
		{"-validate-descriptor", descriptor, "-input", "nas=missing"},
		{"-input", "nas=missing"},
		{"-bound-main", main},
		{"unexpected"},
	}
	for _, args := range cases {
		if err := run(context.Background(), args); err == nil {
			t.Fatalf("接受无效操作 %v", args)
		}
	}
}
