//go:build integration

package testenv

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Runs the actual command binary, with fixture-only settings in a private file.
// Buffers are read only after Wait joins. No credential or packet logging.
type replayCLIProcess struct {
	cmd         *exec.Cmd
	done        chan struct{}
	err         error
	out, stderr bytes.Buffer
	once        sync.Once
}

func buildReplayCLI(t *testing.T, ctx context.Context) string {
	t.Helper()
	program := filepath.Join(t.TempDir(), "mqadmin.exe")
	cmd := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-race", "-o", program, "./cmd/mqadmin")
	cmd.Dir = filepath.Join("..", "..")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mqadmin failed: %v (%d diagnostic bytes)", err, len(output))
	}
	return program
}

func replayCLIConfig(t *testing.T, f *persistenceLifecycleFixture, sqlAddress, amqpAddress string) string {
	t.Helper()
	if sqlAddress == "" {
		sqlAddress = f.dsn.Addr
	}
	host, port, err := net.SplitHostPort(sqlAddress)
	if err != nil {
		t.Fatal("invalid fixture SQL address")
	}
	broker := f.broker
	if amqpAddress != "" {
		broker.Address = amqpAddress
	}
	settings := map[string]any{
		"MYSQL": map[string]any{"user": f.dsn.User, "password": f.dsn.Passwd, "host": host, "port": port, "dbname": f.dsn.DBName},
		"RABBITMQ": map[string]any{"url": broker.AMQPURL(), "vhost": broker.VHost, "managementURL": "http://" + broker.ManagementAddress, "managementUsername": rabbitAdminUser, "managementPassword": f.env.password,
			"publisherSlots": "1", "publishTimeout": "800ms", "connectTimeout": "2s", "writeTimeout": "1s", "closeTimeout": "500ms", "heartbeat": "1s"},
		"RUNTIME": map[string]any{"role": "invalid"}, "REDIS": "invalid",
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal("encode private CLI configuration failed")
	}
	file := filepath.Join(t.TempDir(), "replay.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func startReplayCLI(t *testing.T, ctx context.Context, program, file string, args ...string) *replayCLIProcess {
	t.Helper()
	p := &replayCLIProcess{cmd: exec.CommandContext(ctx, program, args...), done: make(chan struct{})}
	for _, entry := range os.Environ() {
		key := strings.ToUpper(strings.SplitN(entry, "=", 2)[0])
		if key != "ICHAT_CONFIG_FILE" && key != "GORACE" && !strings.HasPrefix(key, "ICHAT_MYSQL_") && !strings.HasPrefix(key, "ICHAT_RABBITMQ_") {
			p.cmd.Env = append(p.cmd.Env, entry)
		}
	}
	p.cmd.Env = append(p.cmd.Env, "ICHAT_CONFIG_FILE="+file, "GORACE=atexit_sleep_ms=0")
	p.cmd.Stdout, p.cmd.Stderr = &p.out, &p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal("start fixture CLI failed", err)
	}
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { p.stop(t, false) })
	return p
}

func (p *replayCLIProcess) stop(t *testing.T, mustKill bool) {
	t.Helper()
	p.once.Do(func() {
		select {
		case <-p.done:
			if mustKill {
				t.Error("CLI exited before the intended kill barrier")
			}
			return
		default:
		}
		if err := p.cmd.Process.Kill(); err != nil {
			t.Error("kill owned CLI process failed", err)
		}
		select {
		case <-p.done:
			if mustKill && p.err == nil {
				t.Error("CLI did not exit from the kill")
			}
		case <-time.After(5 * time.Second):
			t.Error("owned CLI process did not join")
		}
	})
}

func (p *replayCLIProcess) wait(t *testing.T, target any) error {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(25 * time.Second):
		p.stop(t, false)
		t.Fatal("CLI did not finish within fixture deadline")
	}
	if p.out.Len() != 0 && target != nil {
		if json.Unmarshal(p.out.Bytes(), target) != nil {
			t.Fatal("CLI output was not a single valid result")
		}
	}
	return p.err
}
