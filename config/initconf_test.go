package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingConfig(t *testing.T) {
	t.Setenv("ICHAT_CONFIG_FILE", filepath.Join(t.TempDir(), "missing.yml"))
	if _, err := Load(); err == nil {
		t.Fatal("missing explicit config must return an error")
	}
}

func TestLoadConfigIsIndependent(t *testing.T) {
	t.Setenv("ICHAT_LOG_LEVEL", "warn")
	t.Setenv("ICHAT_LOG_ACCESSENABLED", "false")
	configFile := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv("ICHAT_CONFIG_FILE", configFile)
	if err := os.WriteFile(configFile, []byte("APP:\n  name: first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if first.App.Name != "first" || first.LOG.Level != "warn" || first.LOG.AccessEnabled {
		t.Fatalf("unexpected config: app=%q level=%q access=%v", first.App.Name, first.LOG.Level, first.LOG.AccessEnabled)
	}
	if err := os.WriteFile(configFile, []byte("APP:\n  version: second\n"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if second.App.Name != "" || second.App.Version != "second" {
		t.Fatal("Load retained state from an earlier config")
	}
	if Conf != (Config{}) {
		t.Fatal("Load must not assign the legacy global configuration")
	}
}

func TestLoadMalformedConfig(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "invalid.yml")
	t.Setenv("ICHAT_CONFIG_FILE", configFile)
	if err := os.WriteFile(configFile, []byte("APP: ["), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("malformed config must return an error")
	}
}
