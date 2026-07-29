package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigDataDoesNotRewriteNewerLiveFile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	liveData := []byte(`
port: 9090
remote-management:
  secret-key: newer-live-secret
`)
	if errWrite := os.WriteFile(configPath, liveData, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	capturedData := []byte(`
port: 8080
remote-management:
  secret-key: older-captured-secret
`)

	cfg, errLoad := LoadConfigData(configPath, capturedData)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if cfg.Port != 8080 {
		t.Fatalf("captured config port = %d, want 8080", cfg.Port)
	}
	if !looksLikeBcrypt(cfg.RemoteManagement.SecretKey) {
		t.Fatal("captured plaintext management secret was not normalized in memory")
	}

	gotLiveData, errRead := os.ReadFile(configPath)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if !bytes.Equal(gotLiveData, liveData) {
		t.Fatalf("captured snapshot rewrote the newer live config:\n%s", gotLiveData)
	}
}
