package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the real main and its process exit code without building a second
// executable. Ordinary unit tests never open a database or start Docker.
func TestMigrationProcess(t *testing.T) {
	if os.Getenv("ICHAT_MIGRATE_TEST_PROCESS") != "1" {
		return
	}
	main()
	os.Exit(0)
}

func migrationProcess(t *testing.T, configFile string) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMigrationProcess$")
	// Do not let inherited application settings override the isolated config.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "ICHAT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "ICHAT_MIGRATE_TEST_PROCESS=1", "ICHAT_CONFIG_FILE="+configFile)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("migration process timed out: %v", ctx.Err())
	}
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("start migration process: %v", err)
	}
	return stdout.String(), stderr.String(), exitErr.ExitCode()
}

func TestMigrationConfigFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "missing"},
		{name: "malformed", body: "MYSQL: ["},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.yml")
			if tc.body != "" {
				if err := os.WriteFile(file, []byte(tc.body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			stdout, stderr, code := migrationProcess(t, file)
			if code != 1 || !strings.Contains(stderr, "read config") || strings.Contains(stdout, "migration completed") {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
}
