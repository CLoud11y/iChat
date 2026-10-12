package rabbitmq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Management holds operator credentials, never application credentials. It is
// used by the explicit administration command and topology checks only.
type Management struct {
	baseURL  string
	username string
	password string
	client   *http.Client
}

func (m *Management) String() string {
	if m == nil {
		return "rabbitmq management unavailable"
	}
	return "rabbitmq management " + m.baseURL
}

func NewManagement(address, username, password string) (*Management, error) {
	u, err := url.Parse(address)
	if err != nil || u == nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("invalid RabbitMQ management endpoint")
	}
	loopback := u.Hostname() == "localhost"
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
		return nil, errors.New("RabbitMQ management requires HTTPS or loopback HTTP")
	}
	if username == "" || password == "" {
		return nil, errors.New("RabbitMQ management credentials are required")
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, TLSHandshakeTimeout: 3 * time.Second, DisableKeepAlives: true}
	return &Management{baseURL: strings.TrimRight(address, "/"), username: username, password: password, client: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (m *Management) request(ctx context.Context, method, path string, body, result any) (int, error) {
	if m == nil || m.client == nil {
		return 0, errors.New("RabbitMQ management is not configured")
	}
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return 0, errors.New("encode RabbitMQ management request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, m.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return 0, errors.New("build RabbitMQ management request")
	}
	req.SetBasicAuth(m.username, m.password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		// Do not expose response bodies, credentials or URLs through errors.
		return 0, errors.New("RabbitMQ management request failed or timed out")
	}
	defer resp.Body.Close()
	const limit = 1 << 20
	data, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(data) > limit {
		return resp.StatusCode, errors.New("invalid RabbitMQ management response size")
	}
	if resp.StatusCode == http.StatusNotFound && method == http.MethodGet {
		return resp.StatusCode, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("RabbitMQ management returned HTTP %d", resp.StatusCode)
	}
	if result != nil {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(result); err != nil {
			return resp.StatusCode, errors.New("invalid RabbitMQ management JSON")
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return resp.StatusCode, errors.New("trailing RabbitMQ management JSON")
		}
	}
	return resp.StatusCode, nil
}

// ProvisionLocalUser creates the fixed local application account. Existing
// passwords are never changed; foreign permissions or management tags fail.
// The caller authenticates with the supplied app password over AMQP afterward.
func (m *Management) ProvisionLocalUser(ctx context.Context, password string) error {
	if m == nil {
		return errors.New("RabbitMQ management is not configured")
	}
	if password == "" || password == m.password {
		return errors.New("RabbitMQ application password must be nonempty and different from the admin password")
	}
	if _, err := m.get(ctx, "/api/vhosts/"+url.PathEscape(LocalVHost), new(any)); err != nil {
		return err
	}
	var user struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	status, err := m.request(ctx, http.MethodGet, "/api/users/"+LocalAppUser, nil, &user)
	if err != nil {
		return err
	}
	if status != http.StatusNotFound {
		if user.Name != LocalAppUser || len(user.Tags) != 0 {
			return errors.New("existing RabbitMQ application account has unexpected identity or management tags")
		}
		var permissions []struct {
			User      string `json:"user"`
			VHost     string `json:"vhost"`
			Configure string `json:"configure"`
			Write     string `json:"write"`
			Read      string `json:"read"`
		}
		if _, err := m.get(ctx, "/api/users/"+LocalAppUser+"/permissions", &permissions); err != nil {
			return err
		}
		if len(permissions) == 1 && permissions[0].User == LocalAppUser && permissions[0].VHost == LocalVHost && permissions[0].Configure == ResourcePattern && permissions[0].Write == ResourcePattern && permissions[0].Read == ResourcePattern {
			return nil
		}
		if len(permissions) != 0 {
			return errors.New("existing RabbitMQ application account has unexpected permissions")
		}
	} else {
		if _, err := m.request(ctx, http.MethodPut, "/api/users/"+LocalAppUser, map[string]any{"password": password, "tags": []string{}}, nil); err != nil {
			return err
		}
	}
	_, err = m.request(ctx, http.MethodPut, "/api/permissions/"+url.PathEscape(LocalVHost)+"/"+LocalAppUser, map[string]string{"configure": ResourcePattern, "write": ResourcePattern, "read": ResourcePattern}, nil)
	return err
}

func (m *Management) get(ctx context.Context, path string, result any) (int, error) {
	status, err := m.request(ctx, http.MethodGet, path, nil, result)
	if err == nil && status == http.StatusNotFound {
		err = errors.New("RabbitMQ topology object is missing")
	}
	return status, err
}
