package testenv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type rabbitTestAPI struct {
	mu              sync.Mutex
	version         string
	existing        bool
	existingUser    bool
	badMetadata     bool
	badPermissions  bool
	extraPermission bool
	privileged      bool
	failUser        bool
	vhostCreated    bool
	userCreated     bool
	writes          []string
	userPassword    string
}

func rabbitTestEnvironment(t *testing.T, state *rabbitTestAPI) *Environment {
	t.Helper()
	e := &Environment{runID: testRunID, password: "admin-secret", owned: true, rabbitEnabled: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		user, password, ok := r.BasicAuth()
		if !ok || user != rabbitAdminUser || password != e.password {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		vhost := "ichat_test_" + e.runID
		username := "ichat_app_" + e.runID
		if r.Method == http.MethodPut {
			state.writes = append(state.writes, r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/overview":
			json.NewEncoder(w).Encode(map[string]string{"rabbitmq_version": state.version})
		case "/api/vhosts/" + vhost:
			if r.Method == http.MethodPut {
				var metadata map[string]string
				if err := json.NewDecoder(r.Body).Decode(&metadata); err != nil || metadata["description"] != "iChat test run "+e.runID || metadata["default_queue_type"] != "quorum" || metadata["tags"] != "ichat-test,run:"+e.runID {
					w.WriteHeader(400)
					return
				}
				state.vhostCreated = true
				w.WriteHeader(201)
				return
			}
			if !state.vhostCreated && !state.existing {
				w.WriteHeader(404)
				return
			}
			metadata := rabbitVHost{Name: vhost, Description: "iChat test run " + e.runID, Tags: []string{"ichat-test", "run:" + e.runID}, DefaultQueueType: "quorum"}
			if state.badMetadata {
				metadata.Tags[1] = "run:foreign"
			}
			json.NewEncoder(w).Encode(metadata)
		case "/api/users/" + username:
			if r.Method == http.MethodPut {
				if state.failUser {
					w.WriteHeader(500)
					w.Write([]byte(e.password))
					return
				}
				var data map[string]string
				if err := json.NewDecoder(r.Body).Decode(&data); err != nil || data["tags"] != "" || len(data["password"]) != 32 {
					w.WriteHeader(400)
					return
				}
				state.userCreated = true
				state.userPassword = data["password"]
				w.WriteHeader(201)
				return
			}
			if !state.userCreated && !state.existingUser {
				w.WriteHeader(404)
				return
			}
			tags := []string{}
			if state.privileged {
				tags = []string{"administrator"}
			}
			json.NewEncoder(w).Encode(map[string]any{"name": username, "tags": tags})
		case "/api/permissions/" + vhost + "/" + username:
			var data map[string]string
			if err := json.NewDecoder(r.Body).Decode(&data); err != nil || data["configure"] != rabbitResourcePattern || data["write"] != rabbitResourcePattern || data["read"] != rabbitResourcePattern {
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(201)
		case "/api/users/" + username + "/permissions":
			permissions := []rabbitPermissions{{User: username, VHost: vhost, Configure: rabbitResourcePattern, Write: rabbitResourcePattern, Read: rabbitResourcePattern}}
			if state.badPermissions {
				permissions[0].Read = ".*"
			}
			if state.extraPermission {
				permissions = append(permissions, rabbitPermissions{User: username, VHost: "/ichat"})
			}
			json.NewEncoder(w).Encode(permissions)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	address := strings.TrimPrefix(server.URL, "http://")
	e.run = func(_ context.Context, args ...string) ([]byte, error) {
		if args[len(args)-3] == "port" {
			if args[len(args)-1] == "5672" {
				return []byte("127.0.0.1:56789"), nil
			}
			return []byte(address), nil
		}
		return nil, fmt.Errorf("unexpected docker command %v", args)
	}
	return e
}

func TestRabbitMQSetupOwnershipAndPermissionGuards(t *testing.T) {
	testCases := []struct {
		name       string
		state      rabbitTestAPI
		wantWrites int
		success    bool
	}{
		{"valid", rabbitTestAPI{version: RabbitMQVersion}, 3, true},
		{"wrong_version", rabbitTestAPI{version: "4.3.5"}, 0, false},
		{"existing_vhost", rabbitTestAPI{version: RabbitMQVersion, existing: true}, 0, false},
		{"existing_user", rabbitTestAPI{version: RabbitMQVersion, existingUser: true}, 0, false},
		{"foreign_metadata", rabbitTestAPI{version: RabbitMQVersion, badMetadata: true}, 3, false},
		{"wide_permissions", rabbitTestAPI{version: RabbitMQVersion, badPermissions: true}, 3, false},
		{"other_vhost_permission", rabbitTestAPI{version: RabbitMQVersion, extraPermission: true}, 3, false},
		{"privileged_user", rabbitTestAPI{version: RabbitMQVersion, privileged: true}, 3, false},
		{"partial_creation", rabbitTestAPI{version: RabbitMQVersion, failUser: true}, 2, false},
	}
	for i := range testCases {
		tc := &testCases[i]
		t.Run(tc.name, func(t *testing.T) {
			state := &tc.state
			e := rabbitTestEnvironment(t, state)
			err := e.initializeRabbitMQ(context.Background())
			if (err == nil) != tc.success {
				t.Fatalf("setup success=%t error=%v", tc.success, err)
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if len(state.writes) != tc.wantWrites {
				t.Fatalf("setup wrote %d operations, want %d", len(state.writes), tc.wantWrites)
			}
			if err != nil && strings.Contains(err.Error(), e.password) {
				t.Fatal("API response leaked credentials")
			}
			if _, err := e.RabbitMQ(); err == nil {
				t.Fatal("config exposed before overall fixture readiness")
			}
			if tc.success {
				e.ready = true
				conf, err := e.RabbitMQ()
				if err != nil || conf.Password != state.userPassword || conf.Password == e.password || conf.VHost != "ichat_test_"+testRunID {
					t.Fatal("invalid dedicated credentials/config")
				}
				parsed, err := url.Parse(conf.AMQPURL())
				if err != nil {
					t.Fatal(err)
				}
				password, _ := parsed.User.Password()
				if parsed.User.Username() != conf.Username || password != conf.Password || parsed.Path != "/"+conf.VHost {
					t.Fatal("incorrect AMQP URI encoding")
				}
				if strings.Contains(fmt.Sprintf("%+v", conf), conf.Password) || strings.Contains(conf.String(), e.password) {
					t.Fatal("config print leaked credentials")
				}
				e.ready = false
				if _, err := e.RabbitMQ(); err == nil {
					t.Fatal("closed config exposed")
				}
			}
		})
	}
}

func TestRabbitMQOptionalProfileAndRejectedNamespace(t *testing.T) {
	e := &Environment{runID: testRunID}
	if strings.Contains(strings.Join(e.composeArgs("up"), " "), "--profile") {
		t.Fatal("default fixture enabled MQ")
	}
	e.rabbitEnabled = true
	if !strings.Contains(strings.Join(e.composeArgs("up"), " "), "--profile mq up") {
		t.Fatal("MQ fixture did not enable profile")
	}
	if _, err := e.RabbitMQ(); err == nil {
		t.Fatal("unready config exposed")
	}
	if _, err := StartWithRabbitMQ(context.Background(), filepath.Join(t.TempDir(), "docker-compose.yml")); err == nil {
		t.Fatal("MQ adopted development compose")
	}
	for _, invalid := range []*Environment{{runID: "development", owned: true}, {runID: testRunID, owned: false}} {
		invalid.run = func(context.Context, ...string) ([]byte, error) {
			t.Fatal("invalid namespace accessed Docker")
			return nil, nil
		}
		if err := invalid.initializeRabbitMQ(context.Background()); err == nil {
			t.Fatal("unowned MQ setup accepted")
		}
	}
}

func TestRabbitMQAPIBoundsAndRedirects(t *testing.T) {
	var redirected bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/redirect":
			http.Redirect(w, r, target.URL, 307)
		case "/api/large":
			w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
		case "/api/bad-json":
			w.Write([]byte(`{"unfinished":`))
		default:
			w.WriteHeader(500)
			w.Write([]byte("admin-secret"))
		}
	}))
	defer server.Close()
	e := &Environment{runID: testRunID, owned: true, password: "admin-secret", rabbitConfig: RabbitMQConfig{ManagementAddress: strings.TrimPrefix(server.URL, "http://")}}
	for _, path := range []string{"/api/redirect", "/api/large", "/api/bad-json", "/api/fail"} {
		var result map[string]any
		if _, err := e.rabbitAPI(context.Background(), http.MethodGet, path, nil, &result); err == nil || strings.Contains(err.Error(), e.password) {
			t.Fatalf("unsafe response from %s: %v", path, err)
		}
	}
	if redirected {
		t.Fatal("management credentials followed redirect")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.rabbitAPI(ctx, http.MethodGet, "/api/fail", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	e.rabbitConfig.ManagementAddress = "example.com:15672"
	if _, err := e.rabbitAPI(context.Background(), http.MethodPut, "/api/vhosts/ichat", nil, nil); err == nil {
		t.Fatal("foreign management endpoint accepted")
	}
}
