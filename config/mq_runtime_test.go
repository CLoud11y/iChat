package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"iChat/messaging/rabbitmq"
)

func TestMQRuntimeConsumerDefaultsAndBoundaries(t *testing.T) {
	path := runtimeConfigFile(t, validMQYAML)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := c.RABBITMQ.Consumer()
	if err != nil || consumer.Workers != 4 || consumer.Prefetch != 16 || consumer.Options != rabbitmq.DefaultConnectionOptions() ||
		consumer.URL != c.RABBITMQ.URL || consumer.VHost != "/ichat" || consumer.Management == nil {
		t.Fatal("consumer defaults/transport mapping changed", err)
	}
	if err := os.WriteFile(path, []byte(validMQYAML+"  consumerWorkers: 32\n  consumerPrefetch: 256\n  consumerOperationTimeout: 60s\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	consumer, err = c.RABBITMQ.Consumer()
	if err != nil || consumer.Workers != MaxConsumerWorkers || consumer.Prefetch != rabbitmq.MaxConsumerPrefetch || consumer.Options.OperationTimeout != time.Minute {
		t.Fatal("upper admission bounds rejected", err)
	}
	if err := os.WriteFile(path, []byte(validMQYAML+"  consumerWorkers: 1\n  consumerPrefetch: 1\n  consumerOperationTimeout: 1ms\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RABBITMQ.Consumer(); err != nil {
		t.Fatal("lower admission bounds rejected", err)
	}
	if err := os.WriteFile(path, []byte(validMQYAML), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = Load()
	if err != nil || c.RABBITMQ.ConsumerWorkers != 4 || c.RABBITMQ.ConsumerPrefetch != 16 || c.RABBITMQ.ConsumerOperationTimeout != 5*time.Second {
		t.Fatal("consumer overrides persisted across Load", err)
	}
}

func TestMQRuntimeConsumerInvalidInputsAndDirectValidation(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"consumerWorkers", "0"}, {"consumerWorkers", "33"}, {"consumerWorkers", "2.5"}, {"consumerWorkers", ""},
		{"consumerPrefetch", "0"}, {"consumerPrefetch", "257"}, {"consumerPrefetch", "-1"}, {"consumerPrefetch", "SECRET_TOKEN"},
		{"consumerOperationTimeout", "0s"}, {"consumerOperationTimeout", "-1s"}, {"consumerOperationTimeout", "61s"},
		{"consumerOperationTimeout", ""}, {"consumerOperationTimeout", "999999999999999h"},
	} {
		t.Run(tc.key+"_"+tc.value, func(t *testing.T) {
			runtimeConfigFile(t, validMQYAML)
			t.Setenv("ICHAT_RABBITMQ_"+strings.ToUpper(tc.key), tc.value)
			c, err := Load()
			if !errors.Is(err, ErrRabbitMQConfig) || c != (Config{}) {
				t.Fatalf("invalid consumer input was loaded: %v", err)
			}
			if strings.Contains(err.Error(), "SECRET_TOKEN") {
				t.Fatal("error echoed raw consumer input")
			}
		})
	}
	for _, extra := range []string{"  consumerWorkers: true\n", "  consumerWorkers: null\n", "  consumerPrefetch: 1.5\n", "  consumerOperationTimeout: 123\n"} {
		t.Run(strings.TrimSpace(extra), func(t *testing.T) {
			runtimeConfigFile(t, validMQYAML+extra)
			if _, err := Load(); !errors.Is(err, ErrRabbitMQConfig) {
				t.Fatal("consumer YAML type was weakly coerced", err)
			}
		})
	}
	runtimeConfigFile(t, validMQYAML)
	base, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*RabbitMQConfig){
		func(c *RabbitMQConfig) { c.ConsumerWorkers = 0 }, func(c *RabbitMQConfig) { c.ConsumerPrefetch = 0 },
		func(c *RabbitMQConfig) { c.ConsumerOperationTimeout = 0 }, func(c *RabbitMQConfig) { c.VHost = "wrong" },
		func(c *RabbitMQConfig) { c.ManagementPassword = "" }, func(c *RabbitMQConfig) { c.URL = "amqp://user:SECRET_TOKEN@remote.example/%2Fichat" },
	} {
		c := base.RABBITMQ
		mutate(&c)
		settings, err := c.Consumer()
		if !errors.Is(err, ErrRabbitMQConfig) || settings != (ConsumerConfig{}) {
			t.Fatal("direct invalid consumer config bypassed validation", err)
		}
		if strings.Contains(err.Error(), "SECRET_TOKEN") {
			t.Fatal("direct validation echoed credential")
		}
	}
}

func TestMQRuntimeRoleSelectionAndNoFallback(t *testing.T) {
	runtimeConfigFile(t, validMQYAML)
	base, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode      RuntimeMode
		publisher bool
		groups    int
	}{
		{ModeLegacy, false, 0}, {ModeAPI, true, 0}, {ModeWorker, false, 2}, {ModeAll, true, 2},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			c := base
			c.RUNTIME.Mode = tc.mode
			before := c
			plan, err := c.MQRuntime()
			if err != nil || plan.Mode != tc.mode || (plan.Publisher != nil) != tc.publisher || len(plan.Consumers) != tc.groups {
				t.Fatal("MQ roles were mixed or dropped", err)
			}
			if tc.groups == 2 {
				persist, audit := plan.Consumers[0], plan.Consumers[1]
				if persist.Queue != rabbitmq.PersistQueue || persist.Settings.Workers != 4 || audit.Queue != rabbitmq.QuarantineQueue || audit.Settings.Workers != 1 ||
					persist.Settings.Prefetch != 16 || audit.Settings.Prefetch != 16 || persist.Settings.Options != rabbitmq.DefaultConnectionOptions() || audit.Settings.Options != persist.Settings.Options {
					t.Fatal("consumer groups did not reserve independent persistence/audit sessions")
				}
				plan.Consumers[0].Settings.Workers = 1
			}
			if c != before || Conf != (Config{}) {
				t.Fatal("MQ dependency planning mutated input or globals")
			}
		})
	}
	// Dependency-specific pure constructors do not validate inactive roles in a
	// directly assembled config. Load still validates every supported file field.
	worker := base
	worker.RUNTIME.Mode, worker.RABBITMQ.PublisherSlots = ModeWorker, 0
	if plan, err := worker.MQRuntime(); err != nil || plan.Publisher != nil || len(plan.Consumers) != 2 {
		t.Fatal("worker built command publisher dependency", err)
	}
	api := base
	api.RUNTIME.Mode, api.RABBITMQ.ConsumerWorkers = ModeAPI, 0
	if plan, err := api.MQRuntime(); err != nil || plan.Publisher == nil || len(plan.Consumers) != 0 {
		t.Fatal("api built consumer dependency", err)
	}
	var legacy Config
	legacy.RABBITMQ.URL = "SECRET_TOKEN: not a configured endpoint"
	if plan, err := legacy.MQRuntime(); err != nil || plan.Mode != ModeLegacy || plan.Publisher != nil || len(plan.Consumers) != 0 {
		t.Fatal("legacy interpreted unused MQ inputs", err)
	}
	for _, mode := range []RuntimeMode{ModeAPI, ModeWorker, ModeAll} {
		var missing Config
		missing.RUNTIME.Mode = mode
		plan, err := missing.MQRuntime()
		if !errors.Is(err, ErrRabbitMQNotConfigured) || plan.Publisher != nil || len(plan.Consumers) != 0 || plan.Mode != "" {
			t.Fatal("missing required MQ dependency silently fell back", err)
		}
		missing.LEGACY_WORKER.Enabled = true
		if _, err := missing.MQRuntime(); !errors.Is(err, ErrRuntimeConfig) {
			t.Fatal("mixed legacy roles bypassed MQ planning", err)
		}
	}
	partial := base
	partial.RUNTIME.Mode, partial.RABBITMQ.ConsumerWorkers = ModeAll, 0
	if plan, err := partial.MQRuntime(); !errors.Is(err, ErrRabbitMQConfig) || plan.Publisher != nil || len(plan.Consumers) != 0 || plan.Mode != "" {
		t.Fatal("partial all-mode plan escaped after consumer failure", err)
	}
}

func TestMQRuntimePureConstructionAndFormatting(t *testing.T) {
	runtimeConfigFile(t, validMQYAML)
	amqpListener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer amqpListener.Close()
	managementListener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer managementListener.Close()
	t.Setenv("ICHAT_RABBITMQ_URL", "amqp://app:secret-amqp@"+amqpListener.Addr().String()+"/%2Fichat")
	t.Setenv("ICHAT_RABBITMQ_MANAGEMENTURL", "http://"+managementListener.Addr().String())
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := c.RABBITMQ.Consumer()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []RuntimeMode{ModeAPI, ModeWorker, ModeAll} {
		c.RUNTIME.Mode = mode
		plan, err := c.MQRuntime()
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []any{consumer, &consumer, plan, &plan, plan.Consumers} {
			for _, format := range []string{"%v", "%+v", "%#v"} {
				output := fmt.Sprintf(format, value)
				if strings.Contains(output, "secret-amqp") || strings.Contains(output, "secret-management") {
					t.Fatal("dependency formatting exposed credentials")
				}
			}
		}
	}
	for _, listener := range []*net.TCPListener{amqpListener, managementListener} {
		if err := listener.SetDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		conn, err := listener.Accept()
		if conn != nil {
			conn.Close()
			t.Fatal("pure MQ planning opened a network connection")
		}
		var networkError net.Error
		if !errors.As(err, &networkError) || !networkError.Timeout() {
			t.Fatalf("unexpected listener result: %v", err)
		}
	}
}
