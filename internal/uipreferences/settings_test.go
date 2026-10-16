package uipreferences

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreDefaultAndRestartPreserveSharedLanguage(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state", "ui.json")
	store := New(path)
	value, err := store.Read()
	if err != nil || value.Language != "zh-CN" {
		t.Fatalf("默认语言：%#v %v", value, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("读取默认语言不应写入配置")
	}
	for _, language := range []string{"en", "zh-CN", "en"} {
		if _, err := store.Update(Settings{Language: language}); err != nil {
			t.Fatal(err)
		}
		value, err = New(path).Read()
		if err != nil || value.Language != language {
			t.Fatalf("重启或其他客户端读取：%#v %v", value, err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("不应修改语言之外的文件或遗留临时文件：%v %v", entries, err)
	}
	data, _ := os.ReadFile(path)
	if len(data) >= 3 && string(data[:3]) == "\xef\xbb\xbf" {
		t.Fatal("不允许 BOM")
	}
}

func TestStoreInvalidInputPreservesExistingLanguage(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ui.json")
	store := New(path)
	if _, err := store.Update(Settings{Language: "en"}); err != nil {
		t.Fatal(err)
	}
	for _, language := range []string{"", "CN", "en-US", "../en", `"><script>`, "EN"} {
		if _, err := store.Update(Settings{Language: language}); err == nil {
			t.Fatalf("接受无效语言：%q", language)
		}
		value, err := store.Read()
		if err != nil || value.Language != "en" {
			t.Fatalf("错误输入覆盖了配置：%#v %v", value, err)
		}
	}
}

func TestStoreCorruptAndOversizedStateIsNotSilentlyReset(t *testing.T) {
	for _, test := range []struct{ name, content string }{
		{"无效枚举", `{"language":"CN"}`},
		{"多余字段", `{"language":"en","country":"US"}`},
		{"多余对象", `{"language":"en"}{}`},
		{"无效JSON", `{`},
		{"超长状态", string(make([]byte, 4097))},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ui.json")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := New(path).Read(); err == nil {
				t.Fatal("不能把损坏状态当成有效默认值")
			}
			data, _ := os.ReadFile(path)
			if string(data) != test.content {
				t.Fatal("读取改写了原始状态")
			}
		})
	}
}

func TestStoreRejectsSymlinkFileAndParents(t *testing.T) {
	for _, parent := range []bool{false, true} {
		name := "文件链接"
		if parent {
			name = "父目录链接"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "target")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			original := filepath.Join(target, "ui.json")
			if err := os.WriteFile(original, []byte(`{"language":"en"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, "link")
			point := original
			if parent {
				point = target
			}
			if err := os.Symlink(point, link); err != nil {
				t.Skipf("当前环境不允许创建链接：%v", err)
			}
			path := link
			if parent {
				path = filepath.Join(link, "ui.json")
			}
			if _, err := New(path).Read(); err == nil {
				t.Fatal("读取不能跟随链接")
			}
			if _, err := New(path).Update(Settings{Language: "zh-CN"}); err == nil {
				t.Fatal("写入不能跟随链接")
			}
			data, _ := os.ReadFile(original)
			if string(data) != `{"language":"en"}` {
				t.Fatal("修改了链接目标")
			}
		})
	}
}
