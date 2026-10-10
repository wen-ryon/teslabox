package certdelivery

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

var validDeviceID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// DeviceRecord 只保存下载鉴权所需身份，设备私钥不上传到服务端。
type DeviceRecord struct {
	ID            string    `json:"id"`
	PublicKey     []byte    `json:"public_key"`
	RegisteredAt  time.Time `json:"registered_at"`
	LastRequestAt time.Time `json:"last_request_at"`
	Revoked       bool      `json:"revoked"`
}

type pendingChallenge struct {
	device   string
	expires  time.Time
	resource string
}
type requestQuota struct {
	started time.Time
	count   int
}

// DistributionServer 将登记、停用与一次性下载挑战放在独立服务内。
type DistributionServer struct {
	stateDir      string
	bundlePath    string
	mfiBundlePath string
	publicKey     ed25519.PublicKey
	mu            sync.Mutex
	pending       map[string]pendingChallenge
	quota         map[string]requestQuota
}

// NewDistributionServer 只接受本地私有状态路径和签名公钥，不需要 DNS API 权限。
func NewDistributionServer(stateDir, bundlePath string, publicKey ed25519.PublicKey) (*DistributionServer, error) {
	if len(publicKey) != ed25519.PublicKeySize || !filepath.IsAbs(bundlePath) {
		return nil, errors.New("证书分发服务配置无效")
	}
	if err := privateDirectory(stateDir); err != nil {
		return nil, err
	}
	return &DistributionServer{stateDir: stateDir, bundlePath: bundlePath, publicKey: append(ed25519.PublicKey(nil), publicKey...), pending: make(map[string]pendingChallenge), quota: make(map[string]requestQuota)}, nil
}

// Handler 仅提供登记和证书下载，不暴露设备清单与管理接口。
func (s *DistributionServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/register", s.register)
	mux.HandleFunc("POST /v1/challenge", s.newChallenge)
	mux.HandleFunc("POST /v1/bundle", s.download)
	mux.HandleFunc("POST /v1/mfi-bundle", func(w http.ResponseWriter, r *http.Request) { s.downloadResource(w, r, "mfi") })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !s.allowRequest(r.RemoteAddr, r.Header.Get("X-Real-IP")) {
			http.Error(w, "请求过于频繁", http.StatusTooManyRequests)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *DistributionServer) allowRequest(remote, remoteIPHeader string) bool {
	// 仅按直接连接地址限流，不信任可伪造的 X-Forwarded-For；公网代理另设限流。
	address, _, err := net.SplitHostPort(remote)
	if err != nil {
		address = remote
	}
	// 服务仅监听回环地址；仅信任本机代理覆盖后的真实地址，公网头不可直接穿透。
	if peer := net.ParseIP(address); peer != nil && peer.IsLoopback() {
		if real := net.ParseIP(remoteIPHeader); real != nil {
			address = real.String()
		}
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, value := range s.quota {
		if now.Sub(value.started) >= time.Minute {
			delete(s.quota, key)
		}
	}
	value := s.quota[address]
	if value.started.IsZero() {
		if len(s.quota) >= 4096 {
			return false
		}
		value.started = now
	}
	value.count++
	s.quota[address] = value
	return value.count <= 120
}

func decodeRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		http.Error(w, "请求无效", 400)
		return false
	}
	if decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "请求无效", 400)
		return false
	}
	return true
}

func (s *DistributionServer) record(id string) (DeviceRecord, error) {
	var record DeviceRecord
	if !validDeviceID.MatchString(id) {
		return record, errors.New("设备标识无效")
	}
	data, err := readPrivate(filepath.Join(s.stateDir, id+".json"), 4096)
	if err != nil {
		return record, err
	}
	if json.Unmarshal(data, &record) != nil || record.ID != id || len(record.PublicKey) != ed25519.PublicKeySize || deviceID(record.PublicKey) != id {
		return DeviceRecord{}, errors.New("设备登记文件无效")
	}
	_, revokedErr := readPrivate(filepath.Join(s.stateDir, id+".revoked"), 64)
	if revokedErr != nil && !errors.Is(revokedErr, os.ErrNotExist) {
		return DeviceRecord{}, revokedErr
	}
	record.Revoked = revokedErr == nil
	return record, nil
}

func (s *DistributionServer) register(w http.ResponseWriter, r *http.Request) {
	var request registration
	if !decodeRequest(w, r, &request) {
		return
	}
	if len(request.PublicKey) != ed25519.PublicKeySize || !ed25519.Verify(request.PublicKey, registrationPayload(request.PublicKey), request.Proof) {
		http.Error(w, "登记证明无效", 403)
		return
	}
	id := deviceID(request.PublicKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.record(id)
	if errors.Is(err, os.ErrNotExist) {
		record = DeviceRecord{ID: id, PublicKey: request.PublicKey, RegisteredAt: time.Now().UTC(), LastRequestAt: time.Now().UTC()}
		data, _ := json.Marshal(record)
		err = writePrivate(filepath.Join(s.stateDir, id+".json"), data)
	}
	if err != nil {
		http.Error(w, "登记服务暂不可用", 503)
		return
	}
	if record.Revoked {
		http.Error(w, "设备下载权限已停用", 403)
		return
	}
	writeResponse(w, map[string]string{"device_id": id})
}

func (s *DistributionServer) newChallenge(w http.ResponseWriter, r *http.Request) {
	var request struct {
		DeviceID string `json:"device_id"`
		Resource string `json:"resource,omitempty"`
	}
	if !decodeRequest(w, r, &request) {
		return
	}
	if request.Resource != "" && request.Resource != "mfi" {
		http.Error(w, "下载资源无效", 400)
		return
	}
	if request.Resource == "mfi" && s.mfiBundlePath == "" {
		http.Error(w, "软件身份未启用", 404)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.record(request.DeviceID)
	if err != nil || record.Revoked {
		http.Error(w, "设备未登记或已停用", 403)
		return
	}
	now := time.Now()
	for nonce, value := range s.pending {
		if !now.Before(value.expires) {
			delete(s.pending, nonce)
		}
	}
	if len(s.pending) >= 4096 {
		http.Error(w, "挑战服务繁忙", 503)
		return
	}
	for nonce, value := range s.pending {
		if value.device == request.DeviceID {
			delete(s.pending, nonce)
		}
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		http.Error(w, "挑战服务暂不可用", 503)
		return
	}
	nonce := hex.EncodeToString(random)
	expires := now.Add(time.Minute)
	s.pending[nonce] = pendingChallenge{device: request.DeviceID, expires: expires, resource: request.Resource}
	writeResponse(w, challenge{Nonce: nonce, Expires: expires.UTC()})
}

func (s *DistributionServer) download(w http.ResponseWriter, r *http.Request) {
	s.downloadResource(w, r, "")
}

func (s *DistributionServer) downloadResource(w http.ResponseWriter, r *http.Request, resource string) {
	if resource == "mfi" && s.mfiBundlePath == "" {
		http.Error(w, "软件身份未启用", 404)
		return
	}
	var request download
	if !decodeRequest(w, r, &request) {
		return
	}
	s.mu.Lock()
	record, err := s.record(request.DeviceID)
	pending, exists := s.pending[request.Nonce]
	valid := err == nil && !record.Revoked && exists && pending.device == request.DeviceID && pending.resource == resource && time.Now().Before(pending.expires) && ed25519.Verify(record.PublicKey, scopedDownloadPayload(resource, request.DeviceID, request.Nonce), request.Proof)
	if valid {
		delete(s.pending, request.Nonce)
		record.LastRequestAt = time.Now().UTC()
		data, _ := json.Marshal(record)
		if writePrivate(filepath.Join(s.stateDir, record.ID+".json"), data) != nil {
			s.mu.Unlock()
			http.Error(w, "设备记录暂不可用", 503)
			return
		}
	}
	s.mu.Unlock()
	if !valid {
		http.Error(w, "设备下载证明无效", 403)
		return
	}
	bundlePath := s.bundlePath
	if resource == "mfi" {
		bundlePath = s.mfiBundlePath
	}
	data, err := readPrivate(bundlePath, maxEnvelope)
	if err != nil {
		http.Error(w, "统一证书暂不可用", 503)
		return
	}
	defer clear(data)
	if resource == "mfi" {
		bundle, err := verifyMFIEnvelope(data, s.publicKey)
		clear(bundle.PrivateKey)
		if err != nil {
			http.Error(w, "软件身份暂不可用", 503)
			return
		}
	} else if bundle, err := verifyEnvelope(data, s.publicKey); err != nil {
		http.Error(w, "统一证书暂不可用", 503)
		return
	} else {
		clear(bundle.PEM)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func writeResponse(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}

// Revoke 停用登记身份后不能通过重复登记恢复权限，管理员需显式重新批准。
func (s *DistributionServer) Revoke(id string, revoked bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.record(id)
	if err != nil {
		return err
	}
	marker := filepath.Join(s.stateDir, id+".revoked")
	if revoked {
		return writePrivate(marker, []byte("revoked\n"))
	}
	if _, err := readPrivate(marker, 64); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return os.Remove(marker)
}

// Devices 在本机管理命令中读取登记清单，公网接口不返回设备公钥和状态。
func (s *DistributionServer) Devices() ([]DeviceRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.stateDir)
	if err != nil {
		return nil, err
	}
	result := make([]DeviceRecord, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != 69 || filepath.Ext(name) != ".json" {
			continue
		}
		record, err := s.record(name[:64])
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, nil
}
