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
	"iChat/migrations"
	"iChat/tests/clientsim"
	"iChat/workers"

	mysql "github.com/go-sql-driver/mysql"
)

func senderAwaitQuarantineQueue(t *testing.T, f *senderResultFixture, name string, messages int) {
	t.Helper()
	apiRealEventually(t, func() bool {
		q, err := f.channel.QueueInspect(name)
		return err == nil && q.Messages == messages && q.Consumers == 0
	})
}

func TestProductionSenderQuarantineIntegration(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		name := "accepted"
		if unknown {
			name = "lost_acceptance_unknown"
		}
		t.Run(name, func(t *testing.T) { testSenderQuarantine(t, unknown) })
	}
}

func testSenderQuarantine(t *testing.T, unknown bool) {
	var conf config.Config
	f := newSenderResultFixtureWithAPIConfig(t, func(c *config.Config) { conf = *c })
	fault := &senderConstraintFault{}
	fault.enabled.Store(true)
	sqlConfig := mysql.NewConfig()
	sqlConfig.Net, sqlConfig.Addr = "tcp", net.JoinHostPort(conf.MYSQL.Host, conf.MYSQL.Port)
	sqlConfig.User, sqlConfig.Passwd, sqlConfig.DBName = conf.MYSQL.User, conf.MYSQL.Password, conf.MYSQL.DbName
	sqlConfig.ParseTime, sqlConfig.InterpolateParams, sqlConfig.Loc = true, true, time.Local
	sqlConfig.Params = map[string]string{"charset": "utf8mb4"}
	sqlConfig.Timeout, sqlConfig.ReadTimeout, sqlConfig.WriteTimeout = 3*time.Second, 5*time.Second, 5*time.Second
	connector, err := mysql.NewConnector(sqlConfig)
	if err != nil {
		t.Fatal("configure owned constraint fault pool")
	}
	pool := sql.OpenDB(senderConstraintConnector{base: connector, fault: fault})
	t.Cleanup(func() { pool.Close() })
	var schema string
	if err := pool.QueryRowContext(f.ctx, "SELECT DATABASE()").Scan(&schema); err != nil || schema != conf.MYSQL.DbName {
		t.Fatal("constraint fault pool escaped owned schema")
	}
	u, err := url.Parse(f.address)
	if err != nil || u.Scheme != "amqp" || u.Host == "" {
		t.Fatal("invalid owned AMQP URI")
	}
	mainProxy := newSenderConsumerAckProxy(t, u.Host)
	targetProxy := newSenderConfirmProxy(t, u.Host)
	targetURI := *u
	targetURI.Host = targetProxy.listener.Addr().String()
	so := rabbitmq.DefaultSessionOptions()
	so.Publisher.Slots = 1
	so.OperationTimeout, so.CloseTimeout = 2*time.Second, time.Second
	setup, cancelSetup := context.WithTimeout(f.ctx, 5*time.Second)
	target, err := rabbitmq.DialQuarantinePublisher(setup, targetURI.String(), so)
	cancelSetup()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := target.Close(c); err != nil {
			t.Error(err)
		}
	})
	basePubs, baseConfirms := targetProxy.pubs.Load(), targetProxy.acks.Load()
	u.Host = mainProxy.listener.Addr().String()
	source := &senderConsumerSource{}
	po := workers.DefaultPersistenceSupervisorOptions()
	po.Worker.PollTimeout, po.Worker.ProbeInterval, po.Worker.ProbeTimeout = 100*time.Millisecond, time.Second, 500*time.Millisecond
	po.Worker.QuarantinePublisher = target
	main, err := workers.NewPersistenceSupervisor(pool, source.factory(f, u.String()), po)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := main.Close(c); err != nil {
			t.Error(err)
		}
		apiWait(t, main.Done())
	})
	setup, cancelSetup = context.WithTimeout(f.ctx, 8*time.Second)
	err = main.Start(setup, f.ctx)
	cancelSetup()
	if err != nil {
		t.Fatal(err)
	}
	apiRealEventually(t, main.ConsumerReady)
	httpProxy := newSenderRecoveryProxyWithOptions(t, "http://"+apiAddress(f.api), senderRecoveryProxyOptions{dropFirstPost: unknown})
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
	err = local.runner.Submit(f.ctx, entry.ClientMessageID)
	if (!unknown && err != nil) || (unknown && !errors.Is(err, clientsim.ErrHTTPUnavailable)) {
		t.Fatal("unexpected actual submission outcome", err)
	}
	initial := local.entry(t, f.ctx)
	wantState := clientsim.Accepted
	if unknown {
		wantState = clientsim.PublishUnknown
	}
	if initial.State != wantState || initial.Attempts != 1 || initial.Request != request || initial.InFlight || initial.Result != nil {
		t.Fatal("original accepted/unknown submission not retained")
	}
	var original senderObservedDelivery
	select {
	case original = <-mainProxy.deliveries:
	case <-time.After(5 * time.Second):
		t.Fatal("missing original broker delivery")
	}
	trace := httpProxy.snapshot()
	if len(trace) != 1 || trace[0].method != http.MethodPost || trace[0].status != 202 {
		t.Fatal("original API was not actually accepted")
	}
	accepted, err := messaging.DecodeSubmissionResponse(trace[0].response)
	envelope, decodeErr := messaging.DecodeEnvelope(original.body)
	decoded, validateErr := messaging.ValidateEnvelope(envelope)
	if err != nil || decodeErr != nil || validateErr != nil || decoded != command || !accepted.Data.Matches(command) ||
		envelope.PublishAttemptID != accepted.Data.PublishAttemptID || original.redelivered {
		t.Fatal("original envelope/attempt differs from actual receipt")
	}
	select {
	case ack := <-mainProxy.forwardedConsumerAcks:
		if ack.connection != original.connection || ack.channel != original.channel || ack.tag != original.tag || ack.multiple {
			t.Fatal("source ACK did not match original delivery")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("confirmed quarantine transfer did not ACK source")
	}
	if targetProxy.pubs.Load() != basePubs+1 || targetProxy.acks.Load() != baseConfirms+1 || targetProxy.didDrop.Load() || targetProxy.invalid.Load() {
		t.Fatal("quarantine transfer lacked exactly one real target confirm")
	}
	stop, cancelStop := context.WithTimeout(f.ctx, 5*time.Second)
	if err := main.Drain(stop); err != nil {
		t.Fatal(err)
	}
	if err := main.Close(stop); err != nil {
		t.Fatal(err)
	}
	cancelStop()
	apiWait(t, main.Done())
	groups, active, overlap := source.snapshot()
	if len(groups) != 1 || active != 0 || overlap || main.Err() != nil || main.Status().Generation != 1 {
		t.Fatal("quarantine source lost ownership or restarted", main.Err())
	}
	senderAwaitQuarantineQueue(t, f, rabbitmq.PersistQueue, 0)
	senderAwaitQuarantineQueue(t, f, rabbitmq.QuarantineQueue, 1)
	// Audit is deliberately started only after the actual target owns one
	// confirmed wrapper and the source owner has joined. No control Get/ACK.
	auditURI, err := url.Parse(f.address)
	if err != nil {
		t.Fatal(err)
	}
	auditProxy := newSenderConsumerAckProxy(t, auditURI.Host)
	auditURI.Host = auditProxy.listener.Addr().String()
	co := rabbitmq.DefaultConnectionOptions()
	co.OperationTimeout, co.CloseTimeout = 2*time.Second, time.Second
	setup, cancelSetup = context.WithTimeout(f.ctx, 5*time.Second)
	auditSession, err := rabbitmq.DialConsumer(setup, auditURI.String(), f.management, f.vhost, co)
	cancelSetup()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := auditSession.Close(c); err != nil {
			t.Error(err)
		}
		if err := auditSession.WaitClosed(c); err != nil {
			t.Error(err)
		}
	})
	qo := workers.DefaultQuarantineOptions()
	qo.PollTimeout, qo.ProbeInterval, qo.ProbeTimeout = 100*time.Millisecond, time.Second, 500*time.Millisecond
	audit, err := workers.NewQuarantineService(f.pool, []workers.QuarantineConsumer{{Session: auditSession, Prefetch: 1}}, qo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := audit.Close(c); err != nil {
			t.Error(err)
		}
		apiWait(t, audit.Done())
	})
	setup, cancelSetup = context.WithTimeout(f.ctx, 5*time.Second)
	err = audit.Start(setup, f.ctx)
	cancelSetup()
	if err != nil {
		t.Fatal(err)
	}
	var wrapper senderObservedDelivery
	select {
	case wrapper = <-auditProxy.deliveries:
	case <-time.After(5 * time.Second):
		t.Fatal("audit worker did not receive real quarantine wrapper")
	}
	quarantined, err := rabbitmq.DecodeQuarantineEnvelope(wrapper.body)
	if err != nil || quarantined.ErrorCode() != "DATA_CONSTRAINT" || wrapper.redelivered {
		t.Fatal("real constraint did not produce expected quarantine envelope", err)
	}
	raw, err := quarantined.Source()
	if err != nil || !bytes.Equal(raw.Body, original.body) || raw.MessageID != command.CommandID || raw.CorrelationID != envelope.PublishAttemptID {
		t.Fatal("quarantine changed original command body/physical attempt", err)
	}
	repo, err := database.NewQuarantineRepository(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	var before database.QuarantineDetail
	apiRealEventually(t, func() bool { before, err = repo.Get(f.ctx, quarantined.TransportID()); return err == nil })
	identity := before.Summary
	for _, field := range []struct {
		value *string
		want  string
	}{{identity.SenderID, command.SenderID}, {identity.ClientMessageID, command.ClientMessageID}, {identity.CommandID, command.CommandID}, {identity.PayloadHash, command.PayloadHash}} {
		if field.value == nil || *field.value != field.want {
			t.Fatal("audit did not retain canonical command identity")
		}
	}
	actualStable, err := before.Input.Envelope().StableContent()
	wantStable, stableErr := quarantined.StableContent()
	if err != nil || stableErr != nil || !bytes.Equal(actualStable, wantStable) || identity.State != "open" || identity.ErrorCode != "DATA_CONSTRAINT" || identity.ReplayAttemptID != nil || identity.ResolvedAt != nil {
		t.Fatal("audit is not original open technical quarantine")
	}
	select {
	case ack := <-auditProxy.forwardedConsumerAcks:
		if ack.connection != wrapper.connection || ack.channel != wrapper.channel || ack.tag != wrapper.tag || ack.multiple {
			t.Fatal("audit ACK did not match actual wrapper")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("committed quarantine audit did not ACK wrapper")
	}
	stop, cancelStop = context.WithTimeout(f.ctx, 5*time.Second)
	if err := audit.Drain(stop); err != nil {
		t.Fatal(err)
	}
	if err := audit.Close(stop); err != nil {
		t.Fatal(err)
	}
	apiWait(t, audit.Done())
	if err := auditSession.Close(stop); err != nil {
		t.Fatal(err)
	}
	if err := auditSession.WaitClosed(stop); err != nil {
		t.Fatal(err)
	}
	cancelStop()
	// Removing the SQL fault alone does not authorize client resending.
	fault.enabled.Store(false)
	if err := local.runner.Poll(f.ctx, entry.ClientMessageID); err != nil {
		t.Fatal(err)
	}
	protected := local.entry(t, f.ctx)
	if protected.State != clientsim.Quarantined || protected.Attempts != 1 || protected.Request != request || protected.InFlight || protected.Result == nil || protected.Result.State != messaging.StatusQuarantined || protected.Result.Message != nil {
		t.Fatal("actual audited status did not protect original send")
	}
	if err := local.runner.Submit(f.ctx, entry.ClientMessageID); !errors.Is(err, clientsim.ErrBlocked) || len(httpProxy.snapshot()) != 2 {
		t.Fatal("explicit quarantined submission reached HTTP", err)
	}
	local.close(t)
	local = f.openPipelineSenderWithOptions(t, directory, options)
	if !reflect.DeepEqual(protected, local.entry(t, f.ctx)) {
		t.Fatal("quarantined checkpoint changed after disk reopen")
	}
	if err := local.runner.Submit(f.ctx, entry.ClientMessageID); !errors.Is(err, clientsim.ErrBlocked) || len(httpProxy.snapshot()) != 2 {
		t.Fatal("disk reopen authorized quarantined repost", err)
	}
	policy := clientsim.DefaultRecoveryOptions()
	policy.MaxRounds, policy.MaxSubmissions = 2, 2
	if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); !errors.Is(err, clientsim.ErrRecoveryExhausted) {
		t.Fatal("quarantine recovery did not end as bounded nonterminal query", err)
	}
	trace = httpProxy.snapshot()
	if len(trace) != 4 {
		t.Fatal("quarantine recovery changed HTTP count", len(trace))
	}
	for _, exchange := range trace[1:] {
		value, err := messaging.DecodeCommandStatusResponse(exchange.response)
		if exchange.method != http.MethodGet || exchange.status != 200 || err != nil || value.Code != 0 || !reflect.DeepEqual(&value.Data, protected.Result) {
			t.Fatal("quarantine recovery resubmitted or returned another status", err)
		}
	}
	if !reflect.DeepEqual(protected, local.entry(t, f.ctx)) {
		t.Fatal("finite recovery changed quarantined identity/request/attempts")
	}
	local.close(t)
	local = f.openPipelineSenderWithOptions(t, directory, options)
	if !reflect.DeepEqual(protected, local.entry(t, f.ctx)) {
		t.Fatal("post-recovery quarantine checkpoint changed")
	}
	after, err := repo.Get(f.ctx, quarantined.TransportID())
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("query/blocked resend modified original audit", err)
	}
	for table, want := range map[string]int{"message_command": 0, "message": 0, "conversation": 0, "message_outbox": 0, "message_delivery_cursor": 0, "message_quarantine": 1} {
		var count int
		if err := f.pool.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != want {
			t.Fatal("quarantine created partial business collection", table, count, err)
		}
	}
	for _, name := range []string{rabbitmq.PersistQueue, rabbitmq.QuarantineQueue} {
		senderAwaitQuarantineQueue(t, f, name, 0)
		probe := senderNamedQueueProbe(t, f, name)
		apiRealEventually(t, func() bool {
			ready, unacked, consumers, ok := probe()
			return ok && ready == 0 && unacked == 0 && consumers == 0
		})
	}
	if mainProxy.delivered.Load() != 1 || mainProxy.consumerAcks.Load() != 1 || mainProxy.invalid.Load() || mainProxy.didDrop.Load() ||
		auditProxy.delivered.Load() != 1 || auditProxy.consumerAcks.Load() != 1 || auditProxy.invalid.Load() || auditProxy.didDrop.Load() ||
		targetProxy.pubs.Load() != basePubs+1 || targetProxy.acks.Load() != baseConfirms+1 {
		t.Fatal("extra transfer, delivery, ACK or invalid frame")
	}
	if fault.updates.Load() != 1 || fault.faults.Load() != 1 || fault.begins.Load() != 1 || fault.commits.Load() != 0 || fault.rollbacks.Load() != 1 || pool.Stats().InUse != 0 {
		t.Fatal("source did not roll back one real CHECK failure after message INSERT without local retry")
	}
	if err := migrations.ApplyReliable(f.ctx, f.pool); err != nil {
		t.Fatal("fault cleanup changed migration fingerprint", err)
	}
	t.Logf("unknown=%v; 1 POST / 3 GET; actual DATA_CONSTRAINT rollback -> confirmed quarantine -> committed audit; 0 business rows / 1 immutable open audit; attempts=1, explicit and finite recovery repost blocked after repair and disk reopen; both queues ready/unacked/consumers=0", unknown)
}
