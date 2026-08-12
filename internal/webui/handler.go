// Package webui 提供编译进 TeslaBox 二进制的本地管理页面和认证边界。
package webui

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/wen-ryon/teslabox/internal/systeminfo"
	"github.com/wen-ryon/teslabox/internal/webauth"
)

//go:embed assets/*
var assets embed.FS

// Authenticator 是页面路由使用的最小会话认证能力。
type Authenticator interface {
	Enabled() bool
	MustChangePassword() bool
	Authenticate(remoteAddr, password string) error
	ChangePassword(remoteAddr, currentPassword, newPassword string) error
	SetSession(w http.ResponseWriter, r *http.Request)
	ClearSession(w http.ResponseWriter, r *http.Request)
	Authenticated(r *http.Request) bool
}

// NewHandler 把静态页面、登录会话和现有 API 组合到同一来源下。
func NewHandler(apiHandler http.Handler, authenticator Authenticator, logger *slog.Logger) http.Handler {
	assetFS, err := fs.Sub(assets, "assets")
	if err != nil {
		panic("嵌入 Web 资源目录无效")
	}
	systemCollector := systeminfo.NewCollector()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", serveEmbedded(assetFS, "index.html", "text/html; charset=utf-8", "no-store"))
	mux.HandleFunc("GET /app.css", serveEmbedded(assetFS, "app.css", "text/css; charset=utf-8", "no-cache"))
	mux.HandleFunc("GET /app.js", serveEmbedded(assetFS, "app.js", "text/javascript; charset=utf-8", "no-cache"))
	mux.HandleFunc("GET /teslabox-logo.svg", serveEmbedded(assetFS, "teslabox-logo.svg", "image/svg+xml", "public, max-age=86400"))
	mux.HandleFunc("GET /third-party-notices.txt", serveEmbedded(assetFS, "THIRD_PARTY_NOTICES.txt", "text/plain; charset=utf-8", "public, max-age=86400"))
	mux.HandleFunc("GET /api/v1/auth/session", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{
			"enabled": authenticator.Enabled(), "authenticated": authenticator.Authenticated(r),
			"must_change_password": authenticator.MustChangePassword(),
		})
	})
	mux.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "请求来源无效"})
			return
		}
		var input struct {
			Password string `json:"password"`
		}
		decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil || strings.TrimSpace(input.Password) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请输入管理密码"})
			return
		}
		if err := authenticator.Authenticate(r.RemoteAddr, input.Password); err != nil {
			if errors.Is(err, webauth.ErrRateLimited) {
				w.Header().Set("Retry-After", "300")
				writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "尝试次数过多，请稍后再试"})
				return
			}
			logger.Warn("Web 管理页面登录失败", "remote_addr", remoteHost(r.RemoteAddr))
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "密码错误"})
			return
		}
		authenticator.SetSession(w, r)
		writeJSON(w, http.StatusOK, map[string]bool{
			"authenticated": true, "must_change_password": authenticator.MustChangePassword(),
		})
	})
	mux.HandleFunc("POST /api/v1/auth/password", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "请求来源无效"})
			return
		}
		if !authenticator.Authenticated(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "请先登录"})
			return
		}
		var input struct {
			CurrentPassword string `json:"current_password"`
			NewPassword     string `json:"new_password"`
		}
		decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求参数无效"})
			return
		}
		if err := authenticator.ChangePassword(r.RemoteAddr, input.CurrentPassword, input.NewPassword); err != nil {
			switch {
			case errors.Is(err, webauth.ErrRateLimited):
				w.Header().Set("Retry-After", "300")
				writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "尝试次数过多，请稍后再试"})
			case errors.Is(err, webauth.ErrInvalidCredentials):
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "当前密码错误"})
			case errors.Is(err, webauth.ErrPasswordPolicy), errors.Is(err, webauth.ErrPasswordUnchanged):
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			default:
				logger.Error("修改 Web 管理密码失败", "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "暂时无法保存新密码"})
			}
			return
		}
		authenticator.SetSession(w, r)
		writeJSON(w, http.StatusOK, map[string]bool{"changed": true, "must_change_password": false})
	})
	mux.HandleFunc("POST /api/v1/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "请求来源无效"})
			return
		}
		authenticator.ClearSession(w, r)
		writeJSON(w, http.StatusOK, map[string]bool{"authenticated": false})
	})
	protectedAPI := requireAuth(apiHandler, authenticator)
	mux.Handle("GET /api/v1/system", requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, systemCollector.Collect(r.Context()))
	}), authenticator))
	mux.Handle("GET /api/v1/", protectedAPI)
	mux.Handle("POST /api/v1/", protectedAPI)
	mux.Handle("DELETE /api/v1/", protectedAPI)
	mux.Handle("GET /healthz", apiHandler)
	return securityHeaders(mux)
}

func requireAuth(next http.Handler, authenticator Authenticator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authenticator.Authenticated(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "请先登录"})
			return
		}
		if authenticator.MustChangePassword() {
			writeJSON(w, http.StatusPreconditionRequired, map[string]any{
				"error": "请先修改默认管理密码", "password_change_required": true,
			})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "请求来源无效"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func serveEmbedded(files fs.FS, name, contentType, cacheControl string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		content, err := fs.ReadFile(files, name)
		if err != nil {
			http.Error(w, "页面资源不可用", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", cacheControl)
		if _, err := w.Write(content); err != nil {
			return
		}
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; media-src 'self' blob:; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && strings.EqualFold(parsed.Host, r.Host) && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		return
	}
}
