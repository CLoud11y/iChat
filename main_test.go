package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRunMissingConfig(t *testing.T) {
	t.Setenv("ICHAT_CONFIG_FILE", filepath.Join(t.TempDir(), "missing.yml"))
	if err := run(); err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("run() error = %v, want config error before opening dependencies", err)
	}
}
