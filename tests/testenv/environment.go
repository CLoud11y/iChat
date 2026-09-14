// Package testenv provisions disposable infrastructure only when Start is called.
// It never reads the application's configuration or accepts database addresses.
package testenv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iChat/config"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const runLabel = "io.ichat.test.run"
const projectLabel = "com.docker.compose.project"

var validRunID = regexp.MustCompile(`^[0-9a-f]{32}$`)

type commandRunner func(context.Context, ...string) ([]byte, error)

// Environment owns a single run's containers, volumes and network. Calls on an
// Environment must be serialized; separate environments may run concurrently.
type Environment struct {
	runID    string
	password string
	compose  string
	envFile  string
	run      commandRunner
	conf     config.Config
	ready    bool
	owned    bool
}

func randomID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

// Start launches a fresh Compose project and waits for authenticated health
// checks. The caller must Close it after closing all database/client handles.
// On partial startup failure, cleanup is attempted using a fresh timeout.
func Start(ctx context.Context, composeFile string) (*Environment, error) {
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	password, err := randomID()
	if err != nil {
		return nil, err
	}
	compose, err := filepath.Abs(composeFile)
	if err != nil {
		return nil, err
	}
	if filepath.Base(compose) != "docker-compose.test.yml" {
		return nil, errors.New("test environment requires docker-compose.test.yml")
	}
	file, err := os.CreateTemp("", "ichat-test-*.env")
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(file.Name())
		return nil, err
	}
	e := &Environment{runID: id, password: password, compose: compose, envFile: file.Name()}
	e.run = e.docker
	if err := e.start(ctx); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cleanupErr := e.Close(cleanupCtx)
		return nil, errors.Join(fmt.Errorf("start test run %s: %w", id, err), cleanupErr)
	}
	return e, nil
}

func (e *Environment) project() string { return "ichat-test-" + e.runID }

func (e *Environment) docker(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	// Explicit -f/-p and an empty --env-file prevent development .env settings
	// from affecting the test stack. Drop inherited Compose/test overrides too.
	for _, value := range os.Environ() {
		key := strings.ToUpper(strings.SplitN(value, "=", 2)[0])
		if !strings.HasPrefix(key, "COMPOSE_") && !strings.HasPrefix(key, "ICHAT_TEST_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "ICHAT_TEST_RUN_ID="+e.runID, "ICHAT_TEST_PASSWORD="+e.password)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Avoid exposing generated credentials even if Docker includes config
		// fragments in an error. Inspect success output stays internal as well.
		return nil, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.ReplaceAll(string(output), e.password, "[redacted]"))
	}
	return output, nil
}

func (e *Environment) composeArgs(args ...string) []string {
	return append([]string{"compose", "--env-file", e.envFile, "-f", e.compose, "-p", e.project()}, args...)
}

func (e *Environment) start(ctx context.Context) error {
	if !validRunID.MatchString(e.runID) {
		return errors.New("invalid test run ID")
	}
	existing, err := e.resources(ctx)
	if err != nil {
		return err
	}
	if len(existing) != 0 {
		return errors.New("refusing to reuse an existing test project")
	}
	e.owned = true
	if _, err := e.run(ctx, e.composeArgs("up", "-d", "--wait", "--wait-timeout", "180")...); err != nil {
		return err
	}
	resources, err := e.resources(ctx)
	if err != nil {
		return err
	}
	if err := e.verify(ctx, resources); err != nil {
		return err
	}
	mysqlAddr, err := e.address(ctx, "mysql", "3306")
	if err != nil {
		return err
	}
	redisAddr, err := e.address(ctx, "redis", "6379")
	if err != nil {
		return err
	}
	e.conf.MYSQL.Host, e.conf.MYSQL.Port, _ = net.SplitHostPort(mysqlAddr)
	e.conf.MYSQL.User = "ichat_test"
	e.conf.MYSQL.Password = e.password
	e.conf.MYSQL.DbName = "ichat_test_" + e.runID
	e.conf.REDIS.Addr = redisAddr
	e.conf.REDIS.Password = e.password
	e.conf.REDIS.PoolSize = 4
	e.ready = true
	return nil
}

// Config returns only the endpoints discovered from this run's Compose stack.
func (e *Environment) Config() (config.Config, error) {
	if !e.ready {
		return config.Config{}, errors.New("test environment is not ready")
	}
	return e.conf, nil
}

func (e *Environment) RunID() string { return e.runID }

func (e *Environment) address(ctx context.Context, service, port string) (string, error) {
	output, err := e.run(ctx, e.composeArgs("port", service, port)...)
	if err != nil {
		return "", err
	}
	address := strings.TrimSpace(string(output))
	host, port, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" {
		return "", fmt.Errorf("invalid test endpoint %q", address)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", fmt.Errorf("invalid test port %q", port)
	}
	return address, nil
}

type resource struct{ kind, id string }

func (e *Environment) resources(ctx context.Context) ([]resource, error) {
	if !validRunID.MatchString(e.runID) {
		return nil, errors.New("refusing resource access with invalid run ID")
	}
	var resources []resource
	for _, kind := range []string{"container", "volume", "network"} {
		args := []string{kind, "ls", "--quiet", "--filter", "label=" + projectLabel + "=" + e.project()}
		if kind == "container" {
			args = append(args, "--all")
		}
		output, err := e.run(ctx, args...)
		if err != nil {
			return nil, err
		}
		for _, id := range strings.Fields(string(output)) {
			resources = append(resources, resource{kind, id})
		}
	}
	return resources, nil
}

func (e *Environment) verify(ctx context.Context, resources []resource) error {
	for _, r := range resources {
		output, err := e.run(ctx, r.kind, "inspect", r.id)
		if err != nil {
			return err
		}
		var records []struct {
			Labels map[string]string
			Config struct{ Labels map[string]string }
		}
		if err := json.Unmarshal(output, &records); err != nil || len(records) != 1 {
			return fmt.Errorf("cannot verify %s %s", r.kind, r.id)
		}
		labels := records[0].Labels
		if r.kind == "container" {
			labels = records[0].Config.Labels
		}
		if labels[projectLabel] != e.project() || labels[runLabel] != e.runID {
			return fmt.Errorf("refusing foreign %s %s: ownership labels do not match run %s", r.kind, r.id, e.runID)
		}
	}
	return nil
}

// Close checks ALL resource ownership before any removal. It removes only the
// enumerated run's containers, volumes and network, never SQL tables or Redis
// keys in a shared instance. On a mismatch it leaves everything for inspection.
func (e *Environment) Close(ctx context.Context) error {
	e.ready = false
	if e.envFile != "" {
		defer os.Remove(e.envFile)
	}
	if !e.owned {
		return nil
	}
	resources, err := e.resources(ctx)
	if err != nil {
		return err
	}
	if err := e.verify(ctx, resources); err != nil {
		return err
	}
	for _, r := range resources {
		args := []string{r.kind, "rm"}
		if r.kind == "container" {
			args = append(args, "--force")
		}
		if _, err := e.run(ctx, append(args, r.id)...); err != nil {
			return err
		}
	}
	e.owned = false
	return nil
}
