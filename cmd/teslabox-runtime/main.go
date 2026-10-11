package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/wen-ryon/teslabox/internal/runtimecomponents"
)

type inputs []string

func (i *inputs) String() string     { return strings.Join(*i, ",") }
func (i *inputs) Set(s string) error { *i = append(*i, s); return nil }

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("teslabox-runtime", flag.ContinueOnError)
	descriptor := flags.String("validate-descriptor", "", "只校验发布描述；不安装组件")
	seed := flags.String("seed-dir", "", "为编译绑定的组件预缓存离线归档；不激活运行时")
	root := flags.String("cache-root", "/var/lib/teslabox-system/runtime-cache", "离线缓存位置")
	boundMain := flags.String("bound-main", "", "校验公开描述是否随主 ELF 编译绑定")
	describe := flags.Bool("describe", false, "输出编译绑定的公开组件描述")
	var paths inputs
	flags.Var(&paths, "input", "发布输入 id=绝对归档路径，可重复")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("不接受额外位置参数")
	}
	d, available, err := runtimecomponents.Default()
	if err != nil {
		return err
	}
	if *descriptor != "" {
		if *seed != "" || *describe {
			return errors.New("发布校验与设备预缓存不能混用")
		}
		b, err := os.ReadFile(*descriptor)
		if err != nil {
			return err
		}
		d, err = runtimecomponents.Parse(b)
		if err != nil {
			return err
		}
		if *boundMain != "" {
			main, err := os.ReadFile(*boundMain)
			if err != nil {
				return err
			}
			if len(main) < 64 || len(main) > 128<<20 || !bytes.Equal(main[:4], []byte{127, 69, 76, 70}) || !bytes.Contains(main, b) {
				return errors.New("主 ELF 未绑定同一组件描述")
			}
		}
		seen := map[string]bool{}
		for _, value := range paths {
			id, path, ok := strings.Cut(value, "=")
			if !ok || seen[id] {
				return errors.New("输入名称重复或格式无效")
			}
			seen[id] = true
			found := false
			for _, c := range d.Components {
				if c.ID == id {
					found = true
					if err = runtimecomponents.ValidateFile(ctx, c, path); err != nil {
						return fmt.Errorf("%s 输入校验失败: %w", id, err)
					}
				}
			}
			if !found {
				return errors.New("未知发布输入")
			}
		}
		if len(paths) != 0 && len(seen) != 3 {
			return errors.New("发布校验必须提供全部三项输入")
		}
		return nil
	}
	if *boundMain != "" {
		return errors.New("主 ELF 绑定检查仅用于发布校验")
	}
	if len(paths) != 0 {
		return errors.New("设备不能替换编译绑定的输入")
	}
	if !available {
		return errors.New("此工具没有编译绑定的组件描述")
	}
	if *describe {
		if *seed != "" {
			return errors.New("描述输出与缓存不能混用")
		}
		return json.NewEncoder(os.Stdout).Encode(d)
	}
	if *seed == "" {
		return errors.New("请选择发布校验、描述输出或离线预缓存")
	}
	installers := map[string]runtimecomponents.Installer{}
	for _, c := range d.Components {
		installers[c.ID] = func(context.Context, runtimecomponents.Component, string) error {
			return errors.New("预缓存工具不激活组件")
		}
	}
	m, err := runtimecomponents.New(runtimecomponents.Config{Root: *root, Components: d.Components, Install: installers, RequireRoot: true})
	if err != nil {
		return err
	}
	for _, c := range d.Components {
		path := runtimecomponents.ArchivePath(*seed, c)
		if _, err = os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err = m.Seed(ctx, c.ID, filepath.Clean(path)); err != nil {
			return fmt.Errorf("%s 离线输入校验失败: %w", c.ID, err)
		}
	}
	return nil
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
