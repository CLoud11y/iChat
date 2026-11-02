//go:build integration

package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"

	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/migrations"
	"iChat/tests/clientsim"
	"iChat/workers"
)

// This source supplies real topology-verified, independently owned sessions.
// Only the production supervisor decides when to retire/replace a generation.
type senderConsumerSource struct {
	mu      sync.Mutex
	groups  []*senderConsumerGroup
	active  int
	overlap bool
}

type senderConsumerGroup struct {
	session *rabbitmq.ConsumerSession
	source  *senderConsumerSource
	closed  chan struct{}
	once    sync.Once
	err     error
}

func (g *senderConsumerGroup) Consumers() []workers.PersistenceConsumer {
	return []workers.PersistenceConsumer{{Session: g.session, Prefetch: 1}}
}

func (g *senderConsumerGroup) Close(ctx context.Context) error {
	g.once.Do(func() {
		closeErr := g.session.Close(ctx)
		// A close observer deadline cannot substitute for transport join.
		joinErr := g.session.WaitClosed(context.Background())
		g.err = errors.Join(closeErr, joinErr)
		g.source.mu.Lock()
		g.source.active--
		g.source.mu.Unlock()
		close(g.closed)
	})
	return g.err
}

func (s *senderConsumerSource) snapshot() ([]*senderConsumerGroup, int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*senderConsumerGroup(nil), s.groups...), s.active, s.overlap
}

func (s *senderConsumerSource) factory(f *senderResultFixture, address string) workers.PersistenceConsumerFactory {
	return func(ctx context.Context) (workers.PersistenceConsumerGroup, error) {
		s.mu.Lock()
		if s.active != 0 {
			s.overlap = true
		}
		s.mu.Unlock()
		o := rabbitmq.DefaultConnectionOptions()
		o.ConnectTimeout, o.OperationTimeout, o.CloseTimeout = time.Second, 2*time.Second, time.Second
		o.WriteTimeout, o.Heartbeat = time.Second, time.Second
		session, err := rabbitmq.DialConsumer(ctx, address, f.management, f.vhost, o)
		if err != nil {
			return nil, workers.NewPersistenceConnectionError(err)
		}
		group := &senderConsumerGroup{session: session, source: s, closed: make(chan struct{})}
		s.mu.Lock()
		s.active++
		if s.active != 1 {
			s.overlap = true
		}
		s.groups = append(s.groups, group)
		s.mu.Unlock()
		return group, nil
	}
}

func TestProductionSenderConsumerRecoveryIntegration(t *testing.T) {
	for _, initial := range []clientsim.SendState{clientsim.Accepted, clientsim.PublishUnknown} {
		t.Run(string(initial), func(t *testing.T) {
			f := newSenderResultFixture(t)
			brokerURI, err := url.Parse(f.address)
			if err != nil || brokerURI.Host == "" {
				t.Fatal("invalid owned broker URI")
			}
			relay := newAPIRedisProxy(t, brokerURI.Host) // Raw TCP forwarding also carries AMQP.
			relay.setBlocked(false)
			brokerURI.Host = relay.listener.Addr().String()
			source := &senderConsumerSource{}
			po := workers.DefaultPersistenceSupervisorOptions()
			po.Worker.PollTimeout, po.Worker.ProbeInterval, po.Worker.ProbeTimeout = 100*time.Millisecond, time.Second, 500*time.Millisecond
			po.Worker.CloseTimeout = 3 * time.Second
			po.SetupTimeout, po.BaseDelay, po.MaxDelay = 5*time.Second, 100*time.Millisecond, 500*time.Millisecond
			consumer, err := workers.NewPersistenceSupervisor(f.pool, source.factory(f, brokerURI.String()), po)
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
				t.Fatal("invalid initial supervised connection ownership")
			}
			first := groups[0]
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
			relay.setBlocked(true) // Only the consumer's TCP path is interrupted.
			select {
			case <-first.closed:
			case <-time.After(8 * time.Second):
				t.Fatal("old consumer resources did not join after connection failure")
			}
			if consumer.ConsumerReady() || consumer.Err() != nil {
				t.Fatal("consumer did not remain managed and unready during outage", consumer.Err())
			}
			if _, active, overlap := source.snapshot(); active != 0 || overlap {
				t.Fatal("old consumer retained ownership during recovery")
			}
			// Remote cancellation is asynchronous with local transport join.
			apiRealEventually(t, func() bool {
				queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
				return err == nil && queue.Consumers == 0
			})
			httpProxy := newSenderRecoveryProxyWithOptions(t, "http://"+apiAddress(f.api), senderRecoveryProxyOptions{
				dropFirstPost: initial == clientsim.PublishUnknown,
			})
			options, directory := senderGuardOptions(f, httpProxy), t.TempDir()
			local := f.openPipelineSenderWithOptions(t, directory, options)
			request := senderGuardRequest(f)
			entry, err := local.sender.Queue(f.ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			err = local.runner.Submit(f.ctx, entry.ClientMessageID)
			if (initial == clientsim.Accepted && err != nil) ||
				(initial == clientsim.PublishUnknown && !errors.Is(err, clientsim.ErrHTTPUnavailable)) {
				t.Fatal("consumer outage changed actual broker acceptance", err)
			}
			policy := clientsim.DefaultRecoveryOptions()
			policy.MaxRounds, policy.MaxSubmissions = 1, 1
			if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); !errors.Is(err, clientsim.ErrRecoveryExhausted) {
				t.Fatal("single pending query did not retain command", err)
			}
			pending := local.entry(t, f.ctx)
			if pending.State != initial || pending.Attempts != 1 || pending.Request != request || pending.InFlight {
				t.Fatal("pending query changed original local command")
			}
			trace := httpProxy.snapshot()
			if len(trace) != 2 || trace[0].method != http.MethodPost || trace[1].method != http.MethodGet || trace[1].status != 200 {
				t.Fatal("outage performed more than one publication and one query")
			}
			response, err := messaging.DecodeCommandStatusResponse(trace[1].response)
			query, queryErr := messaging.NormalizeStatusQuery(uint64(f.users[0].ID), entry.ClientMessageID, entry.PayloadHash)
			if err != nil || queryErr != nil || response.Code != 0 || response.Data.State != messaging.StatusPendingOrUnknown || response.Data.ValidateFor(query) != nil {
				t.Fatal("unconsumed accepted command did not query as matching pending", err)
			}
			queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
			if err != nil || queue.Messages != 1 || queue.Consumers != 0 {
				t.Fatal("consumer outage lost confirmed original delivery", err)
			}
			for _, table := range []string{"message_command", "message", "message_outbox"} {
				var n int
				if err := f.pool.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != 0 {
					t.Fatal("idle consumer fault admitted business work", table, n, err)
				}
			}
			local.close(t)
			local = f.openPipelineSenderWithOptions(t, directory, options)
			if !reflect.DeepEqual(pending, local.entry(t, f.ctx)) {
				t.Fatal("outage checkpoint changed on restart")
			}
			relay.setBlocked(false)
			apiRealEventually(t, func() bool { return consumer.ConsumerReady() || consumer.Err() != nil })
			if !consumer.ConsumerReady() || consumer.Err() != nil || consumer.Status().Generation <= initialGeneration {
				t.Fatal("production supervisor did not recover a new ready generation", consumer.Err())
			}
			groups, active, overlap = source.snapshot()
			if len(groups) < 2 || active != 1 || overlap || groups[len(groups)-1].session.Done() == first.session.Done() {
				t.Fatal("replacement reused or overlapped an old connection")
			}
			status := awaitSenderGuardTerminal(t, f, entry)
			if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); err != nil {
				t.Fatal("recovered consumer terminal not queryable", err)
			}
			terminal := local.entry(t, f.ctx)
			if terminal.State != clientsim.Persisted || terminal.Attempts != 1 || terminal.Request != request || terminal.InFlight ||
				terminal.CommandID != entry.CommandID || terminal.PayloadHash != entry.PayloadHash || !reflect.DeepEqual(terminal.Result, &status) {
				t.Fatal("new generation changed original command terminal")
			}
			body := f.readSenderResult(t, entry.ClientMessageID)
			if needsQuery, err := local.runner.ObserveCommandResult(f.ctx, body); err != nil || needsQuery || !reflect.DeepEqual(terminal, local.entry(t, f.ctx)) {
				t.Fatal("late real notification changed queried terminal", err)
			}
			local.close(t)
			local = f.openPipelineSenderWithOptions(t, directory, options)
			if !reflect.DeepEqual(terminal, local.entry(t, f.ctx)) {
				t.Fatal("recovered terminal did not survive restart")
			}
			final := httpProxy.snapshot()
			if len(final) != 3 || final[2].method != http.MethodGet || final[2].status != 200 {
				t.Fatal("generation recovery triggered an extra POST")
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
			if active != 0 || overlap {
				t.Fatal("supervisor retained or overlapped owned groups")
			}
			for _, group := range groups {
				select {
				case <-group.closed:
				default:
					t.Fatal("consumer group not joined on shutdown")
				}
			}
			if err := drainSenderOutbox(stop, outbox, store); err != nil {
				t.Fatal(err)
			}
			apiRealEventually(t, func() bool {
				queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
				return err == nil && queue.Consumers == 0
			})
			queue, err = f.channel.QueueInspect(rabbitmq.PersistQueue)
			if err != nil || queue.Messages != 0 || queue.Consumers != 0 {
				t.Fatal("recovered consumer did not ACK original delivery", err)
			}
			for table, want := range map[string]int{"message_command": 1, "message": 1, "conversation": 1, "message_outbox": 2, "message_delivery_cursor": 0, "message_quarantine": 0} {
				var n int
				if err := f.pool.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != want {
					t.Fatal("consumer recovery changed logical collection", table, n, err)
				}
			}
			var completed, watermark int
			if err := f.pool.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM message_outbox WHERE status='completed'").Scan(&completed); err != nil || completed != 2 {
				t.Fatal("recovered Outbox not completed", err)
			}
			if err := f.pool.QueryRowContext(f.ctx, "SELECT last_seq FROM conversation").Scan(&watermark); err != nil || watermark != 1 {
				t.Fatal("consumer recovery allocated an extra sequence", err)
			}
			repo, err := database.NewMessageRepository(f.pool)
			if err != nil {
				t.Fatal(err)
			}
			after, err := repo.GetCommandStatus(f.ctx, uint64(f.users[0].ID), entry.ClientMessageID, entry.PayloadHash)
			if err != nil || !reflect.DeepEqual(after, status) {
				t.Fatal("original terminal changed after recovery", err)
			}
			if err := migrations.ApplyReliable(f.ctx, f.pool); err != nil {
				t.Fatal("schema fingerprint changed", err)
			}
			t.Logf("initial=%s; generation %d -> %d; %d fresh session groups, overlap=false, active=0; 1 POST / 2 GETs, attempts=1; final 1 command / 1 message / 1 conversation last_seq=1 / 2 completed Outbox / 0 cursor / 0 quarantine; queue ready=0 consumers=0", initial, initialGeneration, consumer.Status().Generation, len(groups))
		})
	}
}
