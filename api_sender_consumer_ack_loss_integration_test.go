//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/tests/clientsim"
	"iChat/workers"
)

func senderQueueProbe(t *testing.T, f *senderResultFixture) func() (int, int, int, bool) {
	t.Helper()
	return senderNamedQueueProbe(t, f, rabbitmq.PersistQueue)
}

func senderNamedQueueProbe(t *testing.T, f *senderResultFixture, queue string) func() (int, int, int, bool) {
	t.Helper()
	transport := &http.Transport{Proxy: nil, DisableCompression: true}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(transport.CloseIdleConnections)
	return func() (int, int, int, bool) {
		req, err := http.NewRequestWithContext(f.ctx, http.MethodGet, "http://"+f.managementAddress+"/api/queues/"+url.PathEscape(f.vhost)+"/"+url.PathEscape(queue), nil)
		if err != nil {
			return 0, 0, 0, false
		}
		req.SetBasicAuth("ichat_test_admin", f.client.Options().Password)
		response, err := client.Do(req)
		if err != nil {
			return 0, 0, 0, false
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		var value struct {
			Ready     *int `json:"messages_ready"`
			Unacked   *int `json:"messages_unacknowledged"`
			Consumers *int `json:"consumers"`
		}
		if err != nil || response.StatusCode != 200 || len(body) > 1<<20 || json.Unmarshal(body, &value) != nil ||
			value.Ready == nil || value.Unacked == nil || value.Consumers == nil {
			return 0, 0, 0, false
		}
		return *value.Ready, *value.Unacked, *value.Consumers, true
	}
}

func senderCompletedEvents(t *testing.T, f *senderResultFixture) map[string][]byte {
	t.Helper()
	rows, err := f.pool.QueryContext(f.ctx, "SELECT event_id,payload FROM message_outbox WHERE status='completed'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := make(map[string][]byte)
	for rows.Next() {
		var id string
		var payload []byte
		if err := rows.Scan(&id, &payload); err != nil {
			t.Fatal(err)
		}
		result[id] = payload
	}
	if err := rows.Err(); err != nil || len(result) != 2 {
		t.Fatal("original completed Outbox collection differs", len(result), err)
	}
	return result
}

func TestProductionSenderConsumerAckLossIntegration(t *testing.T) {
	f := newSenderResultFixture(t)
	u, err := url.Parse(f.address)
	if err != nil || u.Scheme != "amqp" || u.Host == "" {
		t.Fatal("invalid owned consumer URI")
	}
	proxy := newSenderConsumerAckProxy(t, u.Host)
	u.Host = proxy.listener.Addr().String()
	source := &senderConsumerSource{}
	po := workers.DefaultPersistenceSupervisorOptions()
	po.Worker.PollTimeout, po.Worker.ProbeInterval, po.Worker.ProbeTimeout = 100*time.Millisecond, time.Second, 500*time.Millisecond
	po.Worker.CloseTimeout = 3 * time.Second
	po.SetupTimeout, po.BaseDelay, po.MaxDelay = 5*time.Second, 100*time.Millisecond, 500*time.Millisecond
	consumer, err := workers.NewPersistenceSupervisor(f.pool, source.factory(f, u.String()), po)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := consumer.Close(c); err != nil {
			t.Error(err)
		}
		apiWait(t, consumer.Done())
	})
	setup, cancelSetup := context.WithTimeout(f.ctx, 8*time.Second)
	err = consumer.Start(setup, f.ctx)
	cancelSetup()
	if err != nil {
		t.Fatal(err)
	}
	apiRealEventually(t, consumer.ConsumerReady)
	initialGeneration := consumer.Status().Generation
	groups, active, overlap := source.snapshot()
	if len(groups) != 1 || active != 1 || overlap {
		t.Fatal("invalid initial consumer ownership")
	}
	firstGroup := groups[0]
	outboxRepo, err := database.NewOutboxRepository(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	store := &senderOutboxStore{OutboxRepository: outboxRepo, claim: make(chan struct{}, 1)}
	publisher, err := workers.NewRedisNotificationPublisher(f.client)
	if err != nil {
		t.Fatal(err)
	}
	oo := workers.DefaultOutboxServiceOptions()
	oo.Loop.PollInterval = 20 * time.Millisecond
	outbox, err := workers.NewOutboxService(func(context.Context) (workers.OutboxResources, error) {
		return workers.OutboxResources{Store: store, Publisher: publisher, ProbeStorage: f.pool.PingContext,
			ProbeNotification: func(c context.Context) error { return f.client.Ping(c).Err() }}, nil
	}, oo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := drainSenderOutbox(c, outbox, store); err != nil {
			t.Error(err)
		}
		if err := outbox.Close(c); err != nil {
			t.Error(err)
		}
	})
	setup, cancelSetup = context.WithTimeout(f.ctx, 5*time.Second)
	err = outbox.Start(setup, f.ctx)
	cancelSetup()
	if err != nil {
		t.Fatal(err)
	}
	proxy.armed.Store(true)
	httpProxy := newSenderRecoveryProxyWithOptions(t, "http://"+apiAddress(f.api), senderRecoveryProxyOptions{})
	options, directory := senderGuardOptions(f, httpProxy), t.TempDir()
	local := f.openPipelineSenderWithOptions(t, directory, options)
	request := senderGuardRequest(f)
	entry, err := local.sender.Queue(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	command, err := messaging.Normalize(uint64(f.users[0].ID), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.runner.Submit(f.ctx, entry.ClientMessageID); err != nil {
		t.Fatal(err)
	}
	select {
	case ack := <-proxy.dropped:
		if ack.connection == 0 || ack.channel == 0 || ack.tag == 0 || ack.multiple {
			t.Fatal("invalid actual single-delivery consumer ACK")
		}
		t.Logf("dropped real consumer Basic.Ack: connection=%d channel=%d deliveryTag=%d multiple=%v", ack.connection, ack.channel, ack.tag, ack.multiple)
	case <-time.After(5 * time.Second):
		t.Fatal("production commit did not emit consumer ACK")
	}
	var original senderObservedDelivery
	select {
	case original = <-proxy.deliveries:
	case <-time.After(5 * time.Second):
		t.Fatal("original broker delivery was not observed")
	}
	trace := httpProxy.snapshot()
	if len(trace) != 1 || trace[0].status != 202 {
		t.Fatal("consumer ACK loss affected actual publisher receipt")
	}
	accepted, err := messaging.DecodeSubmissionResponse(trace[0].response)
	envelope, decodeErr := messaging.DecodeEnvelope(original.body)
	decoded, validateErr := messaging.ValidateEnvelope(envelope)
	if err != nil || decodeErr != nil || validateErr != nil || decoded != command || original.redelivered || original.tag == 0 ||
		envelope.PublishAttemptID != accepted.Data.PublishAttemptID || !accepted.Data.Matches(command) {
		t.Fatal("observed original delivery differs from actual publish attempt")
	}
	status := awaitSenderGuardTerminal(t, f, entry)
	probe := senderQueueProbe(t, f)
	apiRealEventually(t, func() bool {
		ready, unacked, consumers, ok := probe()
		return ok && ready == 0 && unacked == 1 && consumers == 1
	})
	if !consumer.ConsumerReady() || consumer.Err() != nil || proxy.consumerAcks.Load() != 1 || proxy.delivered.Load() != 1 {
		t.Fatal("lost ACK did not leave one live, unacknowledged delivery", consumer.Err())
	}
	policy := clientsim.DefaultRecoveryOptions()
	policy.MaxRounds, policy.MaxSubmissions = 1, 1
	if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); err != nil {
		t.Fatal("committed original terminal was not queryable", err)
	}
	body := f.readSenderResult(t, entry.ClientMessageID)
	if query, err := local.runner.ObserveCommandResult(f.ctx, body); err != nil || query {
		t.Fatal("original real result did not reconcile", err)
	}
	terminal := local.entry(t, f.ctx)
	if terminal.State != clientsim.Persisted || terminal.Attempts != 1 || terminal.Request != request || terminal.InFlight || !reflect.DeepEqual(terminal.Result, &status) {
		t.Fatal("original committed terminal not retained before redelivery")
	}
	events := senderCompletedEvents(t, f)
	local.close(t)
	local = f.openPipelineSenderWithOptions(t, directory, options)
	if !reflect.DeepEqual(terminal, local.entry(t, f.ctx)) {
		t.Fatal("pre-redelivery terminal did not survive restart")
	}
	proxy.disconnect() // ACK loss is not detectable from a successful TCP write alone.
	select {
	case <-firstGroup.closed:
	case <-time.After(8 * time.Second):
		t.Fatal("old owner did not join after connection interruption")
	}
	apiRealEventually(t, func() bool { return consumer.ConsumerReady() || consumer.Err() != nil })
	if !consumer.ConsumerReady() || consumer.Err() != nil || consumer.Status().Generation <= initialGeneration {
		t.Fatal("supervisor did not replace failed delivery connection", consumer.Err())
	}
	var replay senderObservedDelivery
	select {
	case replay = <-proxy.deliveries:
	case <-time.After(5 * time.Second):
		t.Fatal("broker did not redeliver unacknowledged original")
	}
	if !replay.redelivered || replay.connection == original.connection || replay.tag == 0 || !bytes.Equal(replay.body, original.body) {
		t.Fatal("redelivery changed original envelope/attempt or reused old connection")
	}
	select {
	case ack := <-proxy.forwardedConsumerAcks:
		t.Logf("forwarded consumer ACK: connection=%d channel=%d tag=%d multiple=%v; replay connection=%d channel=%d tag=%d",
			ack.connection, ack.channel, ack.tag, ack.multiple, replay.connection, replay.channel, replay.tag)
		if ack.connection != replay.connection || ack.channel != replay.channel || ack.tag != replay.tag || ack.multiple {
			t.Fatal("forwarded ACK does not match actual redelivery")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second consumer ACK was not forwarded to broker")
	}
	// Queue metrics may retain the pre-ACK sample while the channel is idle.
	// A successful relay write alone is also insufficient. The final proof
	// closes/joins the only consumer and requires zero requeued messages.
	readyNow, unackedNow, consumersNow, metricsOK := probe()
	t.Logf("active-channel sample after ACK forwarding: metrics=%v ready=%d unacked=%d consumers=%d", metricsOK, readyNow, unackedNow, consumersNow)
	if !consumer.ConsumerReady() || consumer.Err() != nil || proxy.consumerAcks.Load() != 2 {
		t.Fatal("redelivery consumer did not finish its second ACK", consumer.Err())
	}
	if proxy.invalid.Load() || proxy.delivered.Load() != 2 || proxy.pubs.Load() != 0 || proxy.acks.Load() != 0 || !proxy.didDrop.Load() {
		t.Fatal("unexpected frame direction, extra delivery or invalid capture")
	}
	if !reflect.DeepEqual(events, senderCompletedEvents(t, f)) {
		t.Fatal("duplicate consumer execution changed original Outbox events")
	}
	if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); err != nil || len(httpProxy.snapshot()) != 2 || !reflect.DeepEqual(terminal, local.entry(t, f.ctx)) {
		t.Fatal("broker redelivery resumed client HTTP or changed terminal", err)
	}
	local.close(t)
	local = f.openPipelineSenderWithOptions(t, directory, options)
	if !reflect.DeepEqual(terminal, local.entry(t, f.ctx)) {
		t.Fatal("post-redelivery terminal changed on disk restart")
	}
	stop, cancelStop := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancelStop()
	if err := consumer.Drain(stop); err != nil {
		t.Fatal(err)
	}
	if err := consumer.Close(stop); err != nil {
		t.Fatal(err)
	}
	apiWait(t, consumer.Done())
	groups, active, overlap = source.snapshot()
	if len(groups) < 2 || active != 0 || overlap {
		t.Fatal("consumer generations overlapped or retained ownership")
	}
	for _, group := range groups {
		select {
		case <-group.closed:
		default:
			t.Fatal("consumer group still owns resources")
		}
	}
	if err := drainSenderOutbox(stop, outbox, store); err != nil {
		t.Fatal(err)
	}
	assertSenderGuardCollections(t, f, status)
	deadline := time.Now().Add(20 * time.Second)
	for {
		ready, unacked, consumers, ok := probe()
		if ok && ready == 0 && unacked == 0 && consumers == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("post-join broker counts did not clear: metrics=%v ready=%d unacked=%d consumers=%d", ok, ready, unacked, consumers)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Logf("generation %d -> %d; 1 POST / 1 GET; same publishAttemptId and byte-identical envelope redelivered=true; 2 consumer ACKs, exactly 1 dropped; broker unacked 1 -> 0; original SQL/Outbox/disk terminal unchanged", initialGeneration, consumer.Status().Generation)
}
