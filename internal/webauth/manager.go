// Package webauth 提供 TeslaBox 本地管理页面的单用户认证和签名会话。
package webauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	CookieName       = "teslabox_session"
	maxLoginFailures = 5
	loginWindow      = 5 * time.Minute
	maxAttemptKeys   = 1024
)

var (
	// ErrInvalidCredentials 避免向客户端区分不存在的用户和错误密码。
	ErrInvalidCredentials = errors.New("登录凭据无效")
	// ErrRateLimited 表示同一来源短时间内失败次数过多。
	ErrRateLimited = errors.New("登录尝试过于频繁")
)

// Config 保存已经摘要的密码、会话签名密钥和有效期。
type Config struct {
	PasswordSHA256 string
	SessionSecret  string
	SessionTTL     time.Duration
}

type attempt struct {
	Failures int
	ResetAt  time.Time
}

// Manager 使用无服务端持久状态的 HMAC Cookie，并限制密码尝试速率。
type Manager struct {
	enabled      bool
	passwordHash []byte
	secret       []byte
	ttl          time.Duration
	now          func() time.Time
	mu           sync.Mutex
	attempts     map[string]attempt
}

// New 在配置完整时启用认证；两个密钥都为空时保持兼容的无认证模式。
func New(config Config) (*Manager, error) {
	passwordValue := strings.TrimSpace(config.PasswordSHA256)
	secretValue := strings.TrimSpace(config.SessionSecret)
	if passwordValue == "" && secretValue == "" {
		return &Manager{ttl: config.SessionTTL, now: time.Now, attempts: make(map[string]attempt)}, nil
	}
	passwordHash, err := hex.DecodeString(passwordValue)
	if err != nil || len(passwordHash) != sha256.Size {
		return nil, errors.New("Web 密码摘要必须是 SHA-256 十六进制值")
	}
	secret, err := hex.DecodeString(secretValue)
	if err != nil || len(secret) < sha256.Size {
		return nil, errors.New("Web 会话密钥必须是至少 32 字节的十六进制值")
	}
	if config.SessionTTL <= 0 || config.SessionTTL > 365*24*time.Hour {
		return nil, errors.New("Web 会话有效期必须大于零")
	}
	return &Manager{
		enabled: true, passwordHash: passwordHash, secret: secret, ttl: config.SessionTTL,
		now: time.Now, attempts: make(map[string]attempt),
	}, nil
}

// Enabled 表示当前部署是否要求登录后访问管理页面和 API。
func (m *Manager) Enabled() bool {
	return m != nil && m.enabled
}

// Authenticate 使用恒定时间比较密码摘要，并按来源限制连续失败次数。
func (m *Manager) Authenticate(remoteAddr, password string) error {
	if !m.Enabled() {
		return nil
	}
	key := remoteIP(remoteAddr)
	now := m.now()
	m.mu.Lock()
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
		current = attempt{}
	}
	if current.Failures >= maxLoginFailures {
		m.mu.Unlock()
		return ErrRateLimited
	}
	m.mu.Unlock()

	digest := sha256.Sum256([]byte(password))
	if subtle.ConstantTimeCompare(digest[:], m.passwordHash) == 1 {
		m.mu.Lock()
		delete(m.attempts, key)
		m.mu.Unlock()
		return nil
	}

	m.mu.Lock()
	current = m.attempts[key]
	if current.ResetAt.IsZero() || !now.Before(current.ResetAt) {
		current = attempt{ResetAt: now.Add(loginWindow)}
	}
	current.Failures++
	m.attempts[key] = current
	m.mu.Unlock()
	return ErrInvalidCredentials
}

// SetSession 在密码验证成功后签发 HttpOnly 且 SameSite=Strict 的会话 Cookie。
func (m *Manager) SetSession(w http.ResponseWriter, r *http.Request) {
	if !m.Enabled() {
		return
	}
	expiresAt := m.now().Add(m.ttl)
	payload := "v1." + strconv.FormatInt(expiresAt.Unix(), 10)
	signature := m.sign(payload)
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

// Authenticated 验证 Cookie 签名和过期时间，不信任任何客户端身份头。
func (m *Manager) Authenticated(r *http.Request) bool {
	if !m.Enabled() {
		return true
	}
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return false
	}
	payload := parts[0] + "." + parts[1]
	expected, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(expected, m.signature(payload)) {
		return false
	}
	expiresAt, err := strconv.ParseInt(parts[1], 10, 64)
	return err == nil && m.now().Unix() < expiresAt
}

func (m *Manager) sign(payload string) string {
	return base64.RawURLEncoding.EncodeToString(m.signature(payload))
}

func (m *Manager) signature(payload string) []byte {
	mac := hmac.New(sha256.New, m.secret)
	if _, err := mac.Write([]byte(payload)); err != nil {
		return nil
	}
	return mac.Sum(nil)
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
