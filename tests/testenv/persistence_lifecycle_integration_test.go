//go:build integration

package testenv

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"iChat/application"
	"iChat/config"
	"iChat/database"
	"iChat/health"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/migrations"
	"iChat/models"
	"iChat/utils"
	"iChat/workers"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
)

type persistenceLifecycleFixture struct {
	ctx        context.Context
	env        *Environment
	broker     RabbitMQConfig
	management *rabbitmq.Management
	pool       *sql.DB
	dsn        *mysql.Config
	control    *amqp.Channel
	producer   *rabbitmq.PublisherSession
	users      []models.User
	nextID     int
}

func newPersistenceLifecycleFixture(t *testing.T, ctx context.Context) *persistenceLifecycleFixture {
	t.Helper()
	env, broker := startRabbitFixture(t, ctx)
	c, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	db, err := utils.OpenMySQL(c)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	var schema string
	if err := pool.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); err != nil || schema != "ichat_test_"+env.RunID() {
		t.Fatal("refusing fixture writes outside owned schema", err)
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).AutoMigrate(&models.User{}, &models.Group{}, &models.Relation{}); err != nil {
		t.Fatal(err)
	}
	f := &persistenceLifecycleFixture{ctx: ctx, env: env, broker: broker, pool: pool}
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("%s-%d", env.RunID()[:24], i)
		f.users = append(f.users, models.User{Identifier: id, Phone: id, Name: "lifecycle-fixture"})
	}
	if err := db.WithContext(ctx).Create(&f.users).Error; err != nil {
		t.Fatal(err)
	}
	relations := []models.Relation{{OwnerId: f.users[0].ID, TargetId: f.users[1].ID, Type: models.FriendRelation},
		{OwnerId: f.users[1].ID, TargetId: f.users[0].ID, Type: models.FriendRelation}}
	if err := db.WithContext(ctx).Create(&relations).Error; err != nil {
		t.Fatal(err)
	}
	f.management, err = rabbitmq.NewManagement("http://"+broker.ManagementAddress, rabbitAdminUser, env.password)
	if err != nil {
		t.Fatal(err)
	}
	control := openPublisherConnection(t, ctx, broker)
	f.control, err = control.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := rabbitmq.EnsureLocal(ctx, f.control, f.management, broker.VHost); err != nil {
		t.Fatal(err)
	}
	f.producer, err = rabbitmq.DialPublisher(ctx, broker.AMQPURL(), rabbitmq.DefaultSessionOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.producer.Close(context.Background()) })
	f.dsn = mysql.NewConfig()
	f.dsn.Net, f.dsn.Addr = "tcp", net.JoinHostPort(c.MYSQL.Host, c.MYSQL.Port)
	f.dsn.User, f.dsn.Passwd, f.dsn.DBName = c.MYSQL.User, c.MYSQL.Password, c.MYSQL.DbName
	f.dsn.ParseTime, f.dsn.Timeout, f.dsn.ReadTimeout, f.dsn.WriteTimeout = true, 2*time.Second, 2*time.Second, 2*time.Second
	return f
}

func (f *persistenceLifecycleFixture) publish(t *testing.T) (messaging.Command, rabbitmq.PublishResult) {
	t.Helper()
	f.nextID++
	command, err := messaging.Normalize(uint64(f.users[0].ID), messaging.SendRequest{
		ClientMessageID: fmt.Sprintf("8fd7c2ef-ae26-4bb5-91a5-%012d", f.nextID), TargetType: messaging.Private,
		TargetID: strconv.FormatUint(uint64(f.users[1].ID), 10), Media: messaging.Text, Content: fmt.Sprintf("%s:%d:中🙂", f.env.RunID(), f.nextID)})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := f.producer.Publish(f.ctx, command, time.Now())
	if err != nil || receipt.Outcome != rabbitmq.PublishAccepted {
		t.Fatal("fixture publish failed", err)
	}
	return command, receipt
}

func (f *persistenceLifecycleFixture) session(t *testing.T, broker RabbitMQConfig) *rabbitmq.ConsumerSession {
	t.Helper()
	s, err := rabbitmq.DialConsumer(f.ctx, broker.AMQPURL(), f.management, broker.VHost, rabbitmq.DefaultConnectionOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s
}

func lifecycleAwait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("lifecycle did not reach expected state")
	}
}

func lifecycleEventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !check() {
		select {
		case <-deadline.C:
			t.Fatal("expected lifecycle state not observed")
		case <-tick.C:
		}
	}
}

func (f *persistenceLifecycleFixture) service(t *testing.T, pool *sql.DB, consumers []workers.PersistenceConsumer) *workers.PersistenceService {
	t.Helper()
	o := workers.DefaultPersistenceOptions()
	o.PollTimeout, o.ProbeInterval, o.ProbeTimeout = 100*time.Millisecond, 100*time.Millisecond, 300*time.Millisecond
	o.ProcessingTimeout = 30 * time.Second
	s, err := workers.NewPersistenceService(pool, consumers, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.Close(ctx); errors.Is(err, workers.ErrPersistenceServicePending) {
			t.Error("service still owns work during fixture cleanup", err)
		}
	})
	setup, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	err = s.Start(setup, f.ctx)
	cancel() // Must not cancel the successfully activated subscription or work.
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *persistenceLifecycleFixture) count(t *testing.T, command messaging.Command) int {
	t.Helper()
	var count int
	if err := f.pool.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM message WHERE id=?", command.CommandID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func (f *persistenceLifecycleFixture) checkDurableSet(t *testing.T, command messaging.Command) {
	t.Helper()
	var messages, events, commands int
	if err := f.pool.QueryRowContext(f.ctx, `SELECT (SELECT COUNT(*) FROM message WHERE id=?),
		(SELECT COUNT(*) FROM message_outbox WHERE command_id=?),
		(SELECT COUNT(*) FROM message_command WHERE command_id=? AND state='persisted')`,
		command.CommandID, command.CommandID, command.CommandID).Scan(&messages, &events, &commands); err != nil || messages != 1 || events != 2 || commands != 1 {
		t.Fatal("message/command/Outbox set is not unique and complete", messages, events, commands, err)
	}
}

func (f *persistenceLifecycleFixture) checkEmpty(t *testing.T) {
	t.Helper()
	if _, ok, err := f.control.Get(rabbitmq.PersistQueue, false); err != nil || ok {
		t.Fatal("closed consumer did not release expected MQ responsibility", err)
	}
}

func (f *persistenceLifecycleFixture) awaitQueueMessages(t *testing.T, count int) {
	t.Helper()
	lifecycleEventually(t, func() bool {
		var queue struct {
			Messages *int `json:"messages"`
		}
		_, err := f.env.rabbitAPI(f.ctx, "GET", "/api/queues/"+f.broker.VHost+"/"+rabbitmq.PersistQueue, nil, &queue)
		return err == nil && queue.Messages != nil && *queue.Messages == count
	})
}

func (f *persistenceLifecycleFixture) recover(t *testing.T, receipt rabbitmq.PublishResult, want database.CommandDisposition) {
	t.Helper()
	session := f.session(t, f.broker)
	stream, err := session.OpenConsumer(f.ctx, rabbitmq.PersistQueue, 1)
	if err != nil {
		t.Fatal(err)
	}
	d, err := stream.Next(f.ctx)
	if err != nil || d.Message.CorrelationID != receipt.PublishAttemptID || !d.Message.Redelivered {
		t.Fatal("original attempt was not redelivered", err)
	}
	repo, err := database.NewMessageRepository(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	h, err := workers.NewPersistenceHandler(repo)
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.Handle(f.ctx, d)
	if err != nil || result.Disposition != want {
		t.Fatal("redelivery did not recover expected terminal result", err)
	}
	if err := stream.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
}

func (f *persistenceLifecycleFixture) barrierPool(t *testing.T) (*sql.DB, <-chan context.Context, func()) {
	t.Helper()
	base, err := mysql.NewConnector(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan context.Context, 1), make(chan struct{})
	var committed atomic.Bool
	pool := sql.OpenDB(lifecycleCommitConnector{base: base, after: func(ctx context.Context) {
		if committed.CompareAndSwap(false, true) {
			entered <- ctx
			<-release
		}
	}})
	t.Cleanup(func() { pool.Close() })
	var once sync.Once
	return pool, entered, func() { once.Do(func() { close(release) }) }
}

func TestPersistenceLifecycleIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	f := newPersistenceLifecycleFixture(t, ctx)
	run := func(name string, test func(*testing.T)) {
		t.Helper()
		if !t.Run(name, test) {
			t.FailNow() // Do not reuse a queue contaminated by a failed prerequisite.
		}
	}
	run("graceful_commit_ack_and_unhandled_prefetch", func(t *testing.T) {
		first, _ := f.publish(t)
		second, receipt := f.publish(t)
		pool, barrier, release := f.barrierPool(t)
		defer release()
		s := f.service(t, pool, []workers.PersistenceConsumer{{Session: f.session(t, f.broker), Prefetch: 2}})
		var transaction context.Context
		select {
		case transaction = <-barrier:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if f.count(t, first) != 1 || f.count(t, second) != 0 {
			t.Fatal("sequential loop advanced beyond unacknowledged transaction")
		}
		s.StopAdmission()
		short, stop := context.WithTimeout(ctx, 20*time.Millisecond)
		err := s.Drain(short)
		stop()
		if !errors.Is(err, workers.ErrPersistenceServicePending) || transaction.Err() != nil || s.ConsumerReady() {
			t.Fatal("graceful stop cancelled the admitted transaction", err)
		}
		release()
		if err := s.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(ctx); err != nil {
			t.Fatal(err)
		}
		f.recover(t, receipt, database.CommandApplied)
		f.checkDurableSet(t, first)
		f.checkDurableSet(t, second)
		f.checkEmpty(t)
	})
	run("forced_close_pending_then_committed_redelivery", func(t *testing.T) {
		command, receipt := f.publish(t)
		pool, barrier, release := f.barrierPool(t)
		defer release()
		s := f.service(t, pool, []workers.PersistenceConsumer{{Session: f.session(t, f.broker), Prefetch: 1}})
		select {
		case <-barrier:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		short, stop := context.WithTimeout(ctx, 20*time.Millisecond)
		err := s.Close(short)
		stop()
		if !errors.Is(err, workers.ErrPersistenceServicePending) || f.count(t, command) != 1 {
			t.Fatal("pending observer lost committed storage responsibility", err)
		}
		select {
		case <-s.Done():
			t.Fatal("service reported joined before SQL callback returned")
		default:
		}
		if err := pool.PingContext(ctx); err != nil {
			t.Fatal("pending service closed shared pool", err)
		}
		release()
		lifecycleAwait(t, s.Done())
		if !errors.Is(s.Err(), context.Canceled) {
			t.Fatal("forced transaction/ACK cancellation was lost", s.Err())
		}
		f.recover(t, receipt, database.CommandReplayed)
		f.checkDurableSet(t, command)
		f.checkEmpty(t)
	})
	run("two_workers_sequential_per_connection", func(t *testing.T) {
		var commands []messaging.Command
		for i := 0; i < 8; i++ {
			command, _ := f.publish(t)
			commands = append(commands, command)
			if _, err := f.producer.Publish(ctx, command, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		f.awaitQueueMessages(t, 16) // Observe a positive baseline before consuming.
		s := f.service(t, f.pool, []workers.PersistenceConsumer{{Session: f.session(t, f.broker), Prefetch: 2}, {Session: f.session(t, f.broker), Prefetch: 2}})
		lifecycleEventually(t, func() bool {
			for _, command := range commands {
				if f.count(t, command) != 1 {
					return false
				}
			}
			return true
		})
		// Give the later duplicate attempts a positive broker completion check,
		// rather than stopping as soon as the first copies commit.
		f.awaitQueueMessages(t, 0)
		if err := s.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(ctx); err != nil {
			t.Fatal(err)
		}
		for _, command := range commands {
			f.checkDurableSet(t, command)
		}
		f.checkEmpty(t)
	})
	run("lost_actual_ack_replays_committed_command", func(t *testing.T) {
		command, receipt := f.publish(t)
		proxy := startSessionProxy(t, f.broker.Address)
		proxy.dropConsumerAcks.Store(true)
		broker := f.broker
		broker.Address = proxy.address
		s := f.service(t, f.pool, []workers.PersistenceConsumer{{Session: f.session(t, broker), Prefetch: 1}})
		lifecycleEventually(t, func() bool { return proxy.consumerAcksDropped.Load() == 1 && f.count(t, command) == 1 })
		if err := s.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(ctx); err != nil {
			t.Fatal(err)
		}
		f.recover(t, receipt, database.CommandReplayed)
		f.checkDurableSet(t, command)
		f.checkEmpty(t)
	})
	run("real_factory_runtime_drain", func(t *testing.T) {
		command, _ := f.publish(t)
		pool, barrier, release := f.barrierPool(t)
		defer release()
		factory, err := application.NewPersistenceFactory(pool, workers.DefaultPersistenceOptions())
		if err != nil {
			t.Fatal(err)
		}
		conf := persistenceRuntimeConfig(f)
		type captured struct {
			service *workers.PersistenceService
			health  health.RuntimeHealth
		}
		capturedRole := make(chan captured, 1)
		factories := application.RuntimeFactories{Persistence: func(ctx context.Context, inputs application.RoleDependencies) (application.RuntimeService, error) {
			service, err := factory(ctx, inputs)
			if err == nil {
				capturedRole <- captured{service.(*workers.PersistenceService), inputs.Health}
			}
			return service, err
		}, Quarantine: persistenceFixtureCompanion, Outbox: persistenceFixtureCompanion}
		runCtx, stop := context.WithCancel(ctx)
		defer stop()
		result := make(chan error, 1)
		go func() { result <- application.RunRuntime(runCtx, conf, factories, application.DefaultRuntimeOptions()) }()
		finished := false
		t.Cleanup(func() {
			stop()
			if !finished {
				select {
				case <-result:
				case <-time.After(20 * time.Second):
					t.Error("runtime did not join during cleanup")
				}
			}
		})
		var role captured
		select {
		case role = <-capturedRole:
		case err := <-result:
			finished = true
			t.Fatal("runtime did not construct the real persistence role", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		lifecycleEventually(t, func() bool { return role.health.RuntimeReadiness().Consumers == health.Ready })
		select {
		case <-barrier:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		stop()
		lifecycleEventually(t, func() bool { return !role.service.ConsumerReady() })
		if role.health.RuntimeReadiness().Consumers != health.Stopping {
			t.Fatal("runtime health stayed ready during drain")
		}
		release()
		select {
		case err := <-result:
			finished = true
			if err != nil {
				t.Fatal("runtime failed graceful real SQL/ACK drain", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if err := pool.PingContext(ctx); err != nil {
			t.Fatal("runtime adapter closed externally owned storage", err)
		}
		f.checkDurableSet(t, command)
		f.checkEmpty(t)
	})
	// Run the infrastructure fault last; its recovery may change the test port.
	run("storage_probe_fail_stop_and_explicit_recovery", func(t *testing.T) {
		s := f.service(t, f.pool, []workers.PersistenceConsumer{{Session: f.session(t, f.broker), Prefetch: 1}})
		if err := f.env.SetMySQLRunning(ctx, false); err != nil {
			t.Fatal(err)
		}
		lifecycleAwait(t, s.Done())
		if s.ConsumerReady() || !errors.Is(s.Err(), workers.ErrPersistenceServiceStopped) {
			t.Fatal("idle storage probe failure did not stop service", s.Err())
		}
		command, _ := f.publish(t) // MQ acceptance remains independent of MySQL.
		if err := f.env.SetMySQLRunning(ctx, true); err != nil {
			t.Fatal(err)
		}
		conf, err := f.env.Config()
		if err != nil {
			t.Fatal(err)
		}
		dsn := f.dsn.Clone()
		dsn.Addr = net.JoinHostPort(conf.MYSQL.Host, conf.MYSQL.Port)
		pool, err := sql.Open("mysql", dsn.FormatDSN())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pool.Close() })
		// Wait on state with a bounded probe cadence; do not hammer a MySQL
		// socket that is listening before its handshake becomes available.
		probe, stopProbe := context.WithTimeout(ctx, 30*time.Second)
		defer stopProbe()
		for pool.PingContext(probe) != nil {
			select {
			case <-probe.Done():
				t.Fatal("owned database did not recover")
			case <-time.After(200 * time.Millisecond):
			}
		}
		if s.ConsumerReady() {
			t.Fatal("retired service automatically resumed")
		}
		fresh := f.service(t, pool, []workers.PersistenceConsumer{{Session: f.session(t, f.broker), Prefetch: 1}})
		var exists int
		lifecycleEventually(t, func() bool {
			return pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message WHERE id=?", command.CommandID).Scan(&exists) == nil && exists == 1
		})
		if err := fresh.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if err := fresh.Close(ctx); err != nil {
			t.Fatal(err)
		}
		f.checkEmpty(t)
		var gaps int
		if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation c WHERE
			c.last_seq<>(SELECT COUNT(*) FROM message m WHERE m.conversation_id=c.id) OR
			c.last_seq<>(SELECT COALESCE(MAX(m.seq),0) FROM message m WHERE m.conversation_id=c.id)`).Scan(&gaps); err != nil || gaps != 0 {
			t.Fatal("worker lifecycle left sequence gaps", err)
		}
	})
}

func persistenceRuntimeConfig(f *persistenceLifecycleFixture) config.Config {
	return config.Config{RUNTIME: config.RuntimeConfig{Mode: config.ModeWorker}, RABBITMQ: config.RabbitMQConfig{
		URL: f.broker.AMQPURL(), VHost: f.broker.VHost, ManagementURL: "http://" + f.broker.ManagementAddress,
		ManagementUsername: rabbitAdminUser, ManagementPassword: f.env.password, PublisherSlots: 1,
		ConsumerWorkers: 2, ConsumerPrefetch: 2, ConsumerOperationTimeout: 2 * time.Second, PublishTimeout: 2 * time.Second,
		ConnectTimeout: 10 * time.Second, WriteTimeout: time.Second, CloseTimeout: time.Second, Heartbeat: time.Second,
		RetryMin: 50 * time.Millisecond, RetryMax: 200 * time.Millisecond}}
}

// Only persistence is real in the runtime case. These required companion roles
// test orchestration; they neither audit quarantine nor deliver Outbox events.
type persistenceCompanion struct{ *fixtureRuntimeService }

func (s *persistenceCompanion) ConsumerReady() bool {
	return s.starts.Load() > 0 && s.stops.Load() == 0
}
func persistenceFixtureCompanion(_ context.Context, inputs application.RoleDependencies) (application.RuntimeService, error) {
	return &persistenceCompanion{&fixtureRuntimeService{inputs: inputs, done: make(chan struct{})}}, nil
}

// SQL barriers exist only in integration builds. Ping is forwarded, so the
// production service's storage monitor still sends real MySQL probes.
type lifecycleCommitConnector struct {
	base  driver.Connector
	after func(context.Context)
}

func (c lifecycleCommitConnector) Driver() driver.Driver { return c.base.Driver() }
func (c lifecycleCommitConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return lifecycleCommitConn{Conn: conn, after: c.after}, nil
}

type lifecycleCommitConn struct {
	driver.Conn
	after func(context.Context)
}

func (c lifecycleCommitConn) Ping(ctx context.Context) error { return c.Conn.(driver.Pinger).Ping(ctx) }
func (c lifecycleCommitConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return lifecycleCommitTx{Tx: tx, ctx: ctx, after: c.after}, nil
}

type lifecycleCommitTx struct {
	driver.Tx
	ctx   context.Context
	after func(context.Context)
}

func (t lifecycleCommitTx) Commit() error {
	err := t.Tx.Commit()
	if err == nil {
		t.after(t.ctx)
	}
	return err
}
