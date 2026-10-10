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

func TestMigrationProcess(t *testing.T) {
	if os.Getenv("ICHAT_RELIABLE_MIGRATE_TEST_PROCESS") != "1" {
		return
	}
	main()
	os.Exit(0)
}

func TestMigrationConfigFailure(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
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
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMigrationProcess$")
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(strings.ToUpper(entry), "ICHAT_") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "ICHAT_RELIABLE_MIGRATE_TEST_PROCESS=1", "ICHAT_CONFIG_FILE="+file)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(stderr.String(), "read config") || stdout.Len() != 0 {
				t.Fatalf("exit=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
		})
	}
}
