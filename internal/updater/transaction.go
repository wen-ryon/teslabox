package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// 更新记录独立于下载请求保存；未提交的事务不能因重启而被当成成功。
type updateTransaction struct {
	Version   int               `json:"version"`
	Phase     string            `json:"phase"`
	Files     []transactionFile `json:"files"`
	Candidate *fileSnapshot     `json:"candidate,omitempty"`
}

type transactionFile struct {
	Target string        `json:"target"`
	Before fileSnapshot  `json:"before"`
	Failed *fileSnapshot `json:"failed,omitempty"`
}

type fileSnapshot struct {
	Exists bool   `json:"exists"`
	SHA256 string `json:"sha256,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
}

const transactionName = "transaction.json"
const commitPendingName = "commit-pending"
const commitPendingContent = "commit durability pending\n"

// 提交尝试前已持久保存的隔离标记，不依赖故障之后还能写入磁盘。
func commitPending(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, commitPendingName))
	return !errors.Is(err, os.ErrNotExist)
}

func clearCommitPending(dir string, syncDir func(string) error) error {
	if err := validateCommitPending(dir); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, commitPendingName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(dir)
}

func validateCommitPending(dir string) error {
	state, data, err := snapshotFile(filepath.Join(dir, commitPendingName), 128)
	if err != nil {
		return err
	}
	if state.Exists && string(data) != commitPendingContent {
		return errors.New("提交隔离标记损坏，保留现场")
	}
	return nil
}

func syncRestoredParent(target string, syncDir func(string) error) error {
	// 原状态可能连父目录都不存在，此时同步最近存在的祖先，不为恢复不存在状态新建目录。
	for dir := filepath.Dir(target); ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(dir); err == nil {
			return syncDir(dir)
		} else if !errors.Is(err, os.ErrNotExist) || filepath.Dir(dir) == dir {
			return err
		}
	}
}

// 此处拒绝目录链接，防止备份和恢复落到配置目录之外。
func plainPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) == path {
		return errors.New("事务路径必须是规范绝对路径且不能为根目录")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("事务路径不允许符号链接: %s", current)
		}
		if err == nil && info.IsDir() {
			if err := safeParentDirectory(info); err != nil {
				return fmt.Errorf("事务目录不安全 %s: %w", current, err)
			}
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}

func snapshotFile(path string, limit int64) (fileSnapshot, []byte, error) {
	if err := plainPath(path); err != nil {
		return fileSnapshot{}, nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{}, nil, nil
	}
	if err != nil {
		return fileSnapshot{}, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return fileSnapshot{}, nil, fmt.Errorf("事务文件类型、权限或大小不安全: %s", path)
	}
	if err := safeFileMetadata(info); err != nil {
		return fileSnapshot{}, nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return fileSnapshot{}, nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return fileSnapshot{}, nil, errors.New("事务文件在读取前被替换")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return fileSnapshot{}, nil, err
	}
	last, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, last) || last.Size() != info.Size() || !last.ModTime().Equal(info.ModTime()) || int64(len(data)) != info.Size() {
		return fileSnapshot{}, nil, errors.New("事务文件读取期间发生变化")
	}
	sum := sha256.Sum256(data)
	return fileSnapshot{Exists: true, SHA256: hex.EncodeToString(sum[:]), Mode: uint32(info.Mode().Perm())}, data, nil
}

func durableFile(path string, data []byte, mode os.FileMode) error {
	return durableFileWithSync(path, data, mode, syncDirectory)
}

func durableFileWithSync(path string, data []byte, mode os.FileMode, syncDir func(string) error) error {
	if err := plainPath(path); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".update-transaction-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func (tx *updateTransaction) save(dir string) error {
	return tx.saveWithSync(dir, syncDirectory)
}

func (tx *updateTransaction) saveWithSync(dir string, syncDir func(string) error) error {
	data, err := json.Marshal(tx)
	if err != nil {
		return err
	}
	return durableFileWithSync(filepath.Join(dir, transactionName), data, 0o600, syncDir)
}

// 调用方必须先停止服务并持有安装锁；所有备份落盘后才允许替换程序。
func prepareTransaction(dir string, targets []string) (*updateTransaction, error) {
	if err := plainPath(dir); err != nil {
		return nil, err
	}
	if err := durableMkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	if err := syncDirectory(filepath.Dir(dir)); err != nil {
		return nil, err
	}
	tx := &updateTransaction{Version: 1, Phase: "prepared"}
	seen := map[string]bool{}
	for i, target := range targets {
		if seen[target] {
			return nil, errors.New("事务目标重复")
		}
		seen[target] = true
		limit := int64(16 << 20)
		if i == 0 {
			limit = 128 << 20
		}
		state, data, err := snapshotFile(target, limit)
		if err != nil {
			return nil, err
		}
		if i == 0 && !state.Exists {
			return nil, errors.New("缺少可回滚的原程序")
		}
		if state.Exists {
			if err := durableFile(filepath.Join(dir, fmt.Sprintf("before-%d", i)), data, 0o600); err != nil {
				return nil, err
			}
		}
		tx.Files = append(tx.Files, transactionFile{Target: target, Before: state})
	}
	if err := tx.save(dir); err != nil {
		return nil, err
	}
	return tx, nil
}

func readTransaction(dir string, targets []string) (*updateTransaction, error) {
	if len(targets) == 0 {
		return nil, errors.New("事务目标不能为空")
	}
	_, data, err := snapshotFile(filepath.Join(dir, transactionName), 64<<10)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("事务记录缺失；保留现场，禁止覆盖备份")
	}
	var tx updateTransaction
	if err = json.Unmarshal(data, &tx); err != nil {
		return nil, err
	}
	if tx.Version != 1 || len(tx.Files) != len(targets) {
		return nil, errors.New("事务版本或目标数量不匹配")
	}
	switch tx.Phase {
	case "prepared", "activating", "ready", "restoring", "rolled_back", "committed":
	default:
		return nil, errors.New("未知事务阶段")
	}
	for i, file := range tx.Files {
		if file.Target != targets[i] {
			return nil, errors.New("事务路径与当前安装配置不一致")
		}
		if !validSnapshot(file.Before) || file.Failed != nil && !validSnapshot(*file.Failed) {
			return nil, errors.New("事务快照元数据无效")
		}
	}
	if !tx.Files[0].Before.Exists {
		return nil, errors.New("事务缺少原程序")
	}
	if tx.Candidate != nil && (!validSnapshot(*tx.Candidate) || !tx.Candidate.Exists) {
		return nil, errors.New("候选程序快照无效")
	}
	return &tx, nil
}

// 逐级同步父目录；即使目录已存在，也重做同步以覆盖上次 mkdir 后同步失败的重试。
func durableMkdirAll(path string, mode os.FileMode) error {
	return durableMkdirAllWith(path, mode, syncDirectory)
}

func durableMkdirAllWith(path string, mode os.FileMode, syncDir func(string) error) error {
	if err := plainPath(path); err != nil {
		return err
	}
	var paths []string
	for p := path; filepath.Dir(p) != p; p = filepath.Dir(p) {
		paths = append(paths, p)
	}
	for i := len(paths) - 1; i >= 0; i-- {
		p := paths[i]
		if err := os.Mkdir(p, mode); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("事务目录不是普通目录")
		}
		if err = plainPath(p); err != nil {
			return err
		}
		if err = syncDir(filepath.Dir(p)); err != nil {
			return err
		}
	}
	return syncDir(path)
}

func validSnapshot(s fileSnapshot) bool {
	if !s.Exists {
		return s.SHA256 == "" && s.Mode == 0
	}
	digest, err := hex.DecodeString(s.SHA256)
	return err == nil && len(digest) == sha256.Size && s.Mode & ^uint32(0o777) == 0
}

func sameSnapshot(a, b fileSnapshot) bool {
	return a.Exists == b.Exists && a.SHA256 == b.SHA256 && a.Mode == b.Mode
}

// 恢复先保存失败状态，再验证全部备份；中断后只接受原值或已保存的失败值。
func (tx *updateTransaction) restore(dir string) error {
	return tx.restoreWithSync(dir, syncDirectory)
}

func (tx *updateTransaction) restoreWithSync(dir string, syncDir func(string) error) error {
	if tx.Phase == "committed" {
		return errors.New("已提交事务不能作为手动降级的数据备份")
	}
	for i := range tx.Files {
		file := &tx.Files[i]
		limit := int64(16 << 20)
		if i == 0 {
			limit = 128 << 20
		}
		current, data, err := snapshotFile(file.Target, limit)
		if err != nil {
			return err
		}
		if file.Failed == nil {
			if current.Exists {
				if err := durableFileWithSync(filepath.Join(dir, fmt.Sprintf("failed-%d", i)), data, 0o600, syncDir); err != nil {
					return err
				}
			}
			file.Failed = &current
			if err := tx.saveWithSync(dir, syncDir); err != nil {
				return err
			}
		} else if !sameSnapshot(current, file.Before) && !sameSnapshot(current, *file.Failed) {
			return fmt.Errorf("事务目标存在未记录的新写入，拒绝覆盖: %s", file.Target)
		}
		if file.Failed.Exists {
			failed, _, err := snapshotFile(filepath.Join(dir, fmt.Sprintf("failed-%d", i)), limit)
			if err != nil {
				return err
			}
			if !failed.Exists || failed.SHA256 != file.Failed.SHA256 {
				return errors.New("失败现场备份摘要不匹配，停止恢复")
			}
		}
		if file.Before.Exists {
			backup, _, err := snapshotFile(filepath.Join(dir, fmt.Sprintf("before-%d", i)), limit)
			if err != nil {
				return err
			}
			if !backup.Exists || backup.SHA256 != file.Before.SHA256 {
				return errors.New("回滚备份摘要不匹配")
			}
		}
	}
	tx.Phase = "restoring"
	if err := tx.saveWithSync(dir, syncDir); err != nil {
		return err
	}
	// 状态先恢复，程序最后恢复；启动守卫在全部恢复之前不允许服务运行。
	for offset := len(tx.Files) - 1; offset >= 0; offset-- {
		file := tx.Files[offset]
		limit := int64(16 << 20)
		if offset == 0 {
			limit = 128 << 20
		}
		current, _, err := snapshotFile(file.Target, limit)
		if err != nil {
			return err
		}
		if sameSnapshot(current, file.Before) {
			// 上次 rename/remove 可能成功但目录同步失败；内容相等不代表恢复已持久完成。
			if err = syncRestoredParent(file.Target, syncDir); err != nil {
				return err
			}
			continue
		}
		if !sameSnapshot(current, *file.Failed) {
			return errors.New("恢复期间发现新的并发写入")
		}
		if file.Before.Exists {
			_, data, err := snapshotFile(filepath.Join(dir, fmt.Sprintf("before-%d", offset)), limit)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != file.Before.SHA256 {
				return errors.New("恢复时备份摘要不匹配")
			}
			if err = durableFileWithSync(file.Target, data, os.FileMode(file.Before.Mode), syncDir); err != nil {
				return err
			}
		} else if current.Exists {
			if err = os.Remove(file.Target); err != nil {
				return err
			}
			if err = syncDir(filepath.Dir(file.Target)); err != nil {
				return err
			}
		}
	}
	tx.Phase = "rolled_back"
	if err := tx.saveWithSync(dir, syncDir); err != nil {
		return err
	}
	return clearCommitPending(dir, syncDir)
}
