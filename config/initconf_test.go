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

func TestLegacyWorkerExplicitEnablement(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, env  string
		enabled, invalid bool
	}{
		{"omitted", "APP:\n  name: test\n", "", false, false},
		{"yaml_true", "LEGACY_WORKER:\n  enabled: true\n", "", true, false},
		{"yaml_false", "LEGACY_WORKER:\n  enabled: false\n", "", false, false},
		{"env_only", "APP:\n  name: test\n", "true", true, false},
		{"env_disables", "LEGACY_WORKER:\n  enabled: true\n", "false", false, false},
		{"invalid_env", "APP:\n  name: test\n", "invalid", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ICHAT_LEGACY_WORKER_ENABLED", tc.env)
			file := filepath.Join(t.TempDir(), "config.yml")
			t.Setenv("ICHAT_CONFIG_FILE", file)
			if err := os.WriteFile(file, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			conf, err := Load()
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid worker flag accepted")
				}
				return
			}
			if err != nil || conf.LEGACY_WORKER.Enabled != tc.enabled {
				t.Fatalf("enabled=%v want=%v err=%v", conf.LEGACY_WORKER.Enabled, tc.enabled, err)
			}
		})
	}
}
