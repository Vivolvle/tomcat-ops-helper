package main

import (
	"path/filepath"
	"testing"

	"tomcat-ops-helper/internal/config"
)

// TestRepoConfig_Loads verifies that the config.json shipped at the repo root
// parses cleanly. Run with: go test ./cmd/helper/... -run TestRepoConfig_Loads.
func TestRepoConfig_Loads(t *testing.T) {
	abs, err := filepath.Abs("../../config.json")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	cfg, err := config.LoadHelper(abs)
	if err != nil {
		t.Fatalf("LoadHelper: %v", err)
	}
	if cfg.ServerURL != "wss://localhost:8443/ws" {
		t.Fatalf("server_url = %s", cfg.ServerURL)
	}
	if !cfg.InsecureSkipVerify {
		t.Fatal("insecure_skip_verify should be true for local testing")
	}
}
