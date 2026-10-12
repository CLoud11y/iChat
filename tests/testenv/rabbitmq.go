package testenv

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

const RabbitMQVersion = "4.3.6"
const AMQPClientVersion = "v1.15.0"
const rabbitAdminUser = "ichat_test_admin"
const rabbitResourcePattern = `^ichat\.`

// RabbitMQConfig is returned only after container ownership, version, vhost
// metadata and the dedicated user's permissions have all been verified.
type RabbitMQConfig struct {
	Address           string
	ManagementAddress string
	Username          string
	Password          string
	VHost             string
}

func (c RabbitMQConfig) AMQPURL() string {
	return (&url.URL{Scheme: "amqp", Host: c.Address, User: url.UserPassword(c.Username, c.Password), Path: "/" + c.VHost}).String()
}

// String never includes credentials, even when test diagnostics print config.
func (c RabbitMQConfig) String() string {
	return fmt.Sprintf("rabbitmq address=%s vhost=%s", c.Address, c.VHost)
}

func (e *Environment) RabbitMQ() (RabbitMQConfig, error) {
	if !e.ready || !e.rabbitEnabled {
		return RabbitMQConfig{}, errors.New("RabbitMQ test environment is not ready")
	}
	return e.rabbitConfig, nil
}

type rabbitVHost struct {
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	Tags             []string `json:"tags"`
	DefaultQueueType string   `json:"default_queue_type"`
}
type rabbitPermissions struct {
	User      string `json:"user"`
	VHost     string `json:"vhost"`
	Configure string `json:"configure"`
	Write     string `json:"write"`
	Read      string `json:"read"`
}

func (e *Environment) initializeRabbitMQ(ctx context.Context) error {
	if !validRunID.MatchString(e.runID) || !e.owned {
		return errors.New("refusing RabbitMQ setup without test resource ownership")
	}
	address, err := e.address(ctx, "rabbitmq", "5672")
	if err != nil {
		return err
	}
	management, err := e.address(ctx, "rabbitmq", "15672")
	if err != nil {
		return err
	}
	password, err := randomID()
	if err != nil {
		return err
	}
	e.rabbitConfig = RabbitMQConfig{Address: address, ManagementAddress: management, Username: "ichat_app_" + e.runID, Password: password, VHost: "ichat_test_" + e.runID}
	// Compose health can precede management HTTP readiness. Retry only safe,
	// read-only probes with a bounded deadline; never blindly retry writes.
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var overview struct {
		Version string `json:"rabbitmq_version"`
	}
	for {
		_, err = e.rabbitAPI(probeCtx, http.MethodGet, "/api/overview", nil, &overview)
		if err == nil {
			break
		}
		select {
		case <-probeCtx.Done():
			return fmt.Errorf("RabbitMQ management did not become ready: %w", probeCtx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	if overview.Version != RabbitMQVersion {
		return fmt.Errorf("unexpected RabbitMQ version %q", overview.Version)
	}
	vhostPath := "/api/vhosts/" + url.PathEscape(e.rabbitConfig.VHost)
	userPath := "/api/users/" + url.PathEscape(e.rabbitConfig.Username)
	for _, path := range []string{vhostPath, userPath} {
		status, err := e.rabbitAPI(ctx, http.MethodGet, path, nil, nil)
		if err != nil {
			return err
		}
		if status != http.StatusNotFound {
			return errors.New("refusing to adopt an existing RabbitMQ test vhost or user")
		}
	}
	metadata := map[string]string{"description": "iChat test run " + e.runID, "tags": "ichat-test,run:" + e.runID, "default_queue_type": "quorum"}
	if _, err := e.rabbitAPI(ctx, http.MethodPut, vhostPath, metadata, nil); err != nil {
		return err
	}
	user := map[string]string{"password": password, "tags": ""}
	if _, err := e.rabbitAPI(ctx, http.MethodPut, userPath, user, nil); err != nil {
		return err
	}
	permissionsPath := "/api/permissions/" + url.PathEscape(e.rabbitConfig.VHost) + "/" + url.PathEscape(e.rabbitConfig.Username)
	permissions := map[string]string{"configure": rabbitResourcePattern, "write": rabbitResourcePattern, "read": rabbitResourcePattern}
	if _, err := e.rabbitAPI(ctx, http.MethodPut, permissionsPath, permissions, nil); err != nil {
		return err
	}
	return e.verifyRabbitMQ(ctx)
}

func (e *Environment) verifyRabbitMQ(ctx context.Context) error {
	if !validRunID.MatchString(e.runID) || e.rabbitConfig.VHost != "ichat_test_"+e.runID || e.rabbitConfig.Username != "ichat_app_"+e.runID {
		return errors.New("RabbitMQ test namespace does not match run")
	}
	var vhost rabbitVHost
	status, err := e.rabbitAPI(ctx, http.MethodGet, "/api/vhosts/"+url.PathEscape(e.rabbitConfig.VHost), nil, &vhost)
	if err != nil {
		return err
	}
	if status != http.StatusOK || vhost.Name != e.rabbitConfig.VHost || vhost.Description != "iChat test run "+e.runID || vhost.DefaultQueueType != "quorum" ||
		len(vhost.Tags) != 2 || !containsRabbitTag(vhost.Tags, "ichat-test") || !containsRabbitTag(vhost.Tags, "run:"+e.runID) {
		return errors.New("RabbitMQ test vhost ownership metadata does not match")
	}
	var permissions []rabbitPermissions
	if _, err := e.rabbitAPI(ctx, http.MethodGet, "/api/users/"+url.PathEscape(e.rabbitConfig.Username)+"/permissions", nil, &permissions); err != nil {
		return err
	}
	if len(permissions) != 1 {
		return errors.New("RabbitMQ test user must have exactly one vhost permission")
	}
	permission := permissions[0]
	if permission.User != e.rabbitConfig.Username || permission.VHost != e.rabbitConfig.VHost || permission.Configure != rabbitResourcePattern || permission.Write != rabbitResourcePattern || permission.Read != rabbitResourcePattern {
		return errors.New("RabbitMQ test user permissions do not match run")
	}
	var user struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if _, err := e.rabbitAPI(ctx, http.MethodGet, "/api/users/"+url.PathEscape(e.rabbitConfig.Username), nil, &user); err != nil {
		return err
	}
	if user.Name != e.rabbitConfig.Username || len(user.Tags) != 0 {
		return errors.New("RabbitMQ test user must not have management privileges")
	}
	return nil
}

func containsRabbitTag(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}

// Management operations target only this fixture's loopback endpoint. Disable
// proxies and redirects so inherited process settings cannot move credentials.
func (e *Environment) rabbitAPI(ctx context.Context, method, path string, body, result any) (int, error) {
	if !validRunID.MatchString(e.runID) || !e.owned {
		return 0, errors.New("RabbitMQ API requires owned test resources")
	}
	host, port, err := net.SplitHostPort(e.rabbitConfig.ManagementAddress)
	if err != nil || host != "127.0.0.1" || port == "" || !strings.HasPrefix(path, "/api/") {
		return 0, errors.New("invalid RabbitMQ test management endpoint")
	}
	var payload []byte
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return 0, err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://"+e.rabbitConfig.ManagementAddress+path, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	request.SetBasicAuth(rabbitAdminUser, e.password)
	request.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext}
	defer transport.CloseIdleConnections()
	client := http.Client{Timeout: 5 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("RabbitMQ test API request: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return 0, err
	}
	if len(data) > 1<<20 {
		return 0, errors.New("RabbitMQ test API response exceeds limit")
	}
	status := response.StatusCode
	if status == http.StatusNotFound && method == http.MethodGet {
		return status, nil
	}
	if status < 200 || status >= 300 {
		return status, fmt.Errorf("RabbitMQ test API %s returned HTTP %d", method, status)
	}
	if result != nil {
		if err := json.Unmarshal(data, result); err != nil {
			return status, fmt.Errorf("decode RabbitMQ test API: %w", err)
		}
	}
	return status, nil
}
