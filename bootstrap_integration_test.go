//go:build integration

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"iChat/config"
	"iChat/router"
	"iChat/service"
	"iChat/tests/testenv"
	"iChat/utils"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

func TestBootstrapMySQLRecoveryIntegration(t *testing.T) {
	f := testenv.NewLegacyFixture(t)
	conf, err := f.Env.Config()
	if err != nil {
		t.Fatal(err)
	}
	conf.JWT.Key, conf.JWT.TokenHourLifeSpan = "isolated-recovery-test-key", 1
	conf.LOG.AccessEnabled = false
	// Preserve the application endpoint across Docker's random port changes.
	// Only raw TCP routing changes; every handshake/query reaches real MySQL.
	proxy, target := recoveryTCPProxy(t, net.JoinHostPort(conf.MYSQL.Host, conf.MYSQL.Port))
	conf.MYSQL.Host, conf.MYSQL.Port, _ = net.SplitHostPort(proxy)
	if err := f.DB.Model(&f.Users[0]).Update("password", utils.Encrypt("recovery-password")).Error; err != nil {
		t.Fatal(err)
	}
	control := func(running bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := f.Env.SetMySQLRunning(ctx, running); err != nil {
			t.Fatal(err)
		}
		if running {
			current, err := f.Env.Config()
			if err != nil {
				t.Fatal(err)
			}
			target(net.JoinHostPort(current.MYSQL.Host, current.MYSQL.Port))
		}
	}
	control(false) // The real server is stopped before API construction.
	original := legacyState()
	deps := defaultStartupDependencies()
	deps.loadConfig = func() (config.Config, error) { return conf, nil }
	opener := deps.openMySQL
	var pool *sql.DB
	deps.openMySQL = func(c config.Config) (*gorm.DB, error) {
		db, err := opener(c)
		if err == nil {
			pool, err = db.DB()
		}
		return db, err
	}
	var health router.StorageHealth
	deps.serve = func(c config.Config, rds *redis.Client, users service.AccountUsers, contacts service.ContactQueries, writes service.ContactWrites, messages service.MessageStore, subscriptions service.MessageSubscriptions, storage router.StorageHealth) error {
		health = storage
		tokens, err := utils.NewTokenService(c, utils.NewRedisTokenBlacklist(rds))
		if err != nil {
			return err
		}
		token, err := tokens.GenerateToken(f.Users[0].ID)
		if err != nil {
			return err
		}
		server := httptest.NewServer(router.Router(c, tokens, users, contacts, writes, messages, subscriptions, storage))
		defer server.Close()
		client := &http.Client{Timeout: 12 * time.Second}
		request := func(method, path, body, bearer string) (int, string) {
			t.Helper()
			req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			if body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			if bearer != "" {
				req.Header.Set("Authorization", "Bearer "+bearer)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			return resp.StatusCode, string(data)
		}
		expect := func(method, path, body, bearer string, status int, fragment string) string {
			t.Helper()
			code, data := request(method, path, body, bearer)
			if code != status || !strings.Contains(data, fragment) {
				t.Fatalf("%s: status=%d body=%s, want %d %s", path, code, data, status, fragment)
			}
			return data
		}
		waitReady := func(status int) {
			t.Helper()
			deadline := time.NewTimer(60 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(100 * time.Millisecond)
			defer tick.Stop()
			for {
				code, _ := request("GET", "/health/ready/storage", "", "")
				if code == status {
					return
				}
				select {
				case <-deadline.C:
					t.Fatalf("readiness never became %d", status)
				case <-tick.C:
				}
			}
		}
		loginBody, _ := json.Marshal(map[string]string{"account": f.Users[0].Phone, "password": "recovery-password"})
		checkOffline := func() {
			expect("GET", "/health/live", "", "", 200, "live")
			expect("GET", "/health/ready/accept", "", "", 503, "MQ_ACCEPTANCE_NOT_CONFIGURED")
			expect("POST", "/auth/getContacts", "", "", 401, "UNAUTHENTICATED")
			expect("POST", "/auth/getContacts", "", token, 503, "DEPENDENCY_UNAVAILABLE")
			expect("POST", "/user/login", string(loginBody), "", 503, "DEPENDENCY_UNAVAILABLE")
		}
		waitReady(503)
		checkOffline()
		for cycle := 0; cycle < 2; cycle++ {
			control(true)
			waitReady(200)
			data := expect("POST", "/user/login", string(loginBody), "", 200, `"code":0`)
			var result struct {
				Data service.LoginResp `json:"data"`
			}
			if err := json.Unmarshal([]byte(data), &result); err != nil || result.Data.UserInfo.ID != f.Users[0].ID {
				t.Fatalf("recovered login identity mismatch: %v", err)
			}
			if _, err := tokens.ValidateToken(context.Background(), result.Data.Token); err != nil {
				t.Fatal(err)
			}
			expect("POST", "/auth/getContacts", "", token, 200, `"code":0`)
			t.Logf("recovery cycle %d: same API and injected repositories recovered", cycle+1)
			if cycle == 0 {
				control(false)
				waitReady(503)
				checkOffline()
			}
		}
		return nil
	}
	if err := bootstrap(deps); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, legacyState()) {
		t.Fatal("bootstrap did not restore globals")
	}
	if health == nil || health.StorageReady() {
		t.Fatal("probe not stopped")
	}
	if err := pool.Ping(); err == nil || !strings.Contains(err.Error(), "database is closed") {
		t.Fatalf("bootstrap pool not closed: %v", err)
	}
}

// This integration-only forwarder models a stable database service address.
// Cleanup closes active sockets, cancels dials and waits for all forwarding work.
func recoveryTCPProxy(t *testing.T, initial string) (string, func(string)) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var target atomic.Value
	target.Store(initial)
	ctx, cancel := context.WithCancel(context.Background())
	var active sync.Map
	var workers sync.WaitGroup
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			incoming, err := listener.Accept()
			if err != nil {
				return
			}
			active.Store(incoming, true)
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer active.Delete(incoming)
				defer incoming.Close()
				dialer := net.Dialer{Timeout: 2 * time.Second}
				outgoing, err := dialer.DialContext(ctx, "tcp", target.Load().(string))
				if err != nil {
					return
				}
				defer outgoing.Close()
				done := make(chan struct{})
				go func() { defer close(done); _, _ = io.Copy(incoming, outgoing); _ = incoming.Close() }()
				_, _ = io.Copy(outgoing, incoming)
				_ = outgoing.Close()
				<-done
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		<-accepted
		active.Range(func(key, _ any) bool { _ = key.(net.Conn).Close(); return true })
		workers.Wait()
	})
	return listener.Addr().String(), func(address string) { target.Store(address) }
}
