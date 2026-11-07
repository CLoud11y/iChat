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
	"iChat/tests/clientsim"
	"iChat/workers"

	mysql "github.com/go-sql-driver/mysql"
)

func TestProductionSenderCommitDrainIntegration(t *testing.T) {
	var conf config.Config
	f := newSenderResultFixtureWithAPIConfig(t, func(c *config.Config) { conf = *c })
	sqlProxy := newSenderCommitDelayProxy(t, net.JoinHostPort(conf.MYSQL.Host, conf.MYSQL.Port))
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
	consumer, err := workers.NewPersistenceSupervisor(pool, source.factory(f, u.String()), po)
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
	t.Cleanup(sqlProxy.releaseResponse)
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
	var commitOK senderCommitOK
	select {
	case ok := <-sqlProxy.captured:
		commitOK = senderCommitOK{connection: ok.connection, packet: bytes.Clone(ok.packet)}
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
		t.Fatal("SQL COMMIT delay affected actual publisher receipt")
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
	short, cancelShort := context.WithTimeout(f.ctx, 150*time.Millisecond)
	err = consumer.Drain(short)
	cancelShort()
	if !errors.Is(err, workers.ErrPersistenceSupervisorPending) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("drain observer did not report pending admitted SQL", err)
	}
	groups, active, overlap = source.snapshot()
	if consumer.ConsumerReady() || consumer.Err() != nil || consumer.Status().Generation != initialGeneration ||
		consumer.Status().State != workers.PersistenceStateStopping || len(groups) != 1 || active != 1 || overlap ||
		proxy.consumerAcks.Load() != 0 || sqlProxy.relayed.Load() != 0 || sqlProxy.dropped.Load() != 0 {
		t.Fatal("drain timeout canceled work, lost ownership, or revived a generation", consumer.Status(), consumer.Err())
	}
	select {
	case <-consumer.Done():
		t.Fatal("drain pending was mistaken for supervisor join")
	default:
	}
	select {
	case <-firstGroup.closed:
		t.Fatal("drain observer released original session before COMMIT callback")
	default:
	}
	if err := pool.PingContext(f.ctx); err != nil {
		t.Fatal("pending drain released caller SQL pool", err)
	}
	policy := clientsim.DefaultRecoveryOptions()
	policy.MaxRounds, policy.MaxSubmissions = 1, 1
	if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); err != nil {
		t.Fatal("committed original terminal was not queryable", err)
	}
	trace = httpProxy.snapshot()
	if len(trace) != 2 || trace[1].method != http.MethodGet || trace[1].status != 200 {
		t.Fatal("pending drain caused client resubmission instead of terminal query")
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
	sqlProxy.releaseResponse() // Forward the same real server OK after the drain observer returned pending.
	select {
	case forwarded := <-sqlProxy.forwarded:
		if forwarded.connection != commitOK.connection || !bytes.Equal(forwarded.packet, commitOK.packet) {
			t.Fatal("late COMMIT response changed bytes or connection")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late real COMMIT OK was not forwarded")
	}
	select {
	case ack := <-proxy.forwardedConsumerAcks:
		t.Logf("original delivery ACK after drain pending: connection=%d channel=%d tag=%d multiple=%v", ack.connection, ack.channel, ack.tag, ack.multiple)
		if ack.connection != original.connection || ack.channel != original.channel || ack.tag != original.tag || ack.multiple {
			t.Fatal("late callback ACK did not belong to the original delivery")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late COMMIT callback did not ACK original delivery")
	}
	// Queue metrics may retain the pre-ACK sample while the channel is idle.
	// A successful relay write alone is also insufficient. The final proof
	// closes/joins the only consumer and requires zero requeued messages.
	readyNow, unackedNow, consumersNow, metricsOK := probe()
	t.Logf("active-channel sample after ACK forwarding: metrics=%v ready=%d unacked=%d consumers=%d", metricsOK, readyNow, unackedNow, consumersNow)
	if consumer.ConsumerReady() || consumer.Err() != nil || proxy.consumerAcks.Load() != 1 {
		t.Fatal("draining original consumer did not finish its only ACK", consumer.Err())
	}
	if proxy.invalid.Load() || proxy.delivered.Load() != 1 || proxy.pubs.Load() != 0 || proxy.acks.Load() != 0 || proxy.didDrop.Load() || sqlProxy.invalid.Load() ||
		sqlProxy.commits.Load() != 1 || sqlProxy.rollbacks.Load() != 0 || sqlProxy.dropped.Load() != 0 || sqlProxy.relayed.Load() != 1 {
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
	if err := consumer.Drain(stop); err != nil {
		t.Fatal(err)
	}
	if err := consumer.Close(stop); err != nil {
		t.Fatal(err)
	}
	apiWait(t, consumer.Done())
	groups, active, overlap = source.snapshot()
	if len(groups) != 1 || active != 0 || overlap || consumer.Status().Generation != initialGeneration || consumer.Status().Failures != 0 || consumer.Err() != nil {
		t.Fatal("graceful close retained ownership, recorded a failure, or started another generation")
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
	t.Logf("generation %d unchanged; real COMMIT OK delayed through drain pending then forwarded unchanged; original delivery ACKed, no redelivery; 1 COMMIT / 0 ROLLBACK / 1 ACK; 1 POST / 1 GET, attempts=1; original SQL/Outbox/disk terminal unchanged", initialGeneration)
}
