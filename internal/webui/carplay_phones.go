package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/wen-ryon/teslabox/internal/carplaysettings"
)

type carPlayPhone struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type carPlayPhones struct {
	Phones         []carPlayPhone `json:"phones"`
	Selected       *string        `json:"selected"`
	LastSuccessful *string        `json:"last_successful"`
}

func serveCarPlayPhones(w http.ResponseWriter, path string) {
	fail := func() { http.Error(w, "CarPlay 手机列表暂未就绪", http.StatusServiceUnavailable) }
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 || time.Since(info.ModTime()) > 90*time.Second {
		fail()
		return
	}
	file, err := os.Open(path)
	if err != nil {
		fail()
		return
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		fail()
		return
	}
	var status struct {
		Phones *carPlayPhones `json:"phones"`
	}
	if json.NewDecoder(io.LimitReader(file, 16385)).Decode(&status) != nil || status.Phones == nil || len(status.Phones.Phones) > 8 {
		fail()
		return
	}
	ids := make(map[string]bool)
	for _, phone := range status.Phones.Phones {
		if !carplaysettings.IsPhoneID(phone.ID) || ids[phone.ID] || len(phone.Name) > 128 || strings.IndexFunc(phone.Name, func(value rune) bool { return unicode.IsControl(value) || unicode.Is(unicode.Cf, value) }) >= 0 {
			fail()
			return
		}
		ids[phone.ID] = true
	}
	for _, selected := range []*string{status.Phones.Selected, status.Phones.LastSuccessful} {
		if selected != nil && !ids[*selected] {
			fail()
			return
		}
	}
	if status.Phones.Phones == nil {
		status.Phones.Phones = make([]carPlayPhone, 0)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(status.Phones)
}
