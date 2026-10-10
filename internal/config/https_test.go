package config

import (
	"encoding/base64"
	"testing"

	"github.com/wen-ryon/teslabox/internal/certdelivery"
)

func TestLoadHTTPS(t *testing.T) {
	t.Setenv("TESLABOX_HTTPS_DOMAIN", "carplay.teslabox.dev")
	t.Setenv("TESLABOX_HTTPS_CERTIFICATE_BUNDLE", "/var/lib/teslabox-system/tls/certificate.pem")
	t.Setenv("TESLABOX_HTTPS_ADDR", "127.0.0.1:9443")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPSAddr != "127.0.0.1:9443" || cfg.HTTPSDomain != "carplay.teslabox.dev" {
		t.Fatalf("HTTPS settings not loaded: %q %q", cfg.HTTPSAddr, cfg.HTTPSDomain)
	}
	t.Setenv("TESLABOX_HTTPS_DOMAIN", "teslabox.local")
	if _, err := Load(); err == nil {
		t.Fatal("accepted mDNS certificate name")
	}
	t.Setenv("TESLABOX_HTTPS_DOMAIN", "carplay.teslabox.dev")
	t.Setenv("TESLABOX_HTTPS_CERTIFICATE_BUNDLE", "")
	if _, err := Load(); err == nil {
		t.Fatal("accepted partial configuration")
	}
}

func TestProductCertificateDefaultsRequireNoUserDNSConfiguration(t *testing.T) {
	oldEndpoint, oldKey, oldAddress := certdelivery.DefaultEndpoint, certdelivery.DefaultPublicKey, certdelivery.DefaultPublicAddress
	t.Cleanup(func() {
		certdelivery.DefaultEndpoint, certdelivery.DefaultPublicKey, certdelivery.DefaultPublicAddress = oldEndpoint, oldKey, oldAddress
	})
	certdelivery.DefaultEndpoint = "https://certs.teslabox.online"
	certdelivery.DefaultPublicKey = base64.StdEncoding.EncodeToString(make([]byte, 32))
	certdelivery.DefaultPublicAddress = "203.0.113.8"
	for _, name := range []string{"TESLABOX_HTTPS_DOMAIN", "TESLABOX_HTTPS_CERTIFICATE_BUNDLE", "TESLABOX_HTTPS_CERTIFICATE_ENDPOINT", "TESLABOX_HTTPS_CERTIFICATE_PUBLIC_KEY", "TESLABOX_CARPLAY_PUBLIC_ADDRESS", "TESLABOX_CARPLAY_BROWSER_TOKEN", "TESLABOX_CARPLAY_SERVICE_SETTINGS", "TESLABOX_CARPLAY_MFI_DIRECTORY"} {
		t.Setenv(name, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPSDomain != certdelivery.ProductDomain || cfg.CarPlayPublicAddress != "203.0.113.8" || cfg.HTTPSCertificateBundle == "" || cfg.HTTPSCertificateIdentity == "" || cfg.CarPlayBrowserToken != "/run/teslabox-carplay/browser-token" {
		t.Fatalf("product defaults missing: %#v", cfg)
	}
	if cfg.CarPlayServiceSettings != "/var/lib/teslabox-system/carplay/service.json" || cfg.CarPlayMFIDirectory != "/var/lib/teslabox-system/carplay/mfi" {
		t.Fatalf("product phone registration settings = %q", cfg.CarPlayServiceSettings)
	}
	t.Setenv("TESLABOX_CARPLAY_SERVICE_SETTINGS", "/var/lib/teslabox-carplay-experiment/carplay-service.json")
	cfg, err = Load()
	if err != nil || cfg.CarPlayServiceSettings != "/var/lib/teslabox-carplay-experiment/carplay-service.json" {
		t.Fatalf("legacy override failed: %q %v", cfg.CarPlayServiceSettings, err)
	}
	t.Setenv("TESLABOX_HTTPS_DOMAIN", "different.teslabox.online")
	if _, err := Load(); err == nil {
		t.Fatal("accepted conflicting product domain")
	}
}

func TestCarPlayServiceSettingsRejectUnsafePath(t *testing.T) {
	for _, path := range []string{"/", "relative/service.json", "/var/lib/../service.json"} {
		t.Run(path, func(t *testing.T) {
			t.Setenv("TESLABOX_CARPLAY_SERVICE_SETTINGS", path)
			if _, err := Load(); err == nil {
				t.Fatal("accepted unsafe service settings path")
			}
		})
	}
}
