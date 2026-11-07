//go:build integration

package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/config"
	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/tests/clientsim"
	"iChat/workers"

	mysql "github.com/go-sql-driver/mysql"
)

func TestProductionSenderCommitLossIntegration(t *testing.T) {
	var conf config.Config
	f := newSenderResultFixtureWithAPIConfig(t, func(c *config.Config) { conf = *c })
	sqlProxy := newSenderCommitLossProxy(t, net.JoinHostPort(conf.MYSQL.Host, conf.MYSQL.Port))
	sqlConfig := mysql.NewConfig()
	sqlConfig.Net, sqlConfig.Addr = "tcp", sqlProxy.listener.Addr().String()
	sqlConfig.User, sqlConfig.Passwd, sqlConfig.DBName = conf.MYSQL.User, conf.MYSQL.Password, conf.MYSQL.DbName
	sqlConfig.ParseTime, sqlConfig.InterpolateParams, sqlConfig.Loc = true, true, time.Local
	sqlConfig.Params = map[string]string{"charset": "utf8mb4"}
	sqlConfig.Timeout, sqlConfig.ReadTimeout, sqlConfig.WriteTimeout = 3*time.Second, 30*time.Second, 5*time.Second
	connector, err := mysql.NewConnector(sqlConfig)
	if err != nil {
		t.Fatal("configure owned persistence SQL proxy")
	}
	pool := sql.OpenDB(connector)
	pool.SetMaxOpenConns(2)
	t.Cleanup(func() { pool.Close() })
	var schema string
	if err := pool.QueryRowContext(f.ctx, "SELECT DATABASE()").Scan(&schema); err != nil || schema != conf.MYSQL.DbName {
		t.Fatal("proxied SQL escaped owned schema")
	}
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
	po.Worker.ProcessingTimeout = 45 * time.Second // Includes the deliberate SQL/management observation barrier.
	po.SetupTimeout, po.BaseDelay, po.MaxDelay = 45*time.Second, 100*time.Millisecond, 500*time.Millisecond
	replacement := make(chan struct{})
	replacementRequested := make(chan struct{})
	var calls atomic.Uint64
	var requested sync.Once
	factory := source.factory(f, u.String())
	consumer, err := workers.NewPersistenceSupervisor(pool, func(ctx context.Context) (workers.PersistenceConsumerGroup, error) {
		if calls.Add(1) > 1 {
			requested.Do(func() { close(replacementRequested) })
			select {
			case <-replacement:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return factory(ctx)
	}, po)
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
	sqlProxy.armed.Store(true)
	// Release a held SQL callback before joining owners even on test failure.
	t.Cleanup(sqlProxy.discardResponse)
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
	case ok := <-sqlProxy.captured:
		if ok.connection == 0 || len(ok.packet) < 11 {
			t.Fatal("missing actual COMMIT OK")
		}
		t.Logf("withheld real COMMIT OK: connection=%d sequence=%d bytes=%d", ok.connection, ok.packet[3], len(ok.packet))
	case <-time.After(5 * time.Second):
		t.Fatal("real COMMIT OK not captured")
	}
	var original senderObservedDelivery
	select {
	case original = <-proxy.deliveries:
	case <-time.After(5 * time.Second):
		t.Fatal("original broker delivery was not observed")
	}
	trace := httpProxy.snapshot()
	if len(trace) != 1 || trace[0].method != http.MethodPost || trace[0].status != 202 {
		t.Fatal("SQL COMMIT reply loss affected actual publisher receipt")
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
	if !consumer.ConsumerReady() || consumer.Err() != nil || proxy.consumerAcks.Load() != 0 || proxy.delivered.Load() != 1 ||
		sqlProxy.commits.Load() != 1 || sqlProxy.dropped.Load() != 0 || sqlProxy.invalid.Load() {
		t.Fatal("held real COMMIT response did not retain original unacknowledged delivery", consumer.Err())
	}
	if entry := local.entry(t, f.ctx); entry.State != clientsim.Accepted || entry.Attempts != 1 || entry.Result != nil || entry.InFlight {
		t.Fatal("normal 202 did not preserve accepted state before SQL reconciliation")
	}
	events := senderCompletedEvents(t, f)
	sqlProxy.discardResponse() // Only now does the real driver receive EOF: COMMIT is unknown.
	select {
	case <-firstGroup.closed:
	case <-time.After(8 * time.Second):
		t.Fatal("unknown COMMIT did not retire original delivery owner")
	}
	select {
	case <-replacementRequested:
	case <-time.After(5 * time.Second):
		failure := consumer.Err()
		t.Fatalf("production supervisor did not request recovery: status=%+v class=%s commitUnknown=%v deadline=%v canceled=%v invalidConnection=%v cleanup=%v",
			consumer.Status(), workers.ClassifyPersistenceFailure(failure), errors.Is(failure, database.ErrCommandCommitUnknown),
			errors.Is(failure, context.DeadlineExceeded), errors.Is(failure, context.Canceled), errors.Is(failure, mysql.ErrInvalidConn),
			errors.Is(failure, workers.ErrPersistenceServiceClose) || errors.Is(failure, workers.ErrPersistenceSupervisorClose))
	}
	failure := consumer.Status()
	groups, active, overlap = source.snapshot()
	if failure.LastFailure != workers.FailureCommitUnknown || failure.Ready || consumer.Err() != nil ||
		len(groups) != 1 || active != 0 || overlap || proxy.consumerAcks.Load() != 0 || sqlProxy.dropped.Load() != 1 {
		t.Fatal("unknown COMMIT was ACKed, misclassified, or lost old ownership", failure, consumer.Err())
	}
	queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
	if err != nil || queue.Messages != 1 || queue.Consumers != 0 {
		t.Fatal("unknown COMMIT did not retain original broker responsibility", err)
	}
	policy := clientsim.DefaultRecoveryOptions()
	policy.MaxRounds, policy.MaxSubmissions = 1, 1
	if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); err != nil {
		t.Fatal("committed original terminal was not queryable", err)
	}
	trace = httpProxy.snapshot()
	if len(trace) != 2 || trace[1].method != http.MethodGet || trace[1].status != 200 {
		t.Fatal("unknown consumer COMMIT caused client resubmission instead of terminal query")
	}
	body := f.readSenderResult(t, entry.ClientMessageID)
	if query, err := local.runner.ObserveCommandResult(f.ctx, body); err != nil || query {
		t.Fatal("original real result did not reconcile", err)
	}
	terminal := local.entry(t, f.ctx)
	if terminal.State != clientsim.Persisted || terminal.Attempts != 1 || terminal.Request != request || terminal.InFlight || !reflect.DeepEqual(terminal.Result, &status) {
		t.Fatal("original committed terminal not retained before redelivery")
	}
	local.close(t)
	local = f.openPipelineSenderWithOptions(t, directory, options)
	if !reflect.DeepEqual(terminal, local.entry(t, f.ctx)) {
		t.Fatal("pre-redelivery terminal did not survive restart")
	}
	close(replacement) // The production-requested next generation may now acquire its real session.
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
		t.Fatal("redelivery consumer ACK was not forwarded to broker")
	}
	// Queue metrics may retain the pre-ACK sample while the channel is idle.
	// A successful relay write alone is also insufficient. The final proof
	// closes/joins the only consumer and requires zero requeued messages.
	readyNow, unackedNow, consumersNow, metricsOK := probe()
	t.Logf("active-channel sample after ACK forwarding: metrics=%v ready=%d unacked=%d consumers=%d", metricsOK, readyNow, unackedNow, consumersNow)
	if !consumer.ConsumerReady() || consumer.Err() != nil || proxy.consumerAcks.Load() != 1 {
		t.Fatal("redelivery consumer did not finish its only ACK", consumer.Err())
	}
	if proxy.invalid.Load() || proxy.delivered.Load() != 2 || proxy.pubs.Load() != 0 || proxy.acks.Load() != 0 || proxy.didDrop.Load() || sqlProxy.invalid.Load() ||
		sqlProxy.commits.Load() != 1 || sqlProxy.rollbacks.Load() != 1 || sqlProxy.dropped.Load() != 1 {
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
	if err := pool.PingContext(f.ctx); err != nil {
		t.Fatal("supervisor closed borrowed persistence SQL pool", err)
	}
	t.Logf("generation %d -> %d; actual COMMIT OK lost, classified commit_unknown; 1 COMMIT / 1 replay ROLLBACK / 1 consumer ACK; 1 POST / 1 GET; same publishAttemptId and byte-identical envelope redelivered=true; original SQL/Outbox/disk terminal unchanged", initialGeneration, consumer.Status().Generation)
}
