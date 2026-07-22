package imageinit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wen-ryon/teslabox/internal/system"
)

const (
	minimumImageSize          = 32 * 1000 * 1000 * 1000
	minimumReserve            = 16 * 1000 * 1000 * 1000
	minimumTeslaCamPartition  = 32 * 1000 * 1000 * 1000
	minimumMusicPartition     = 1 * 1000 * 1000 * 1000
	minimumTeslaCamPercentage = 10
	maximumTeslaCamPercentage = 95
	bytesPerMiB               = 1024 * 1024
)

// Options 保存虚拟磁盘初始化的显式参数。
type Options struct {
	DataRoot        string
	ImagePath       string
	SizeBytes       int64
	Label           string
	MusicEnabled    bool
	TeslaCamPercent int
	MusicLabel      string
	Execute         bool
}

// Layout 描述车机最终看到的两个分区容量，便于页面准确预览初始化结果。
type Layout struct {
	TeslaCamBytes int64
	MusicBytes    int64
}

// Environment 描述镜像创建前经过核验的 backing store。
type Environment struct {
	DataRoot    string
	ImagePath   string
	FileSystem  string
	Reflink     bool
	TotalBytes  int64
	FreeBytes   int64
	ImageExists bool
}

// Command 是不会经过 shell 解释的初始化命令。
type Command struct {
	Name string
	Args []string
}

// Result 返回环境信息、执行计划和最终 loop 设备。
type Result struct {
	Environment Environment
	Commands    []Command
	LoopDevice  string
	Layout      Layout
	WasExecuted bool
}

// Inspector 隔离真实文件系统和挂载信息读取。
type Inspector interface {
	Inspect(ctx context.Context, options Options) (Environment, error)
}

// OSInspector 从 Linux 挂载表和 XFS 元数据核验 backing store。
type OSInspector struct {
	runner system.Runner
}

// NewOSInspector 创建只读环境检查器。
func NewOSInspector(runner system.Runner) *OSInspector {
	return &OSInspector{runner: runner}
}

// Inspect 确认镜像位于目标 XFS 挂载内且 reflink 已开启。
func (i *OSInspector) Inspect(ctx context.Context, options Options) (Environment, error) {
	resolvedRoot, err := filepath.EvalSymlinks(options.DataRoot)
	if err != nil {
		return Environment{}, fmt.Errorf("解析数据盘路径: %w", err)
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(options.ImagePath))
	if err != nil {
		return Environment{}, fmt.Errorf("解析镜像目录: %w", err)
	}
	resolvedImage := filepath.Join(resolvedParent, filepath.Base(options.ImagePath))
	if !isWithinRoot(filepath.ToSlash(resolvedRoot), filepath.ToSlash(resolvedImage)) {
		return Environment{}, errors.New("镜像真实路径不在数据盘目录内")
	}

	environment := Environment{DataRoot: resolvedRoot, ImagePath: resolvedImage}
	if _, err := os.Lstat(resolvedImage); err == nil {
		environment.ImageExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return Environment{}, fmt.Errorf("检查镜像文件: %w", err)
	}

	findmntOutput, err := i.runner.Run(ctx, "findmnt", "--noheadings", "--output", "TARGET,FSTYPE", "--target", resolvedRoot)
	if err != nil {
		return Environment{}, fmt.Errorf("读取数据盘挂载信息: %w", err)
	}
	fields := strings.Fields(string(findmntOutput))
	if len(fields) != 2 || filepath.Clean(fields[0]) != filepath.Clean(resolvedRoot) {
		return Environment{}, fmt.Errorf("数据目录不是独立挂载点: %q", strings.TrimSpace(string(findmntOutput)))
	}
	environment.FileSystem = fields[1]

	xfsOutput, err := i.runner.Run(ctx, "/usr/sbin/xfs_spaceman", "-c", "info", resolvedRoot)
	if err != nil {
		return Environment{}, fmt.Errorf("读取 XFS 特性: %w", err)
	}
	environment.Reflink = strings.Contains(string(xfsOutput), "reflink=1")

	statOutput, err := i.runner.Run(ctx, "stat", "--file-system", "--format=%S:%b:%a", resolvedRoot)
	if err != nil {
		return Environment{}, fmt.Errorf("读取数据盘容量: %w", err)
	}
	blockSize, totalBlocks, freeBlocks, err := parseStatFS(string(statOutput))
	if err != nil {
		return Environment{}, err
	}
	environment.TotalBytes, err = checkedMultiply(blockSize, totalBlocks)
	if err != nil {
		return Environment{}, fmt.Errorf("计算数据盘总容量: %w", err)
	}
	environment.FreeBytes, err = checkedMultiply(blockSize, freeBlocks)
	if err != nil {
		return Environment{}, fmt.Errorf("计算数据盘可用容量: %w", err)
	}
	return environment, nil
}

// Initializer 在完成环境核验后创建可供 Gadget 导出的 TeslaCam 镜像。
type Initializer struct {
	inspector Inspector
	runner    system.Runner
	lookPath  func(string) (string, error)
}

// New 创建镜像初始化器。
func New(inspector Inspector, runner system.Runner) *Initializer {
	return &Initializer{inspector: inspector, runner: runner, lookPath: exec.LookPath}
}

// Run 默认只返回计划，Execute 为真时才创建镜像和文件系统。
func (i *Initializer) Run(ctx context.Context, options Options) (result Result, err error) {
	if err := validateOptions(options); err != nil {
		return Result{}, err
	}
	layout, err := PlanLayout(options.SizeBytes, options.MusicEnabled, options.TeslaCamPercent)
	if err != nil {
		return Result{}, err
	}
	environment, err := i.inspector.Inspect(ctx, options)
	if err != nil {
		return Result{}, err
	}
	if err := validateEnvironment(environment, options.SizeBytes); err != nil {
		return Result{}, err
	}

	commands := buildPlan(environment.ImagePath, options, layout)
	result = Result{Environment: environment, Commands: commands, Layout: layout}
	if !options.Execute {
		return result, nil
	}
	tools, err := i.requiredFormattingTools(options)
	if err != nil {
		return result, err
	}

	created := false
	defer func() {
		if err != nil && created {
			if removeErr := os.Remove(environment.ImagePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("清理未完成镜像: %w", removeErr))
			}
		}
	}()

	for _, command := range imageCreationCommands(environment.ImagePath, options, layout) {
		if _, err = i.runner.Run(ctx, command.Name, command.Args...); err != nil {
			return result, fmt.Errorf("创建镜像步骤 %s 失败: %w", command.Name, err)
		}
		created = true
	}

	loopOutput, runErr := i.runner.Run(ctx, "losetup", "--find", "--show", "--partscan", environment.ImagePath)
	if runErr != nil {
		return result, fmt.Errorf("关联镜像 loop 设备: %w", runErr)
	}
	loopDevice := strings.TrimSpace(string(loopOutput))
	if !strings.HasPrefix(loopDevice, "/dev/loop") || strings.ContainsAny(loopDevice, " \t\r\n") {
		return result, fmt.Errorf("losetup 返回了异常设备路径 %q", loopDevice)
	}
	result.LoopDevice = loopDevice
	loopAttached := true
	defer func() {
		if loopAttached {
			if _, detachErr := i.runner.Run(context.WithoutCancel(ctx), "losetup", "--detach", loopDevice); detachErr != nil {
				err = errors.Join(err, fmt.Errorf("解除 loop 设备: %w", detachErr))
			}
		}
	}()

	partition := loopDevice + "p1"
	if _, err = i.runner.Run(ctx, "udevadm", "settle"); err != nil {
		return result, fmt.Errorf("等待镜像分区设备: %w", err)
	}
	if _, err = i.runner.Run(ctx, tools.exFAT, "-L", options.Label, partition); err != nil {
		return result, fmt.Errorf("格式化 TeslaCam 分区: %w", err)
	}
	if options.MusicEnabled {
		if _, err = i.runner.Run(ctx, tools.vfat, "-F", "32", "-n", options.MusicLabel, loopDevice+"p2"); err != nil {
			return result, fmt.Errorf("格式化音乐分区: %w", err)
		}
	}

	mountPath, err := os.MkdirTemp("/run", "teslabox-image-init-")
	if err != nil {
		return result, fmt.Errorf("创建镜像临时挂载点: %w", err)
	}
	defer os.Remove(mountPath)
	mounted := false
	defer func() {
		if mounted {
			if _, unmountErr := i.runner.Run(context.WithoutCancel(ctx), "umount", mountPath); unmountErr != nil {
				err = errors.Join(err, fmt.Errorf("卸载镜像临时挂载点: %w", unmountErr))
			}
		}
	}()
	if _, err = i.runner.Run(ctx, "/sbin/mount.exfat-fuse", partition, mountPath); err != nil {
		return result, fmt.Errorf("挂载 TeslaCam 分区: %w", err)
	}
	mounted = true
	if err = os.Mkdir(filepath.Join(mountPath, "TeslaCam"), 0o755); err != nil {
		return result, fmt.Errorf("创建 TeslaCam 目录: %w", err)
	}
	if _, err = i.runner.Run(ctx, "sync"); err != nil {
		return result, fmt.Errorf("同步 TeslaCam 镜像: %w", err)
	}
	if _, err = i.runner.Run(ctx, "umount", mountPath); err != nil {
		return result, fmt.Errorf("卸载 TeslaCam 镜像: %w", err)
	}
	mounted = false
	if _, err = i.runner.Run(ctx, "losetup", "--detach", loopDevice); err != nil {
		return result, fmt.Errorf("解除 TeslaCam loop 设备: %w", err)
	}
	loopAttached = false
	result.WasExecuted = true
	return result, nil
}

type formattingTools struct {
	exFAT string
	vfat  string
}

// requiredFormattingTools 在写入镜像前验证格式化程序，避免已经擦除数据盘后才因缺少系统依赖而中断。
func (i *Initializer) requiredFormattingTools(options Options) (formattingTools, error) {
	exFAT, err := i.findTool("mkfs.exfat", "exfatprogs")
	if err != nil {
		return formattingTools{}, err
	}
	tools := formattingTools{exFAT: exFAT}
	if !options.MusicEnabled {
		return tools, nil
	}
	vfat, err := i.findTool("mkfs.vfat", "dosfstools")
	if err != nil {
		return formattingTools{}, err
	}
	tools.vfat = vfat
	return tools, nil
}

func (i *Initializer) findTool(name, packageName string) (string, error) {
	if i.lookPath == nil {
		return "", errors.New("格式化程序检查器不可用")
	}
	for _, candidate := range []string{"/usr/sbin/" + name, "/sbin/" + name, name} {
		if resolved, err := i.lookPath(candidate); err == nil {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("缺少 %s；请先执行 sudo apt-get install -y %s，再重新初始化", name, packageName)
}

func validateOptions(options Options) error {
	root := path.Clean(strings.ReplaceAll(strings.TrimSpace(options.DataRoot), "\\", "/"))
	image := path.Clean(strings.ReplaceAll(strings.TrimSpace(options.ImagePath), "\\", "/"))
	if !path.IsAbs(root) || root == "/" {
		return errors.New("data-root 必须是非根目录的绝对路径")
	}
	if !path.IsAbs(image) || !isWithinRoot(root, image) {
		return errors.New("image 必须是 data-root 内的绝对路径")
	}
	if options.SizeBytes < minimumImageSize {
		return fmt.Errorf("size-bytes 不能小于 %d", minimumImageSize)
	}
	if strings.TrimSpace(options.Label) == "" || len(options.Label) > 15 || strings.ContainsAny(options.Label, "/\\\t\r\n") {
		return errors.New("label 必须是 1 到 15 字节且不含路径分隔符或空白的 exFAT 卷标")
	}
	if options.MusicEnabled {
		if options.TeslaCamPercent < minimumTeslaCamPercentage || options.TeslaCamPercent > maximumTeslaCamPercentage {
			return fmt.Errorf("TeslaCam 分区比例必须在 %d%% 到 %d%% 之间", minimumTeslaCamPercentage, maximumTeslaCamPercentage)
		}
		if strings.TrimSpace(options.MusicLabel) == "" || len(options.MusicLabel) > 11 || strings.ContainsAny(options.MusicLabel, "/\\\t\r\n") {
			return errors.New("music-label 必须是 1 到 11 字节且不含路径分隔符或空白的 FAT32 卷标")
		}
	}
	return nil
}

func validateEnvironment(environment Environment, imageSize int64) error {
	if environment.FileSystem != "xfs" {
		return fmt.Errorf("backing store 不是 XFS: %q", environment.FileSystem)
	}
	if !environment.Reflink {
		return errors.New("backing store 未启用 XFS reflink")
	}
	if environment.ImageExists {
		return fmt.Errorf("镜像已存在，拒绝覆盖: %s", environment.ImagePath)
	}
	if environment.TotalBytes-imageSize < minimumReserve {
		return fmt.Errorf("镜像过大，必须为快照至少预留 %d 字节", minimumReserve)
	}
	if environment.FreeBytes < minimumReserve {
		return fmt.Errorf("数据盘剩余空间不足 %d 字节", minimumReserve)
	}
	return nil
}

func buildPlan(imagePath string, options Options, layout Layout) []Command {
	commands := imageCreationCommands(imagePath, options, layout)
	commands = append(commands,
		Command{Name: "losetup", Args: []string{"--find", "--show", "--partscan", imagePath}},
		Command{Name: "mkfs.exfat", Args: []string{"-L", options.Label, "<loop-device>p1"}},
	)
	if options.MusicEnabled {
		commands = append(commands, Command{Name: "mkfs.vfat", Args: []string{"-F", "32", "-n", options.MusicLabel, "<loop-device>p2"}})
	}
	commands = append(commands,
		Command{Name: "/sbin/mount.exfat-fuse", Args: []string{"<loop-device>p1", "<temporary-mount>"}},
		Command{Name: "mkdir", Args: []string{"<temporary-mount>/TeslaCam"}},
		Command{Name: "sync"},
		Command{Name: "umount", Args: []string{"<temporary-mount>"}},
		Command{Name: "losetup", Args: []string{"--detach", "<loop-device>"}},
	)
	return commands
}

func imageCreationCommands(imagePath string, options Options, layout Layout) []Command {
	commands := []Command{
		{Name: "truncate", Args: []string{"--size", strconv.FormatInt(options.SizeBytes, 10), imagePath}},
		{Name: "parted", Args: []string{"--script", imagePath, "mklabel", "msdos"}},
	}
	// exFAT 与 NTFS 在 MBR 中共用 0x07 类型码；先声明 ntfs 才能稳定写入车机需要的分区类型码。
	if !options.MusicEnabled {
		return append(commands, Command{Name: "parted", Args: []string{"--script", "--align", "optimal", imagePath, "mkpart", "primary", "ntfs", "1MiB", "100%"}})
	}
	teslaCamEndMiB := 1 + layout.TeslaCamBytes/bytesPerMiB
	boundary := strconv.FormatInt(teslaCamEndMiB, 10) + "MiB"
	return append(commands,
		Command{Name: "parted", Args: []string{"--script", "--align", "optimal", imagePath, "mkpart", "primary", "ntfs", "1MiB", boundary}},
		Command{Name: "parted", Args: []string{"--script", "--align", "optimal", imagePath, "mkpart", "primary", "fat32", boundary, "100%"}},
	)
}

// PlanLayout 在执行任何磁盘写入前验证分区比例并返回实际 MiB 对齐容量。
func PlanLayout(sizeBytes int64, musicEnabled bool, teslaCamPercent int) (Layout, error) {
	usableMiB := sizeBytes/bytesPerMiB - 1
	if usableMiB <= 0 {
		return Layout{}, errors.New("镜像容量不足以创建分区")
	}
	if !musicEnabled {
		return Layout{TeslaCamBytes: usableMiB * bytesPerMiB}, nil
	}
	if teslaCamPercent < minimumTeslaCamPercentage || teslaCamPercent > maximumTeslaCamPercentage {
		return Layout{}, fmt.Errorf("TeslaCam 分区比例必须在 %d%% 到 %d%% 之间", minimumTeslaCamPercentage, maximumTeslaCamPercentage)
	}
	teslaCamMiB := usableMiB * int64(teslaCamPercent) / 100
	musicMiB := usableMiB - teslaCamMiB
	layout := Layout{TeslaCamBytes: teslaCamMiB * bytesPerMiB, MusicBytes: musicMiB * bytesPerMiB}
	if layout.TeslaCamBytes < minimumTeslaCamPartition {
		return Layout{}, fmt.Errorf("TeslaCam 分区不能小于 %d 字节", minimumTeslaCamPartition)
	}
	if layout.MusicBytes < minimumMusicPartition {
		return Layout{}, fmt.Errorf("音乐分区不能小于 %d 字节", minimumMusicPartition)
	}
	return layout, nil
}

func isWithinRoot(root, target string) bool {
	root = path.Clean(root)
	target = path.Clean(target)
	return target != root && strings.HasPrefix(target, root+"/")
}

func parseStatFS(output string) (int64, int64, int64, error) {
	parts := strings.Split(strings.TrimSpace(output), ":")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("解析 stat 输出: %q", strings.TrimSpace(output))
	}
	values := make([]int64, 3)
	for index, part := range parts {
		value, err := strconv.ParseInt(part, 10, 64)
		if err != nil || value <= 0 {
			return 0, 0, 0, fmt.Errorf("解析 stat 数值 %q", part)
		}
		values[index] = value
	}
	return values[0], values[1], values[2], nil
}

func checkedMultiply(left, right int64) (int64, error) {
	if left > math.MaxInt64/right {
		return 0, errors.New("容量数值溢出")
	}
	return left * right, nil
}
