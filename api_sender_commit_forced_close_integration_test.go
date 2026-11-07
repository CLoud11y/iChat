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

func TestProductionSenderCommitForcedCloseIntegration(t *testing.T) {
	var conf config.Config
	f := newSenderResultFixtureWithAPIConfig(t, func(c *config.Config) { conf = *c })
	gate := newSenderCommitCallbackGate()
	sqlConfig := mysql.NewConfig()
	sqlConfig.Net, sqlConfig.Addr = "tcp", net.JoinHostPort(conf.MYSQL.Host, conf.MYSQL.Port)
	sqlConfig.User, sqlConfig.Passwd, sqlConfig.DBName = conf.MYSQL.User, conf.MYSQL.Password, conf.MYSQL.DbName
	sqlConfig.ParseTime, sqlConfig.InterpolateParams, sqlConfig.Loc = true, true, time.Local
	sqlConfig.Params = map[string]string{"charset": "utf8mb4"}
	sqlConfig.Timeout, sqlConfig.ReadTimeout, sqlConfig.WriteTimeout = 3*time.Second, 30*time.Second, 5*time.Second
	connector, err := mysql.NewConnector(sqlConfig)
	if err != nil {
		t.Fatal("configure owned persistence callback pool")
	}
	pool := sql.OpenDB(senderCommitCallbackConnector{base: connector, gate: gate})
	pool.SetMaxOpenConns(2)
	t.Cleanup(func() { pool.Close() })
	var schema string
	if err := pool.QueryRowContext(f.ctx, "SELECT DATABASE()").Scan(&schema); err != nil || schema != conf.MYSQL.DbName {
		t.Fatal("callback pool escaped owned schema")
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
	consumer, err := workers.NewPersistenceSupervisor(pool, source.factory(f, u.String()), po)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := consumer.Close(c); err != nil && !errors.Is(err, context.Canceled) {
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
	gate.armed.Store(true)
	// Release a held SQL callback before joining owners even on test failure.
	t.Cleanup(gate.unblock)
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
	var operation context.Context
	select {
	case operation = <-gate.entered:
		if operation.Err() != nil {
			t.Fatal("actual successful COMMIT callback entered already canceled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("actual successful MySQL COMMIT did not reach callback barrier")
	}
	var original senderObservedDelivery
	select {
	case original = <-proxy.deliveries:
	case <-time.After(5 * time.Second):
		t.Fatal("original broker delivery was not observed")
	}
	trace := httpProxy.snapshot()
	if len(trace) != 1 || trace[0].method != http.MethodPost || trace[0].status != 202 {
		t.Fatal("slow COMMIT callback affected actual publisher receipt")
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
		gate.commits.Load() != 1 || gate.returned.Load() != 0 || operation.Err() != nil {
		t.Fatal("held successful COMMIT callback did not retain original unacknowledged delivery", consumer.Err())
	}
	if entry := local.entry(t, f.ctx); entry.State != clientsim.Accepted || entry.Attempts != 1 || entry.Result != nil || entry.InFlight {
		t.Fatal("normal 202 did not preserve accepted state before SQL reconciliation")
	}
	events := senderCompletedEvents(t, f)
	short, cancelShort := context.WithTimeout(f.ctx, 150*time.Millisecond)
	err = consumer.Close(short)
	cancelShort()
	if !errors.Is(err, workers.ErrPersistenceSupervisorPending) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("forced close observer did not report pending callback", err)
	}
	select {
	case <-operation.Done():
	case <-time.After(time.Second):
		t.Fatal("forced close did not cancel admitted work")
	}
	groups, active, overlap = source.snapshot()
	if !errors.Is(operation.Err(), context.Canceled) || consumer.ConsumerReady() || consumer.Err() != nil ||
		consumer.Status().Generation != initialGeneration || consumer.Status().State != workers.PersistenceStateStopping ||
		len(groups) != 1 || active != 1 || overlap || proxy.consumerAcks.Load() != 0 || gate.returned.Load() != 0 || pool.Stats().InUse < 1 {
		t.Fatal("forced close lost pending callback/SQL/session ownership or started recovery", consumer.Status(), consumer.Err())
	}
	select {
	case <-consumer.Done():
		t.Fatal("forced close pending was mistaken for join")
	default:
	}
	select {
	case <-firstGroup.closed:
		t.Fatal("forced close observer released session before callback join")
	default:
	}
	select {
	case <-gate.finished:
		t.Fatal("canceled callback returned before explicit barrier release")
	default:
	}
	if err := pool.PingContext(f.ctx); err != nil {
		t.Fatal("pending forced close released borrowed SQL pool", err)
	}
	policy := clientsim.DefaultRecoveryOptions()
	policy.MaxRounds, policy.MaxSubmissions = 1, 1
	if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); err != nil {
		t.Fatal("committed original terminal was not queryable", err)
	}
	trace = httpProxy.snapshot()
	if len(trace) != 2 || trace[1].method != http.MethodGet || trace[1].status != 200 {
		t.Fatal("pending forced close caused client resubmission instead of terminal query")
	}
	body := f.readSenderResult(t, entry.ClientMessageID)
	if query, err := local.runner.ObserveCommandResult(f.ctx, body); err != nil || query {
		t.Fatal("original real result did not reconcile", err)
	}
	terminal := local.entry(t, f.ctx)
	if terminal.State != clientsim.Persisted || terminal.Attempts != 1 || terminal.Request != request || terminal.InFlight || !reflect.DeepEqual(terminal.Result, &status) {
		t.Fatal("original committed terminal not retained before late callback")
	}
	local.close(t)
	local = f.openPipelineSenderWithOptions(t, directory, options)
	if !reflect.DeepEqual(terminal, local.entry(t, f.ctx)) {
		t.Fatal("pre-callback terminal did not survive restart")
	}
	gate.unblock() // Return the original successful driver result after work cancellation.
	apiWait(t, gate.finished)
	apiWait(t, consumer.Done())
	var boundary *workers.PersistenceError
	if !errors.Is(consumer.Err(), context.Canceled) || errors.Is(consumer.Err(), database.ErrCommandCommitUnknown) ||
		!errors.As(consumer.Err(), &boundary) || boundary.Kind != workers.AcknowledgementFailed || boundary.Attempts != 1 ||
		consumer.ConsumerReady() || consumer.Status().Generation != initialGeneration || consumer.Status().Failures != 0 {
		t.Fatal("late successful callback lost cancellation or revived stopped owner", consumer.Status(), consumer.Err())
	}
	groups, active, overlap = source.snapshot()
	if len(groups) != 1 || active != 0 || overlap || proxy.consumerAcks.Load() != 0 || proxy.delivered.Load() != 1 || gate.returned.Load() != 1 || pool.Stats().InUse != 0 {
		t.Fatal("old callback ACKed or released ownership before actual join")
	}
	select {
	case <-firstGroup.closed:
	default:
		t.Fatal("old consumer transport was not joined")
	}
	queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
	t.Logf("broker sample after old owner join: ready=%d consumers=%d inspectError=%v", queue.Messages, queue.Consumers, err)
	// A client-side transport join does not wait for the broker's quorum
	// requeue to finish. Require the actual owned message before restarting.
	deadlineRequeue := time.Now().Add(15 * time.Second)
	for err != nil || queue.Messages != 1 || queue.Consumers != 0 {
		if time.Now().After(deadlineRequeue) {
			t.Fatalf("canceled callback did not retain original broker message: ready=%d consumers=%d error=%v", queue.Messages, queue.Consumers, err)
		}
		time.Sleep(20 * time.Millisecond)
		queue, err = f.channel.QueueInspect(rabbitmq.PersistQueue)
	}
	if err := pool.PingContext(f.ctx); err != nil {
		t.Fatal("joined supervisor closed caller SQL pool", err)
	}
	if !reflect.DeepEqual(events, senderCompletedEvents(t, f)) || !reflect.DeepEqual(terminal, local.entry(t, f.ctx)) || len(httpProxy.snapshot()) != 2 {
		t.Fatal("canceled late callback changed SQL events or client terminal")
	}
	// Close is terminal: a distinct, explicitly started owner takes over only
	// after the old owner has actually joined. This is not an automatic restart.
	fresh, err := workers.NewPersistenceSupervisor(pool, source.factory(f, u.String()), po)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := fresh.Close(c); err != nil {
			t.Error(err)
		}
		apiWait(t, fresh.Done())
	})
	setup, cancelSetup = context.WithTimeout(f.ctx, 8*time.Second)
	err = fresh.Start(setup, f.ctx)
	cancelSetup()
	if err != nil {
		t.Fatal(err)
	}
	apiRealEventually(t, fresh.ConsumerReady)
	var replay senderObservedDelivery
	select {
	case replay = <-proxy.deliveries:
	case <-time.After(5 * time.Second):
		t.Fatal("fresh owner did not receive broker redelivery")
	}
	if !replay.redelivered || replay.connection == original.connection || replay.tag == 0 || !bytes.Equal(replay.body, original.body) {
		t.Fatal("explicit restart changed original envelope/attempt or reused retired connection")
	}
	select {
	case ack := <-proxy.forwardedConsumerAcks:
		t.Logf("fresh owner redelivery ACK: connection=%d channel=%d tag=%d multiple=%v", ack.connection, ack.channel, ack.tag, ack.multiple)
		if ack.connection != replay.connection || ack.channel != replay.channel || ack.tag != replay.tag || ack.multiple {
			t.Fatal("fresh ACK did not match actual redelivery")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fresh owner did not ACK idempotent replay")
	}
	// Queue metrics may retain the pre-ACK sample while the channel is idle.
	// A successful relay write alone is also insufficient. The final proof
	// closes/joins the only consumer and requires zero requeued messages.
	readyNow, unackedNow, consumersNow, metricsOK := probe()
	t.Logf("active-channel sample after ACK forwarding: metrics=%v ready=%d unacked=%d consumers=%d", metricsOK, readyNow, unackedNow, consumersNow)
	if !fresh.ConsumerReady() || fresh.Err() != nil || proxy.consumerAcks.Load() != 1 {
		t.Fatal("fresh consumer did not finish the only ACK", fresh.Err())
	}
	if proxy.invalid.Load() || proxy.delivered.Load() != 2 || proxy.pubs.Load() != 0 || proxy.acks.Load() != 0 || proxy.didDrop.Load() ||
		gate.commits.Load() != 1 || gate.rollbacks.Load() != 1 || gate.returned.Load() != 1 {
		t.Fatal("unexpected frame direction, extra delivery or invalid capture")
	}
	if !reflect.DeepEqual(events, senderCompletedEvents(t, f)) {
		t.Fatal("late COMMIT callback changed original Outbox events")
	}
	if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); err != nil || len(httpProxy.snapshot()) != 2 || !reflect.DeepEqual(terminal, local.entry(t, f.ctx)) {
		t.Fatal("late COMMIT callback resumed client HTTP or changed terminal", err)
	}
	local.close(t)
	local = f.openPipelineSenderWithOptions(t, directory, options)
	if !reflect.DeepEqual(terminal, local.entry(t, f.ctx)) {
		t.Fatal("post-callback terminal changed on disk restart")
	}
	stop, cancelStop := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancelStop()
	if err := fresh.Drain(stop); err != nil {
		t.Fatal(err)
	}
	if err := fresh.Close(stop); err != nil {
		t.Fatal(err)
	}
	apiWait(t, fresh.Done())
	groups, active, overlap = source.snapshot()
	if len(groups) != 2 || active != 0 || overlap || consumer.Status().Generation != initialGeneration || fresh.Status().Generation != 1 || fresh.Status().Failures != 0 || fresh.Err() != nil {
		t.Fatal("explicit owners overlapped, retained ownership, or revived recovery")
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
		t.Fatal("fresh supervisor closed borrowed persistence SQL pool", err)
	}
	if pool.Stats().InUse != 0 {
		t.Fatal("SQL callback ownership remains after both joins")
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pool.PingContext(f.ctx); err == nil {
		t.Fatal("explicit SQL owner did not release pool after reconciliation")
	}
	t.Logf("old owner generation %d stopped with canceled post-COMMIT ACK, no revival; fresh owner generation=1, original envelope/attempt redelivered=true; 1 COMMIT / 1 replay ROLLBACK / 1 ACK; 1 POST / 1 GET, attempts=1; original SQL/Outbox/disk terminal unchanged; SQL owner closed after both joins", initialGeneration)
}
