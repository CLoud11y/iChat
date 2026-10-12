package rabbitmq

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestLocalUserProvisioningGuards(t *testing.T) {
	for _, scenario := range []string{"new", "existing", "partial", "privileged", "foreign_permissions", "broad_permissions"} {
		t.Run(scenario, func(t *testing.T) {
			var mu sync.Mutex
			var writes []string
			userExists := scenario != "new"
			permissions := []map[string]string{}
			if scenario != "new" && scenario != "partial" {
				permissions = append(permissions, map[string]string{"user": LocalAppUser, "vhost": LocalVHost, "configure": ResourcePattern, "write": ResourcePattern, "read": ResourcePattern})
			}
			if scenario == "foreign_permissions" {
				permissions[0]["vhost"] = "/foreign"
			}
			if scenario == "broad_permissions" {
				permissions[0]["write"] = ".*"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				path := r.URL.EscapedPath()
				if r.Method == http.MethodGet {
					switch path {
					case "/api/vhosts/" + url.PathEscape(LocalVHost):
						json.NewEncoder(w).Encode(map[string]string{"name": LocalVHost})
					case "/api/users/" + LocalAppUser:
						if !userExists {
							w.WriteHeader(404)
							return
						}
						tags := []string{}
						if scenario == "privileged" {
							tags = append(tags, "administrator")
						}
						json.NewEncoder(w).Encode(map[string]any{"name": LocalAppUser, "tags": tags})
					case "/api/users/" + LocalAppUser + "/permissions":
						json.NewEncoder(w).Encode(permissions)
					default:
						t.Error("unexpected GET", path)
						w.WriteHeader(500)
					}
					return
				}
				if r.Method != http.MethodPut {
					t.Error("destructive method", r.Method)
					w.WriteHeader(500)
					return
				}
				writes = append(writes, path)
				var body map[string]any
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid provisioning body")
				}
				if path == "/api/users/"+LocalAppUser {
					if userExists || body["password"] != "app-secret" || len(body["tags"].([]any)) != 0 {
						t.Error("overwrote user or wrong credentials/tags")
					}
					userExists = true
				} else if path == "/api/permissions/"+url.PathEscape(LocalVHost)+"/"+LocalAppUser {
					for _, key := range []string{"configure", "write", "read"} {
						if body[key] != ResourcePattern {
							t.Error("broad application permission")
						}
					}
					permissions = []map[string]string{{"user": LocalAppUser, "vhost": LocalVHost, "configure": ResourcePattern, "write": ResourcePattern, "read": ResourcePattern}}
				} else {
					t.Error("unexpected write", path)
				}
				w.WriteHeader(204)
			}))
			defer server.Close()
			m, err := NewManagement(server.URL, "admin", "admin-secret")
			if err != nil {
				t.Fatal(err)
			}
			err = m.ProvisionLocalUser(context.Background(), "app-secret")
			mu.Lock()
			count := len(writes)
			mu.Unlock()
			want := 0
			switch scenario {
			case "new":
				want = 2
			case "partial":
				want = 1
			}
			if scenario == "privileged" || scenario == "foreign_permissions" || scenario == "broad_permissions" {
				if err == nil {
					t.Fatal("unsafe existing account adopted")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if count != want {
				t.Fatalf("writes=%d want=%d", count, want)
			}
			if strings.Contains(fmt.Sprintf("%+v", m), "secret") {
				t.Fatal("management config leaked password")
			}
			if scenario == "new" || scenario == "partial" {
				if err := m.ProvisionLocalUser(context.Background(), "app-secret"); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				count = len(writes)
				mu.Unlock()
				if count != want {
					t.Fatal("repeat provisioning overwrote account")
				}
			}
		})
	}
}
