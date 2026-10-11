// Package nasupload 保存手动上传任务，并将网络传输与车端只读快照隔离。
package nasupload

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"path"
	"strings"
	"time"
)

// Target 使用结构化字段，禁止用户配置任意命令或挂载系统目录。
type Target struct {
	ID          string  `json:"id"`
	IsDefault   bool    `json:"is_default"`
	Name        string  `json:"name"`
	Protocol    string  `json:"protocol"`
	Host        string  `json:"host"`
	Port        int     `json:"port"`
	URL         string  `json:"url"`
	User        string  `json:"user"`
	Domain      string  `json:"domain"`
	Path        string  `json:"path"`
	HostKey     string  `json:"host_key"`
	AllowHTTP   bool    `json:"allow_http"`
	Password    string  `json:"password,omitempty"`
	Credential  string  `json:"credential,omitempty"`
	HasPassword bool    `json:"has_password"`
	Revision    string  `json:"revision"`
	Quality     Quality `json:"quality"`
}

// Quality 将源文件与转码区分；高度指单路视频或合并画布的总高度。
type Quality struct {
	Mode        string `json:"mode"`
	Height      int    `json:"height"`
	FPS         int    `json:"fps"`
	BitrateKbps int    `json:"bitrate_kbps"`
	Layout      string `json:"layout"`
}

// Request 固定一次手动选择，画质可覆盖目标的默认设置。
type Request struct {
	TargetID    string   `json:"target_id"`
	SessionID   string   `json:"session_id"`
	SnapshotID  string   `json:"snapshot_id"`
	MomentIDs   []string `json:"moment_ids"`
	Cameras     []string `json:"cameras"`
	Quality     *Quality `json:"quality,omitempty"`
	AutoEventID string   `json:"auto_event_id,omitempty"`
	AutoKey     string   `json:"auto_key,omitempty"`
	Role        string   `json:"role,omitempty"`
}

// Input 只允许 API 在受保护快照中解析文件路径。
type Input struct {
	MomentID   string    `json:"moment_id"`
	Camera     string    `json:"camera"`
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	RecordedAt time.Time `json:"recorded_at"`
	Size       uint64    `json:"size"`
}

// FileResult 保留可核验的远端落点和内容摘要，不返回板端路径。
type FileResult struct {
	MomentID   string    `json:"moment_id"`
	Cameras    []string  `json:"cameras"`
	Name       string    `json:"name"`
	RemotePath string    `json:"remote_path"`
	SHA256     string    `json:"sha256"`
	Bytes      uint64    `json:"bytes"`
	VerifiedAt time.Time `json:"verified_at"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Task 的成功计数不依赖浏览器保持打开。
type Task struct {
	ID             string       `json:"id"`
	Request        Request      `json:"request"`
	TargetName     string       `json:"target_name"`
	TargetRevision string       `json:"target_revision"`
	State          string       `json:"state"`
	CreatedAt      time.Time    `json:"created_at"`
	Error          string       `json:"error,omitempty"`
	Attempts       int          `json:"attempts"`
	NextRetryAt    time.Time    `json:"next_retry_at,omitempty"`
	Files          []FileResult `json:"files"`
	TotalFiles     int          `json:"total_files"`
}

// Transport 的校验要求适用于没有原生散列接口的 WebDAV 服务。
type Transport interface {
	Obscure(context.Context, string) (string, error)
	Test(context.Context, Target) error
	Upload(context.Context, Target, string, string, string, string) error
}

// Processor 与上传分离，编码必须复用现有硬件调度器。
type Processor interface {
	Process(context.Context, Quality, []Input, string) error
}

func validateQuality(q Quality) error {
	if q.Mode == "original" {
		return nil
	}
	if q.Mode != "compressed" || q.Height < 240 || q.Height > 1080 || q.Height%2 != 0 || q.FPS < 1 || q.FPS > 36 || q.BitrateKbps < 256 || q.BitrateKbps > 20000 {
		return errors.New("压缩画质需设置偶数高度 240–1080、帧率 1–36、码率 256–20000 kbps")
	}
	switch q.Layout {
	case "separate", "composite6", "composite4a", "composite4b", "composite3":
		if q.Layout != "separate" && q.Height%12 != 0 {
			return errors.New("合并画面的高度需为 12 的倍数，例如 480、720、1080")
		}
		return nil
	}
	return errors.New("请选择分视角文件或支持的合并布局")
}

func validateTarget(t Target) error {
	if strings.TrimSpace(t.Name) == "" || len(t.Name) > 100 || len(t.User) > 256 || t.User == "" || t.Port < 0 || t.Port > 65535 {
		return errors.New("上传目标名称、用户名或端口无效")
	}
	for _, s := range []string{t.Host, t.User, t.Domain, t.Path, t.URL, t.HostKey, t.Password} {
		if strings.ContainsAny(s, "\r\n\x00") {
			return errors.New("上传配置不能包含换行或空字符")
		}
	}
	if len(t.Path) > 1024 || strings.Contains(t.Path, "\\") || strings.Contains(t.Path, ":") {
		return errors.New("目标目录无效")
	}
	for _, s := range strings.Split(t.Path, "/") {
		if s == ".." {
			return errors.New("目标目录不能包含上级目录")
		}
	}
	if t.Protocol == "webdav" {
		u, err := url.Parse(t.URL)
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && t.AllowHTTP)) {
			return errors.New("WebDAV 需填写 HTTPS 地址；HTTP 需显式允许")
		}
	} else if t.Protocol == "smb" || t.Protocol == "sftp" {
		if t.Host == "" || strings.ContainsAny(t.Host, "/\\ @[]:") && net.ParseIP(t.Host) == nil {
			return errors.New("请填写主机名或 IP，端口单独配置")
		}
		if t.Protocol == "smb" && strings.Trim(t.Path, "/") == "" {
			return errors.New("SMB 目录必须以共享名称开头")
		}
		if t.Protocol == "sftp" {
			fields := strings.Fields(t.HostKey)
			if len(fields) != 2 {
				return errors.New("SFTP 需填写服务器公钥，例如 ssh-ed25519 AAAA…，不能只填指纹")
			}
			if !strings.HasPrefix(fields[0], "ssh-") && !strings.HasPrefix(fields[0], "ecdsa-") {
				return errors.New("SFTP 公钥算法无效")
			}
			if data, err := base64.StdEncoding.DecodeString(fields[1]); err != nil || len(data) < 24 {
				return errors.New("SFTP 公钥内容无效")
			}
		}
	} else {
		return errors.New("仅支持 SMB、WebDAV、SFTP")
	}
	return validateQuality(t.Quality)
}

func publicTarget(t Target) Target {
	t.Password = ""
	t.HasPassword = t.Credential != ""
	t.Credential = ""
	return t
}
func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func remoteFile(t Target, identity string, inputs []Input, q Quality, hash string, name string) string {
	return path.Join(t.Path, "teslabox", digest(identity)[:16], inputs[0].RecordedAt.Format("2006-01-02"), hash[:24]+"-"+digest(q)[:12], path.Base(name))
}
