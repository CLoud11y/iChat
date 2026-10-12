//go:build integration

package testenv

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"iChat/messaging/rabbitmq"
)

func TestRabbitMQTopologyIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	env, conf := startRabbitFixture(t, ctx)
	m, err := rabbitmq.NewManagement("http://"+conf.ManagementAddress, rabbitAdminUser, env.password)
	if err != nil {
		t.Fatal(err)
	}
	conn := openRabbitProbe(t, conf, nil)
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := rabbitmq.EnsureLocal(ctx, ch, m, conf.VHost); err != nil {
			t.Fatal(err)
		}
	}
	returns := ch.NotifyReturn(make(chan amqp.Return, 1))
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	if err := ch.Confirm(false); err != nil {
		t.Fatal(err)
	}
	publish := func(id string) []byte {
		t.Helper()
		body := []byte("topology:" + env.RunID() + ":" + id + ":中🙂\x00")
		tag := ch.GetNextPublishSeqNo()
		if err := ch.PublishWithContext(ctx, rabbitmq.CommandsExchange, rabbitmq.SubmitRoutingKey, true, false, amqp.Publishing{DeliveryMode: amqp.Persistent, MessageId: id, CorrelationId: "attempt:" + id, ContentType: "application/octet-stream", Body: body}); err != nil {
			t.Fatal(err)
		}
		waitRabbitConfirm(t, ctx, confirms, tag)
		select {
		case <-returns:
			t.Fatal("formal command was unroutable")
		default:
		}
		return body
	}
	t.Run("fixed_topology_and_reliable_rejection", func(t *testing.T) {
		body := publish("rejected")
		d := getTopologyDelivery(t, ctx, ch, rabbitmq.PersistQueue)
		assertTopologyContent(t, d, "rejected", body)
		if err := d.Reject(false); err != nil {
			t.Fatal(err)
		}
		dead := getTopologyDelivery(t, ctx, ch, rabbitmq.QuarantineQueue)
		assertTopologyDeadLetter(t, dead, "rejected", "rejected", body)
		if err := dead.Ack(false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("delivery_limit_uses_reject_and_quarantine_has_no_limit", func(t *testing.T) {
		body := publish("delivery-limit")
		// In 4.3 basic.nack requeue does not increment delivery-count.
		// Basic.reject requeue does, so the 51st failure exceeds limit 50.
		for i := 0; i <= rabbitmq.DeliveryLimit; i++ {
			d := getTopologyDelivery(t, ctx, ch, rabbitmq.PersistQueue)
			assertTopologyContent(t, d, "delivery-limit", body)
			if i > 0 && fmt.Sprint(d.Headers["x-delivery-count"]) != fmt.Sprint(i) {
				t.Fatalf("delivery count=%v want=%d", d.Headers["x-delivery-count"], i)
			}
			if err := d.Reject(true); err != nil {
				t.Fatal(err)
			}
		}
		dead := getTopologyDelivery(t, ctx, ch, rabbitmq.QuarantineQueue)
		assertTopologyDeadLetter(t, dead, "delivery-limit", "delivery_limit", body)
		for i := 0; i < rabbitmq.DeliveryLimit+5; i++ {
			if err := dead.Reject(true); err != nil {
				t.Fatal(err)
			}
			dead = getTopologyDelivery(t, ctx, ch, rabbitmq.QuarantineQueue)
			assertTopologyContent(t, dead, "delivery-limit", body)
		}
		if err := dead.Ack(false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unroutable_dlx_retains_source_and_recovers", func(t *testing.T) {
		if err := ch.QueueUnbind(rabbitmq.QuarantineQueue, rabbitmq.QuarantineRoutingKey, rabbitmq.QuarantineExchange, nil); err != nil {
			t.Fatal(err)
		}
		if err := rabbitmq.VerifyLocal(ctx, m, conf.VHost); err == nil {
			t.Fatal("missing quarantine binding was ready")
		}
		body := publish("dlx-outage")
		d := getTopologyDelivery(t, ctx, ch, rabbitmq.PersistQueue)
		assertTopologyContent(t, d, "dlx-outage", body)
		if err := d.Reject(false); err != nil {
			t.Fatal(err)
		}
		// Management collects counters asynchronously. A live dead letter is
		// counted by the source until its target confirms the transfer.
		waitTopologyMessages(t, ctx, env, rabbitmq.PersistQueue, 1)
		for i := 0; i < 3; i++ {
			if _, ok, err := ch.Get(rabbitmq.QuarantineQueue, false); err != nil || ok {
				t.Fatalf("unbound DLX delivered or errored: ok=%v err=%v", ok, err)
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(200 * time.Millisecond):
			}
		}
		if err := ch.QueueBind(rabbitmq.QuarantineQueue, rabbitmq.QuarantineRoutingKey, rabbitmq.QuarantineExchange, false, nil); err != nil {
			t.Fatal(err)
		}
		if err := rabbitmq.VerifyLocal(ctx, m, conf.VHost); err != nil {
			t.Fatal(err)
		}
		// v4.3.6's unchanged broker default retries every 180 seconds.
		// Keep that real timing and allow one interval plus scheduling margin.
		dead := getTopologyDeliveryWithin(t, ctx, ch, rabbitmq.QuarantineQueue, 4*time.Minute)
		assertTopologyDeadLetter(t, dead, "dlx-outage", "rejected", body)
		if err := dead.Ack(false); err != nil {
			t.Fatal(err)
		}
		waitTopologyMessages(t, ctx, env, rabbitmq.PersistQueue, 0)
	})
	t.Run("operator_policy_fails_without_mutating_data", func(t *testing.T) {
		body := publish("policy-sentinel")
		path := "/api/operator-policies/" + url.PathEscape(conf.VHost) + "/ichat.test.override"
		if _, err := env.rabbitAPI(ctx, http.MethodPut, path, map[string]any{"pattern": `^ichat\.message\.persist\.v1$`, "apply-to": "quorum_queues", "priority": 100, "definition": map[string]string{"overflow": "drop-head"}}, nil); err != nil {
			t.Fatal(err)
		}
		if err := rabbitmq.EnsureLocal(ctx, ch, m, conf.VHost); err == nil {
			t.Fatal("operator override accepted")
		}
		// Remove only the explicitly-created policy in this owned fixture.
		if _, err := env.rabbitAPI(ctx, http.MethodDelete, path, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := rabbitmq.VerifyLocal(ctx, m, conf.VHost); err != nil {
			t.Fatal(err)
		}
		d := getTopologyDelivery(t, ctx, ch, rabbitmq.PersistQueue)
		assertTopologyContent(t, d, "policy-sentinel", body)
		if err := d.Ack(false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("existing_classic_queue_is_not_deleted_or_replaced", func(t *testing.T) {
		vhost := "ichat_topology_" + env.RunID()
		if _, err := env.rabbitAPI(ctx, http.MethodPut, "/api/vhosts/"+vhost, map[string]any{}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := env.rabbitAPI(ctx, http.MethodPut, "/api/permissions/"+vhost+"/"+rabbitAdminUser, map[string]string{"configure": ".*", "write": ".*", "read": ".*"}, nil); err != nil {
			t.Fatal(err)
		}
		adminConf := conf
		adminConf.VHost, adminConf.Username, adminConf.Password = vhost, rabbitAdminUser, env.password
		other := openRabbitProbe(t, adminConf, nil)
		bad, err := other.Channel()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bad.QueueDeclare(rabbitmq.PersistQueue, true, false, false, false, amqp.Table{"x-queue-type": "classic"}); err != nil {
			t.Fatal(err)
		}
		otherConfirms := bad.NotifyPublish(make(chan amqp.Confirmation, 1))
		if err := bad.Confirm(false); err != nil {
			t.Fatal(err)
		}
		body := []byte("preserve-classic:" + env.RunID())
		if err := bad.PublishWithContext(ctx, "", rabbitmq.PersistQueue, true, false, amqp.Publishing{DeliveryMode: amqp.Persistent, MessageId: "classic", Body: body}); err != nil {
			t.Fatal(err)
		}
		waitRabbitConfirm(t, ctx, otherConfirms, 1)
		if err := rabbitmq.EnsureLocal(ctx, bad, m, vhost); err == nil {
			t.Fatal("classic conflict accepted")
		}
		d := getTopologyDelivery(t, ctx, bad, rabbitmq.PersistQueue)
		if d.MessageId != "classic" || !bytes.Equal(d.Body, body) {
			t.Fatal("failed initialization changed existing queue content")
		}
		if err := d.Ack(false); err != nil {
			t.Fatal(err)
		}
		var queue struct {
			Type string `json:"type"`
		}
		if _, err := env.rabbitAPI(ctx, http.MethodGet, "/api/queues/"+vhost+"/"+rabbitmq.PersistQueue, nil, &queue); err != nil || queue.Type != "classic" {
			t.Fatal("existing queue replaced", err)
		}
		var target any
		if status, err := env.rabbitAPI(ctx, http.MethodGet, "/api/queues/"+vhost+"/"+rabbitmq.QuarantineQueue, nil, &target); err != nil || status != 404 {
			t.Fatal("preflight created partial topology", err)
		}
	})
	t.Run("local_admin_command_initializes_and_checks", func(t *testing.T) {
		if _, err := env.rabbitAPI(ctx, http.MethodPut, "/api/vhosts/"+url.PathEscape(rabbitmq.LocalVHost), map[string]any{}, nil); err != nil {
			t.Fatal(err)
		}
		// This is the local Compose bootstrap account, inside the disposable
		// fixture broker. No development .env or persistent volume is used.
		if _, err := env.rabbitAPI(ctx, http.MethodPut, "/api/users/ichat_admin", map[string]any{"password": env.password, "tags": []string{"administrator"}}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := env.rabbitAPI(ctx, http.MethodPut, "/api/permissions/"+url.PathEscape(rabbitmq.LocalVHost)+"/ichat_admin", map[string]string{"configure": ".*", "write": ".*", "read": ".*"}, nil); err != nil {
			t.Fatal(err)
		}
		appPassword, err := randomID()
		if err != nil {
			t.Fatal(err)
		}
		_, amqpPort, err := net.SplitHostPort(conf.Address)
		if err != nil {
			t.Fatal(err)
		}
		_, managementPort, err := net.SplitHostPort(conf.ManagementAddress)
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(t.TempDir(), "mq.env")
		content := fmt.Sprintf("RABBITMQ_ADMIN_PASSWORD=%s\nRABBITMQ_APP_PASSWORD=%s\nRABBITMQ_PORT=%s\nRABBITMQ_MANAGEMENT_PORT=%s\n", env.password, appPassword, amqpPort, managementPort)
		if err := os.WriteFile(file, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		for _, action := range []string{"init", "init", "check"} {
			command := exec.CommandContext(ctx, "go", "run", "-mod=readonly", "./cmd/mqadmin", action, "--env-file", file)
			command.Dir = filepath.Join("..", "..")
			for _, variable := range os.Environ() {
				if !strings.HasPrefix(strings.ToUpper(variable), "RABBITMQ_") {
					command.Env = append(command.Env, variable)
				}
			}
			result, err := command.CombinedOutput()
			if bytes.Contains(result, []byte(env.password)) || bytes.Contains(result, []byte(appPassword)) {
				t.Fatal("mqadmin leaked credentials")
			}
			if err != nil {
				t.Fatalf("mqadmin %s failed: %v %s", action, err, result)
			}
			t.Logf("mqadmin %s: %s", action, strings.TrimSpace(string(result)))
		}
		if err := rabbitmq.VerifyLocal(ctx, m, rabbitmq.LocalVHost); err != nil {
			t.Fatal(err)
		}
		appConf := conf
		appConf.VHost, appConf.Username, appConf.Password = rabbitmq.LocalVHost, rabbitmq.LocalAppUser, appPassword
		local := openRabbitProbe(t, appConf, nil)
		localChannel, err := local.Channel()
		if err != nil {
			t.Fatal(err)
		}
		if err := rabbitmq.EnsureLocal(ctx, localChannel, m, rabbitmq.LocalVHost); err != nil {
			t.Fatal(err)
		}
	})
}

func getTopologyDelivery(t *testing.T, ctx context.Context, ch *amqp.Channel, queue string) amqp.Delivery {
	t.Helper()
	return getTopologyDeliveryWithin(t, ctx, ch, queue, 20*time.Second)
}

func getTopologyDeliveryWithin(t *testing.T, ctx context.Context, ch *amqp.Channel, queue string, timeout time.Duration) amqp.Delivery {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		d, ok, err := ch.Get(queue, false)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			return d
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-deadline.C:
			t.Fatalf("delivery timed out: %s", queue)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func assertTopologyContent(t *testing.T, d amqp.Delivery, id string, body []byte) {
	t.Helper()
	if d.MessageId != id || d.CorrelationId != "attempt:"+id || d.DeliveryMode != amqp.Persistent || !bytes.Equal(d.Body, body) {
		t.Fatal("message identity, persistence or raw body changed")
	}
}

func assertTopologyDeadLetter(t *testing.T, d amqp.Delivery, id, reason string, body []byte) {
	t.Helper()
	assertTopologyContent(t, d, id, body)
	deaths, ok := d.Headers["x-death"].([]any)
	if !ok || len(deaths) != 1 {
		t.Fatalf("missing dead letter audit header: %v", d.Headers)
	}
	death, ok := deaths[0].(amqp.Table)
	if !ok || death["queue"] != rabbitmq.PersistQueue || death["reason"] != reason {
		t.Fatalf("incorrect dead letter origin: %v", death)
	}
}

func waitTopologyMessages(t *testing.T, ctx context.Context, env *Environment, queue string, want int) {
	t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for {
		var state struct {
			Messages int `json:"messages"`
			Ready    int `json:"messages_ready"`
			Unacked  int `json:"messages_unacknowledged"`
		}
		if _, err := env.rabbitAPI(ctx, http.MethodGet, "/api/queues/"+url.PathEscape(env.rabbitConfig.VHost)+"/"+queue, nil, &state); err != nil {
			t.Fatal(err)
		}
		if state.Messages == want {
			t.Logf("%s source messages=%d ready=%d unacked=%d", queue, state.Messages, state.Ready, state.Unacked)
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-deadline.C:
			t.Fatalf("source responsibility count=%+v want=%d", state, want)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
