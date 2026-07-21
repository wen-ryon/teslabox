// Package webauth 提供 TeslaBox 本地管理页面的单用户认证和签名会话。
package webauth

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	CookieName        = "teslabox_session"
	DefaultPassword   = "teslabox"
	maxLoginFailures  = 5
	loginWindow       = 5 * time.Minute
	maxAttemptKeys    = 1024
	passwordMinBytes  = 8
	passwordMaxBytes  = 128
	pbkdf2Iterations  = 600_000
	credentialVersion = 1
)

var (
	// ErrInvalidCredentials 避免向客户端区分不存在的用户和错误密码。
	ErrInvalidCredentials = errors.New("登录凭据无效")
	// ErrRateLimited 表示同一来源短时间内失败次数过多。
	ErrRateLimited = errors.New("登录尝试过于频繁")
	// ErrPasswordPolicy 表示新密码没有满足本地管理页面的最低安全边界。
	ErrPasswordPolicy = errors.New("新密码必须为 8 到 128 个字节")
	// ErrPasswordUnchanged 避免用户把默认密码或旧密码再次保存为新密码。
	ErrPasswordUnchanged = errors.New("新密码不能与当前密码相同")
)

// Config 支持从旧环境变量平滑迁移到可原子更新的本地认证状态文件。
type Config struct {
	StatePath      string
	PasswordSHA256 string
	SessionSecret  string
	SessionTTL     time.Duration
}

type attempt struct {
	Failures int
	ResetAt  time.Time
}

type credentialState struct {
	Version       int    `json:"version"`
	Algorithm     string `json:"algorithm"`
	Iterations    int    `json:"iterations"`
	Salt          string `json:"salt"`
	PasswordHash  string `json:"password_hash"`
	SessionSecret string `json:"session_secret"`
	Generation    uint64 `json:"generation"`
	MustChange    bool   `json:"must_change"`
}

type credential struct {
	algorithm  string
	iterations int
	salt       []byte
	hash       []byte
	secret     []byte
	generation uint64
	mustChange bool
}

// Manager 使用带版本的 HMAC Cookie，并限制密码尝试速率和并发改密竞态。
type Manager struct {
	statePath string
	ttl       time.Duration
	now       func() time.Time

	stateMu    sync.RWMutex
	changeMu   sync.Mutex
	credential credential
	attemptMu  sync.Mutex
	attempts   map[string]attempt
}

// New 优先加载持久状态；仅在全新安装且没有旧配置时创建默认密码引导状态。
func New(config Config) (*Manager, error) {
	if config.SessionTTL <= 0 || config.SessionTTL > 365*24*time.Hour {
		return nil, errors.New("Web 会话有效期必须大于零且不超过 365 天")
	}
	manager := &Manager{
		statePath: strings.TrimSpace(config.StatePath), ttl: config.SessionTTL,
		now: time.Now, attempts: make(map[string]attempt),
	}
	if manager.statePath != "" {
		state, err := readCredentialState(manager.statePath)
		if err == nil {
			credential, err := decodeCredential(state)
			if err != nil {
				return nil, fmt.Errorf("读取 Web 认证状态: %w", err)
			}
			manager.credential = credential
			return manager, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("读取 Web 认证状态: %w", err)
		}
	}

	legacyPassword := strings.TrimSpace(config.PasswordSHA256)
	legacySecret := strings.TrimSpace(config.SessionSecret)
	if legacyPassword != "" || legacySecret != "" {
		credential, err := legacyCredential(legacyPassword, legacySecret)
		if err != nil {
			return nil, err
		}
		manager.credential = credential
		return manager, nil
	}
	if manager.statePath == "" {
		return nil, errors.New("全新安装必须配置 Web 认证状态文件路径")
	}
	state, credential, err := newCredentialState(DefaultPassword, 1, true)
	if err != nil {
		return nil, err
	}
	if err := writeCredentialState(manager.statePath, state); err != nil {
		return nil, fmt.Errorf("创建默认 Web 认证状态: %w", err)
	}
	manager.credential = credential
	return manager, nil
}

// Enabled 始终返回 true，避免全新安装以无认证方式暴露设备管理接口。
func (m *Manager) Enabled() bool { return m != nil }

// MustChangePassword 表示当前凭据仍是首次启动默认密码。
func (m *Manager) MustChangePassword() bool {
	if m == nil {
		return false
	}
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.credential.mustChange
}

// Authenticate 使用慢哈希和恒定时间比较，并按来源限制连续失败次数。
func (m *Manager) Authenticate(remoteAddr, password string) error {
	if m == nil || len(password) > passwordMaxBytes {
		return ErrInvalidCredentials
	}
	key, err := m.allowAttempt(remoteIP(remoteAddr))
	if err != nil {
		return err
	}

	m.stateMu.RLock()
	current := cloneCredential(m.credential)
	m.stateMu.RUnlock()
	verified := verifyPassword(current, password)
	m.stateMu.RLock()
	unchanged := current.generation == m.credential.generation && hmac.Equal(current.secret, m.credential.secret)
	m.stateMu.RUnlock()
	if verified && unchanged {
		m.attemptMu.Lock()
		delete(m.attempts, key)
		m.attemptMu.Unlock()
		return nil
	}
	m.recordFailure(key)
	return ErrInvalidCredentials
}

// ChangePassword 验证当前密码后原子保存新哈希，并轮换会话密钥使旧会话立即失效。
func (m *Manager) ChangePassword(remoteAddr, currentPassword, newPassword string) error {
	m.changeMu.Lock()
	defer m.changeMu.Unlock()
	if err := m.Authenticate(remoteAddr, currentPassword); err != nil {
		return err
	}
	if len(newPassword) < passwordMinBytes || len(newPassword) > passwordMaxBytes || strings.TrimSpace(newPassword) == "" {
		return ErrPasswordPolicy
	}
	currentDigest := sha256.Sum256([]byte(currentPassword))
	newDigest := sha256.Sum256([]byte(newPassword))
	if subtle.ConstantTimeCompare(currentDigest[:], newDigest[:]) == 1 {
		return ErrPasswordUnchanged
	}
	if m.statePath == "" {
		return errors.New("Web 认证状态文件路径未配置")
	}

	m.stateMu.RLock()
	nextGeneration := m.credential.generation + 1
	m.stateMu.RUnlock()
	state, next, err := newCredentialState(newPassword, nextGeneration, false)
	if err != nil {
		return err
	}
	if err := writeCredentialState(m.statePath, state); err != nil {
		return fmt.Errorf("保存新 Web 管理密码: %w", err)
	}
	m.stateMu.Lock()
	m.credential = next
	m.stateMu.Unlock()
	m.attemptMu.Lock()
	clear(m.attempts)
	m.attemptMu.Unlock()
	return nil
}

// SetSession 在密码验证成功后签发 HttpOnly 且 SameSite=Strict 的会话 Cookie。
func (m *Manager) SetSession(w http.ResponseWriter, r *http.Request) {
	if m == nil {
		return
	}
	m.stateMu.RLock()
	secret := append([]byte(nil), m.credential.secret...)
	generation := m.credential.generation
	m.stateMu.RUnlock()
	expiresAt := m.now().Add(m.ttl)
	payload := "v2." + strconv.FormatInt(expiresAt.Unix(), 10) + "." + strconv.FormatUint(generation, 10)
	signature := signPayload(secret, payload)
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: payload + "." + signature, Path: "/",
		Expires: expiresAt, MaxAge: int(m.ttl.Seconds()), HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode,
	})
}

// ClearSession 立即让浏览器删除当前会话 Cookie。
func (m *Manager) ClearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		Expires: time.Unix(1, 0), HttpOnly: true, Secure: r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
}

// Authenticated 验证 Cookie 签名、凭据代次和过期时间，不信任客户端身份头。
func (m *Manager) Authenticated(r *http.Request) bool {
	if m == nil {
		return false
	}
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return false
	}
	m.stateMu.RLock()
	current := cloneCredential(m.credential)
	m.stateMu.RUnlock()
	parts := strings.Split(cookie.Value, ".")
	if len(parts) == 3 && parts[0] == "v1" && current.generation == 0 {
		return validSignedSession(parts[0]+"."+parts[1], parts[1], parts[2], current.secret, m.now())
	}
	if len(parts) != 4 || parts[0] != "v2" {
		return false
	}
	generation, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil || generation != current.generation {
		return false
	}
	return validSignedSession(strings.Join(parts[:3], "."), parts[1], parts[3], current.secret, m.now())
}

func (m *Manager) allowAttempt(key string) (string, error) {
	now := m.now()
	m.attemptMu.Lock()
	defer m.attemptMu.Unlock()
	if len(m.attempts) >= maxAttemptKeys {
		for attemptKey, value := range m.attempts {
			if !value.ResetAt.IsZero() && !now.Before(value.ResetAt) {
				delete(m.attempts, attemptKey)
			}
		}
		if len(m.attempts) >= maxAttemptKeys {
			key = "overflow"
		}
	}
	current := m.attempts[key]
	if !current.ResetAt.IsZero() && !now.Before(current.ResetAt) {
		delete(m.attempts, key)
		return key, nil
	}
	if current.Failures >= maxLoginFailures {
		return key, ErrRateLimited
	}
	return key, nil
}

func (m *Manager) recordFailure(key string) {
	now := m.now()
	m.attemptMu.Lock()
	defer m.attemptMu.Unlock()
	current := m.attempts[key]
	if current.ResetAt.IsZero() || !now.Before(current.ResetAt) {
		current = attempt{ResetAt: now.Add(loginWindow)}
	}
	current.Failures++
	m.attempts[key] = current
}

func legacyCredential(passwordValue, secretValue string) (credential, error) {
	passwordHash, err := hex.DecodeString(passwordValue)
	if err != nil || len(passwordHash) != sha256.Size {
		return credential{}, errors.New("Web 密码摘要必须是 SHA-256 十六进制值")
	}
	secret, err := hex.DecodeString(secretValue)
	if err != nil || len(secret) < sha256.Size {
		return credential{}, errors.New("Web 会话密钥必须是至少 32 字节的十六进制值")
	}
	return credential{algorithm: "sha256", hash: passwordHash, secret: secret}, nil
}

func newCredentialState(password string, generation uint64, mustChange bool) (credentialState, credential, error) {
	salt, err := randomBytes(16)
	if err != nil {
		return credentialState{}, credential{}, fmt.Errorf("生成密码盐: %w", err)
	}
	secret, err := randomBytes(32)
	if err != nil {
		return credentialState{}, credential{}, fmt.Errorf("生成会话密钥: %w", err)
	}
	hash, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, sha256.Size)
	if err != nil {
		return credentialState{}, credential{}, fmt.Errorf("计算密码哈希: %w", err)
	}
	state := credentialState{
		Version: credentialVersion, Algorithm: "pbkdf2-sha256", Iterations: pbkdf2Iterations,
		Salt: base64.RawStdEncoding.EncodeToString(salt), PasswordHash: base64.RawStdEncoding.EncodeToString(hash),
		SessionSecret: base64.RawStdEncoding.EncodeToString(secret), Generation: generation, MustChange: mustChange,
	}
	return state, credential{
		algorithm: state.Algorithm, iterations: state.Iterations, salt: salt, hash: hash,
		secret: secret, generation: generation, mustChange: mustChange,
	}, nil
}

func decodeCredential(state credentialState) (credential, error) {
	if state.Version != credentialVersion || state.Algorithm != "pbkdf2-sha256" || state.Iterations < pbkdf2Iterations || state.Iterations > 2_000_000 || state.Generation == 0 {
		return credential{}, errors.New("认证状态版本或算法不受支持")
	}
	salt, err := base64.RawStdEncoding.DecodeString(state.Salt)
	if err != nil || len(salt) < 16 || len(salt) > 64 {
		return credential{}, errors.New("认证状态密码盐无效")
	}
	hash, err := base64.RawStdEncoding.DecodeString(state.PasswordHash)
	if err != nil || len(hash) != sha256.Size {
		return credential{}, errors.New("认证状态密码哈希无效")
	}
	secret, err := base64.RawStdEncoding.DecodeString(state.SessionSecret)
	if err != nil || len(secret) < sha256.Size || len(secret) > 64 {
		return credential{}, errors.New("认证状态会话密钥无效")
	}
	return credential{
		algorithm: state.Algorithm, iterations: state.Iterations, salt: salt, hash: hash,
		secret: secret, generation: state.Generation, mustChange: state.MustChange,
	}, nil
}

func verifyPassword(current credential, password string) bool {
	if current.algorithm == "sha256" {
		digest := sha256.Sum256([]byte(password))
		return subtle.ConstantTimeCompare(digest[:], current.hash) == 1
	}
	derived, err := pbkdf2.Key(sha256.New, password, current.salt, current.iterations, len(current.hash))
	return err == nil && subtle.ConstantTimeCompare(derived, current.hash) == 1
}

func cloneCredential(value credential) credential {
	value.salt = append([]byte(nil), value.salt...)
	value.hash = append([]byte(nil), value.hash...)
	value.secret = append([]byte(nil), value.secret...)
	return value
}

func validSignedSession(payload, expiry, encodedSignature string, secret []byte, now time.Time) bool {
	expected, err := base64.RawURLEncoding.DecodeString(encodedSignature)
	if err != nil || !hmac.Equal(expected, signature(secret, payload)) {
		return false
	}
	expiresAt, err := strconv.ParseInt(expiry, 10, 64)
	return err == nil && now.Unix() < expiresAt
}

func signPayload(secret []byte, payload string) string {
	return base64.RawURLEncoding.EncodeToString(signature(secret, payload))
}

func signature(secret []byte, payload string) []byte {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(payload))
	return mac.Sum(nil)
}

func randomBytes(size int) ([]byte, error) {
	value := make([]byte, size)
	_, err := rand.Read(value)
	return value, err
}

func readCredentialState(path string) (credentialState, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return credentialState{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return credentialState{}, errors.New("认证状态必须是普通文件且不能是符号链接")
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return credentialState{}, fmt.Errorf("收紧认证状态权限: %w", err)
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return credentialState{}, err
	}
	defer file.Close()
	var state credentialState
	decoder := json.NewDecoder(io.LimitReader(file, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return credentialState{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return credentialState{}, errors.New("认证状态包含多余内容")
	}
	return state, nil
}

func writeCredentialState(path string, state credentialState) (resultErr error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("创建认证状态目录: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".web-auth-*")
	if err != nil {
		return fmt.Errorf("创建认证临时文件: %w", err)
	}
	name := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			resultErr = errors.Join(resultErr, temporary.Close())
		}
		if resultErr != nil {
			_ = os.Remove(name)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("限制认证状态权限: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(state); err != nil {
		return fmt.Errorf("编码认证状态: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("同步认证状态: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭认证状态: %w", err)
	}
	closed = true
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("替换认证状态: %w", err)
	}
	return nil
}

func remoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err == nil {
		return host
	}
	if remoteAddr == "" {
		return "unknown"
	}
	return fmt.Sprintf("raw:%s", remoteAddr)
}
