package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"iChat/messaging/rabbitmq"
)

var mqKeys = []string{"url", "vhost", "managementURL", "managementUsername", "managementPassword", "publisherSlots", "publishTimeout", "connectTimeout", "writeTimeout", "closeTimeout", "heartbeat", "retryMin", "retryMax", "consumerWorkers", "consumerPrefetch", "consumerOperationTimeout"}

func mqConfigFile(t *testing.T, yaml string) string {
	t.Helper()
	// Never inherit local MQ credentials or timing overrides into these tests.
	for _, key := range append([]string{""}, mqKeys...) {
		name := "ICHAT_RABBITMQ"
		if key != "" {
			name += "_" + strings.ToUpper(key)
		}
		old, present := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(name, old)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICHAT_CONFIG_FILE", path)
	return path
}

const validMQYAML = `APP:
  name: test
RABBITMQ:
  url: 'amqp://app:secret-amqp@127.0.0.1:5672/%2Fichat'
  managementURL: 'http://127.0.0.1:15672'
  managementUsername: 'observer'
  managementPassword: 'secret-management'
`

func TestRabbitMQDefaultsAndIndependentReload(t *testing.T) {
	path := mqConfigFile(t, "APP:\n  name: legacy\n")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.RABBITMQ.VHost != "/ichat" || c.RABBITMQ.PublisherSlots != 8 || c.RABBITMQ.PublishTimeout != 5*time.Second ||
		c.RABBITMQ.ConnectTimeout != 5*time.Second || c.RABBITMQ.WriteTimeout != 3*time.Second || c.RABBITMQ.CloseTimeout != 3*time.Second ||
		c.RABBITMQ.Heartbeat != 2*time.Second || c.RABBITMQ.RetryMin != time.Second || c.RABBITMQ.RetryMax != 30*time.Second {
		t.Fatal("unexpected MQ defaults")
	}
	if _, err := c.RABBITMQ.Publisher(); !errors.Is(err, ErrRabbitMQNotConfigured) {
		t.Fatalf("unconfigured: %v", err)
	}
	if err := os.WriteFile(path, []byte(validMQYAML), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.RABBITMQ.Publisher()
	if err != nil {
		t.Fatal(err)
	}
	if p.URL != c.RABBITMQ.URL || p.VHost != "/ichat" || p.Management == nil || p.Options != rabbitmq.DefaultSupervisorOptions() {
		t.Fatal("configuration did not preserve defaults and map supervisor settings")
	}
	if err := os.WriteFile(path, []byte("APP:\n  name: legacy\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = Load()
	if err != nil || c.RABBITMQ.URL != "" || c.RABBITMQ.ManagementPassword != "" {
		t.Fatal("Load retained earlier MQ secrets")
	}
}

func TestRabbitMQEnvironmentOnlyAndOverrides(t *testing.T) {
	for _, yaml := range []string{"APP:\n  name: test\n", validMQYAML} {
		t.Run(fmt.Sprintf("yaml_bytes_%d", len(yaml)), func(t *testing.T) {
			mqConfigFile(t, yaml)
			values := map[string]string{
				"url": "amqps://env-user:env-secret@broker.example:5671/env-vhost", "vhost": "env-vhost",
				"managementURL": "https://broker.example:15671", "managementUsername": "env-observer", "managementPassword": "env-management",
				"publisherSlots": "3", "publishTimeout": "6s", "connectTimeout": "7s", "writeTimeout": "4s", "closeTimeout": "8s",
				"heartbeat": "3s", "retryMin": "2s", "retryMax": "40s",
				"consumerWorkers": "7", "consumerPrefetch": "23", "consumerOperationTimeout": "9s",
			}
			for key, value := range values {
				t.Setenv("ICHAT_RABBITMQ_"+strings.ToUpper(key), value)
			}
			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			p, err := c.RABBITMQ.Publisher()
			if err != nil {
				t.Fatal(err)
			}
			want := rabbitmq.DefaultSupervisorOptions()
			want.Session.Publisher.Slots = 3
			want.Session.OperationTimeout, want.Session.ConnectTimeout = 6*time.Second, 7*time.Second
			want.Session.WriteTimeout, want.Session.CloseTimeout, want.Session.Heartbeat = 4*time.Second, 8*time.Second, 3*time.Second
			want.RetryMin, want.RetryMax = 2*time.Second, 40*time.Second
			if p.Options != want || p.URL != values["url"] || p.VHost != values["vhost"] || c.RABBITMQ.ManagementURL != values["managementURL"] ||
				c.RABBITMQ.ManagementUsername != values["managementUsername"] || c.RABBITMQ.ManagementPassword != values["managementPassword"] {
				t.Fatal("MQ environment settings were dropped or did not override YAML")
			}
			consumer, err := c.RABBITMQ.Consumer()
			if err != nil || consumer.Workers != 7 || consumer.Prefetch != 23 || consumer.Options.OperationTimeout != 9*time.Second ||
				consumer.Options.ConnectTimeout != 7*time.Second || consumer.Options.WriteTimeout != 4*time.Second ||
				consumer.Options.CloseTimeout != 8*time.Second || consumer.Options.Heartbeat != 3*time.Second || consumer.Management == nil {
				t.Fatal("consumer environment settings were dropped or mapped to publisher timeout", err)
			}
		})
	}
}

func TestRabbitMQInvalidInputAndRedactedErrors(t *testing.T) {
	cases := []struct{ name, yaml, key, value string }{
		{"missing_amqp", "RABBITMQ:\n  managementPassword: secret-management\n", "", ""},
		{"empty_password_override", validMQYAML, "managementPassword", ""},
		{"empty_url_override", validMQYAML, "url", ""},
		{"wrong_vhost", validMQYAML, "vhost", "ichat"},
		{"malformed_uri", validMQYAML, "url", "amqp://app:secret-amqp@%xx"},
		{"missing_amqp_password", validMQYAML, "url", "amqp://app@127.0.0.1/%2Fichat"},
		{"remote_cleartext", validMQYAML, "url", "amqp://app:secret-amqp@broker.example/%2Fichat"},
		{"uri_query", validMQYAML, "url", "amqp://app:secret-amqp@127.0.0.1/%2Fichat?secret-management"},
		{"management_path", validMQYAML, "managementURL", "http://127.0.0.1:15672/api/secret-management"},
		{"management_userinfo", validMQYAML, "managementURL", "http://app:secret-management@127.0.0.1:15672"},
		{"remote_management_cleartext", validMQYAML, "managementURL", "http://broker.example"},
		{"slots_zero", validMQYAML, "publisherSlots", "0"},
		{"slots_overflow", validMQYAML, "publisherSlots", "65"},
		{"slots_invalid", validMQYAML, "publisherSlots", "secret-management"},
		{"slots_float", validMQYAML, "publisherSlots", "2.5"},
		{"negative_duration", validMQYAML, "writeTimeout", "-1s"},
		{"zero_duration", validMQYAML, "publishTimeout", "0s"},
		{"long_duration", validMQYAML, "connectTimeout", "61s"},
		{"duration_overflow", validMQYAML, "closeTimeout", "999999999999999h"},
		{"duration_invalid", validMQYAML, "writeTimeout", "secret-amqp"},
		{"empty_duration", validMQYAML, "heartbeat", ""},
		{"heartbeat_fraction", validMQYAML, "heartbeat", "1500ms"},
		{"tiny_retry", validMQYAML, "retryMin", "1us"},
		{"retry_inverted", validMQYAML, "retryMin", "40s"},
		{"section_scalar", "RABBITMQ: secret-amqp\n", "", ""},
		{"unknown_key", validMQYAML + "  publisherSlot: 3\n", "", ""},
		{"numeric_duration", validMQYAML + "  writeTimeout: 123\n", "", ""},
		{"null_duration", validMQYAML + "  writeTimeout: null\n", "", ""},
		{"yaml_slots_float", validMQYAML + "  publisherSlots: 2.5\n", "", ""},
		{"empty_slots", validMQYAML, "publisherSlots", ""},
		{"bool_url", "RABBITMQ:\n  url: true\n", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mqConfigFile(t, tc.yaml)
			if tc.key != "" {
				t.Setenv("ICHAT_RABBITMQ_"+strings.ToUpper(tc.key), tc.value)
			}
			c, err := Load()
			if !errors.Is(err, ErrRabbitMQConfig) || c != (Config{}) {
				t.Fatalf("invalid config was not rejected: %v", err)
			}
			if strings.Contains(err.Error(), "secret-amqp") || strings.Contains(err.Error(), "secret-management") {
				t.Fatal("error leaked a secret")
			}
		})
	}
}

func TestRabbitMQExampleAndSyntaxError(t *testing.T) {
	yaml, err := os.ReadFile("config.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	path := mqConfigFile(t, string(yaml))
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RABBITMQ.Publisher(); !errors.Is(err, ErrRabbitMQNotConfigured) {
		t.Fatal("example must preserve legacy startup")
	}
	if err := os.WriteFile(path, []byte("RABBITMQ: [secret-amqp, secret-management"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = Load()
	if err == nil || c != (Config{}) {
		t.Fatal("malformed syntax was accepted")
	}
	if strings.Contains(err.Error(), "secret-amqp") || strings.Contains(err.Error(), "secret-management") {
		t.Fatal("parse error echoed configuration text")
	}
}

func TestRabbitMQFormattingAndExplicitValidation(t *testing.T) {
	mqConfigFile(t, validMQYAML)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.RABBITMQ.Publisher()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{c, &c, c.RABBITMQ, &c.RABBITMQ, p, &p} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			output := fmt.Sprintf(format, value)
			if strings.Contains(output, "secret-amqp") || strings.Contains(output, "secret-management") {
				t.Fatal("configuration formatting leaked credentials")
			}
		}
	}
	c.RABBITMQ.PublishTimeout = 0
	if p, err := c.RABBITMQ.Publisher(); !errors.Is(err, ErrRabbitMQConfig) || p != (PublisherConfig{}) {
		t.Fatal("explicit invalid configuration accepted")
	}
}

func TestRabbitMQLoadAndBuildDoNotConnect(t *testing.T) {
	mqConfigFile(t, validMQYAML)
	for _, key := range []string{"url", "managementURL"} {
		listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		value := "http://" + listener.Addr().String()
		if key == "url" {
			value = "amqp://app:secret-amqp@" + listener.Addr().String() + "/%2Fichat"
		}
		t.Setenv("ICHAT_RABBITMQ_"+strings.ToUpper(key), value)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.RABBITMQ.Publisher(); err != nil {
			t.Fatal(err)
		}
		if err := listener.SetDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		conn, err := listener.Accept()
		if conn != nil {
			conn.Close()
			t.Fatal("configuration unexpectedly opened a connection")
		}
		var networkError net.Error
		if !errors.As(err, &networkError) || !networkError.Timeout() {
			t.Fatalf("unexpected listener result: %v", err)
		}
	}
}
