package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// UpdateProxies must touch only the "proxies" key: other stored keys (including ones this version
// does not know) survive byte-for-byte in meaning, and environment overrides applied by Load are
// never written back.
func TestUpdateProxies_RewritesOnlyTheProxiesKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", dir)
	t.Setenv("ANTIGRAVITY_PORT", "9999") // an override Load would apply

	stored := `{"port": 8080, "db_path": "/data/accounts.db", "future_key": {"nested": true}}`
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(stored), 0o600); err != nil {
		t.Fatal(err)
	}

	next, err := UpdateProxies(func(cur []string) ([]string, error) {
		if len(cur) != 0 {
			t.Errorf("expected an empty stored pool, got %v", cur)
		}
		return []string{"http://proxy.example.com:3128"}, nil
	})
	if err != nil {
		t.Fatalf("UpdateProxies: %v", err)
	}
	if len(next) != 1 {
		t.Fatalf("returned pool: %v", next)
	}

	data, err := os.ReadFile(filepath.Join(dir, ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["port"] != float64(8080) {
		t.Errorf("port = %v; the ANTIGRAVITY_PORT override must not be persisted", got["port"])
	}
	if got["db_path"] != "/data/accounts.db" {
		t.Errorf("db_path changed: %v", got["db_path"])
	}
	if _, ok := got["future_key"]; !ok {
		t.Error("unknown key was dropped")
	}
	if _, ok := got["quota_interval"]; ok {
		t.Error("defaults were written into the file; only proxies may change")
	}

	pool, err := StoredProxies()
	if err != nil || len(pool) != 1 || pool[0] != "http://proxy.example.com:3128" {
		t.Errorf("StoredProxies = %v (err %v)", pool, err)
	}

	// Emptying the pool removes the key instead of leaving "proxies": null behind.
	if _, err := UpdateProxies(func([]string) ([]string, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, ConfigFileName))
	got = map[string]any{}
	_ = json.Unmarshal(data, &got)
	if _, ok := got["proxies"]; ok {
		t.Error("empty pool should remove the proxies key")
	}
}

func TestUpdateProxies_CreatesMissingFile(t *testing.T) {
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", filepath.Join(t.TempDir(), "new"))
	if _, err := UpdateProxies(func([]string) ([]string, error) {
		return []string{"socks5://10.0.0.1:1080"}, nil
	}); err != nil {
		t.Fatalf("UpdateProxies on a missing file: %v", err)
	}
	cfg, err := Load()
	if err != nil || len(cfg.Proxies) != 1 {
		t.Fatalf("Load after create: %+v (err %v)", cfg, err)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("defaults must still apply on load, port = %d", cfg.Port)
	}
}
