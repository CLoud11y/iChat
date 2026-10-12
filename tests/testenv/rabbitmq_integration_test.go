//go:build integration

package testenv

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const rabbitProbeExchange = "ichat.test.probe.exchange"
const rabbitProbeQueue = "ichat.test.probe.queue"

func TestRabbitMQEnvironmentIntegration(t *testing.T) {
	t.Setenv("COMPOSE_PROFILES", "development")
	t.Setenv("COMPOSE_PROJECT_NAME", "ichat-development")
	t.Setenv("ICHAT_TEST_RUN_ID", "development")
	t.Setenv("ICHAT_TEST_PASSWORD", "must-not-use")
	t.Setenv("ICHAT_CONFIG_FILE", filepath.Join(t.TempDir(), "must-not-read.yml"))
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	first, firstConf := startRabbitFixture(t, ctx)
	second, secondConf := startRabbitFixture(t, ctx)
	if first.RunID() == second.RunID() || firstConf.VHost == secondConf.VHost || firstConf.Address == secondConf.Address || firstConf.Username == secondConf.Username || firstConf.Password == secondConf.Password {
		t.Fatal("RabbitMQ fixtures shared an identity or endpoint")
	}
	firstConn := openRabbitProbe(t, firstConf, nil)
	secondConn := openRabbitProbe(t, secondConf, nil)
	firstChannel := declareRabbitProbe(t, firstConn)
	secondChannel := declareRabbitProbe(t, secondConn)
	firstBody := []byte("first:" + first.RunID() + ":中🙂\x00")
	secondBody := []byte("second:" + second.RunID() + ":中🙂\x00")
	publishRabbitProbe(t, ctx, firstChannel, firstBody)
	publishRabbitProbe(t, ctx, secondChannel, secondBody)

	t.Run("broker_version_and_single_member_quorum", func(t *testing.T) {
		for _, env := range []*Environment{first, second} {
			if err := env.verifyRabbitMQ(ctx); err != nil {
				t.Fatal(err)
			}
			var queue struct {
				Type       string   `json:"type"`
				Durable    bool     `json:"durable"`
				AutoDelete bool     `json:"auto_delete"`
				Members    []string `json:"members"`
			}
			if _, err := env.rabbitAPI(ctx, http.MethodGet, "/api/queues/"+url.PathEscape(env.rabbitConfig.VHost)+"/"+rabbitProbeQueue, nil, &queue); err != nil {
				t.Fatal(err)
			}
			if queue.Type != "quorum" || !queue.Durable || queue.AutoDelete || len(queue.Members) != 1 {
				t.Fatalf("probe queue properties=%+v", queue)
			}
		}
	})

	t.Run("vhost_and_resource_permissions_are_enforced", func(t *testing.T) {
		if _, err := first.rabbitAPI(ctx, http.MethodPut, "/api/vhosts/"+url.PathEscape("/ichat"), map[string]string{}, nil); err != nil {
			t.Fatal(err)
		}
		for _, vhost := range []string{"/ichat", "/ichat_bootstrap_" + first.RunID()} {
			foreign := firstConf
			foreign.VHost = vhost
			connection, err := amqp.DialConfig(foreign.AMQPURL(), amqp.Config{Recovery: nil, Dial: amqp.DefaultDial(3 * time.Second)})
			if connection != nil {
				connection.CloseDeadline(time.Now().Add(time.Second))
			}
			if !errors.Is(err, amqp.ErrVhost) {
				t.Fatalf("app could access foreign vhost %q, err=%v", vhost, err)
			}
		}
		foreignCredentials := firstConf
		foreignCredentials.Address = secondConf.Address
		foreignCredentials.VHost = secondConf.VHost
		connection, err := amqp.DialConfig(foreignCredentials.AMQPURL(), amqp.Config{Recovery: nil, Dial: amqp.DefaultDial(3 * time.Second)})
		if connection != nil {
			connection.CloseDeadline(time.Now().Add(time.Second))
		}
		if !errors.Is(err, amqp.ErrCredentials) {
			t.Fatalf("cross-run credentials accepted: %v", err)
		}
		channel, err := firstConn.Channel()
		if err != nil {
			t.Fatal(err)
		}
		_, err = channel.QueueDeclare("foreign.queue", true, false, false, false, amqp.Table{"x-queue-type": "classic"})
		var protocolErr *amqp.Error
		if !errors.As(err, &protocolErr) || protocolErr.Code != amqp.AccessRefused {
			t.Fatalf("out-of-namespace resource allowed: %v", err)
		}
		channel.Close()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+firstConf.ManagementAddress+"/api/overview", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.SetBasicAuth(firstConf.Username, firstConf.Password)
		client := http.Client{Timeout: 3 * time.Second}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 401 && response.StatusCode != 403 {
			t.Fatalf("application user has management access: HTTP %d", response.StatusCode)
		}
	})

	t.Run("buffered_return_is_present_when_ack_is_read_first", func(t *testing.T) {
		channel, err := firstConn.Channel()
		if err != nil {
			t.Fatal(err)
		}
		defer channel.Close()
		returns := channel.NotifyReturn(make(chan amqp.Return, 1))
		confirms := channel.NotifyPublish(make(chan amqp.Confirmation, 1))
		if err := channel.Confirm(false); err != nil {
			t.Fatal(err)
		}
		message := amqp.Publishing{ContentType: "application/octet-stream", DeliveryMode: amqp.Persistent, MessageId: "unroutable:" + first.RunID(), CorrelationId: "attempt:" + first.RunID(), Body: []byte("must-return")}
		if err := channel.PublishWithContext(ctx, rabbitProbeExchange, "missing", true, false, message); err != nil {
			t.Fatal(err)
		}
		// Deliberately read ACK before Return, testing the fixed client's
		// serial dispatch prerequisite, not a production publisher decision.
		waitRabbitConfirm(t, ctx, confirms, 1)
		select {
		case returned, ok := <-returns:
			if !ok || returned.ReplyCode != 312 || returned.MessageId != message.MessageId || returned.CorrelationId != message.CorrelationId || !bytes.Equal(returned.Body, message.Body) {
				t.Fatal("return lost its identity/content")
			}
		default:
			t.Fatal("ACK became visible before the buffered return")
		}
		before := channel.GetNextPublishSeqNo()
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := channel.PublishWithContext(canceled, rabbitProbeExchange, "bound", true, false, message); !errors.Is(err, context.Canceled) || channel.GetNextPublishSeqNo() != before {
			t.Fatalf("pre-cancelled publish wrote bytes/consumed tag: %v", err)
		}
	})

	t.Run("caller_sasl_credentials_can_be_reused_without_recovery", func(t *testing.T) {
		auth := &amqp.PlainAuth{Username: firstConf.Username, Password: firstConf.Password}
		for i := 0; i < 2; i++ {
			conn := openRabbitProbe(t, firstConf, auth)
			if auth.Username != firstConf.Username || auth.Password != firstConf.Password {
				t.Fatal("handshake changed caller-provided SASL credentials")
			}
			if err := conn.CloseDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("same_names_keep_independent_content_and_cleanup", func(t *testing.T) {
		readRabbitProbe(t, firstChannel, firstBody)
		// Stop clients before reclaiming the first fixture, leaving the
		// second fixture's same-named queue and pending message untouched.
		if err := firstConn.CloseDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		cleanupCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if err := first.Close(cleanupCtx); err != nil {
			t.Fatal(err)
		}
		if _, err := first.RabbitMQ(); err == nil {
			t.Fatal("closed MQ fixture exposed config")
		}
		remaining, err := first.resources(cleanupCtx)
		if err != nil || len(remaining) != 0 {
			t.Fatalf("first fixture not fully cleaned: resources=%v err=%v", remaining, err)
		}
		readRabbitProbe(t, secondChannel, secondBody)
		if err := second.verifyRabbitMQ(ctx); err != nil {
			t.Fatal("first cleanup damaged second fixture", err)
		}
	})
}

func startRabbitFixture(t *testing.T, ctx context.Context) (*Environment, RabbitMQConfig) {
	t.Helper()
	env, err := StartWithRabbitMQ(ctx, filepath.Join("..", "..", "docker-compose.test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("RabbitMQ fixture run=%s broker=%s client=%s", env.RunID(), RabbitMQVersion, AMQPClientVersion)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := env.Close(cleanupCtx); err != nil {
			t.Error(err)
		}
	})
	conf, err := env.RabbitMQ()
	if err != nil {
		t.Fatal(err)
	}
	if conf.Password == "must-not-use" || conf.VHost != "ichat_test_"+env.RunID() {
		t.Fatal("inherited environment overrode MQ fixture")
	}
	return env, conf
}

func openRabbitProbe(t *testing.T, conf RabbitMQConfig, auth *amqp.PlainAuth) *amqp.Connection {
	t.Helper()
	options := amqp.Config{Recovery: nil, Heartbeat: 2 * time.Second, Dial: amqp.DefaultDial(5 * time.Second)}
	if auth != nil {
		options.SASL = []amqp.Authentication{auth}
	}
	connection, err := amqp.DialConfig(conf.AMQPURL(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := connection.CloseDeadline(time.Now().Add(3 * time.Second)); err != nil && !errors.Is(err, amqp.ErrClosed) {
			t.Error(err)
		}
	})
	if connection.Config.Recovery != nil || connection.IsRecoveryEnabled() || connection.IsTopologyRecoveryEnabled() {
		t.Fatal("experimental automatic recovery enabled")
	}
	return connection
}

func declareRabbitProbe(t *testing.T, connection *amqp.Connection) *amqp.Channel {
	t.Helper()
	channel, err := connection.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := channel.ExchangeDeclare(rabbitProbeExchange, "direct", true, false, false, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := channel.QueueDeclare(rabbitProbeQueue, true, false, false, false, amqp.Table{"x-queue-type": "quorum", "x-quorum-initial-group-size": 1}); err != nil {
		t.Fatal(err)
	}
	if err := channel.QueueBind(rabbitProbeQueue, "bound", rabbitProbeExchange, false, nil); err != nil {
		t.Fatal(err)
	}
	return channel
}

func publishRabbitProbe(t *testing.T, ctx context.Context, channel *amqp.Channel, body []byte) {
	t.Helper()
	confirms := channel.NotifyPublish(make(chan amqp.Confirmation, 1))
	returns := channel.NotifyReturn(make(chan amqp.Return, 1))
	if err := channel.Confirm(false); err != nil {
		t.Fatal(err)
	}
	if err := channel.PublishWithContext(ctx, rabbitProbeExchange, "bound", true, false, amqp.Publishing{DeliveryMode: amqp.Persistent, Body: body}); err != nil {
		t.Fatal(err)
	}
	waitRabbitConfirm(t, ctx, confirms, 1)
	select {
	case <-returns:
		t.Fatal("bound probe returned")
	default:
	}
}

func waitRabbitConfirm(t *testing.T, ctx context.Context, confirms <-chan amqp.Confirmation, tag uint64) {
	t.Helper()
	confirmCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case confirm, ok := <-confirms:
		if !ok || !confirm.Ack || confirm.DeliveryTag != tag {
			t.Fatalf("unexpected confirm=%+v open=%t", confirm, ok)
		}
	case <-confirmCtx.Done():
		t.Fatal(confirmCtx.Err())
	}
}

func readRabbitProbe(t *testing.T, channel *amqp.Channel, want []byte) {
	t.Helper()
	message, ok, err := channel.Get(rabbitProbeQueue, false)
	if err != nil || !ok || !bytes.Equal(message.Body, want) || message.DeliveryMode != amqp.Persistent {
		t.Fatalf("isolated queue content mismatch: delivered=%t err=%v", ok, err)
	}
	if err := message.Ack(false); err != nil {
		t.Fatal(err)
	}
	queue, err := channel.QueueInspect(rabbitProbeQueue)
	if err != nil || queue.Messages != 0 {
		t.Fatalf("probe queue not empty: count=%d err=%v", queue.Messages, err)
	}
}
