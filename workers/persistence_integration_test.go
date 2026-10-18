//go:build integration

package workers

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/migrations"
	"iChat/models"
	"iChat/tests/testenv"
	"iChat/utils"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
)

type persistenceFixture struct {
	ctx        context.Context
	cleanup    func(func())
	env        *testenv.Environment
	pool       *sql.DB
	repository *database.MessageRepository
	handler    *PersistenceHandler
	broker     testenv.RabbitMQConfig
	management *rabbitmq.Management
	producer   *rabbitmq.PublisherSession
	control    *amqp.Channel
	users      []models.User
	group      models.Group
	dsn        string
}

func newPersistenceFixture(t *testing.T, ctx context.Context) *persistenceFixture {
	t.Helper()
	env, err := testenv.StartWithRabbitMQ(ctx, filepath.Join("..", "docker-compose.test.yml"))
	if env != nil {
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := env.Close(cleanup); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("persistence consumer fixture runId=%s RabbitMQ=%s", env.RunID(), rabbitmq.BrokerVersion)
	conf, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	db, err := utils.OpenMySQL(conf)
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
		t.Fatal("refusing persistence writes outside owned schema", err)
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).AutoMigrate(&models.User{}, &models.Group{}, &models.Relation{}); err != nil {
		t.Fatal(err)
	}
	f := &persistenceFixture{ctx: ctx, cleanup: t.Cleanup, env: env, pool: pool}
	for i := 0; i < 3; i++ {
		identity := fmt.Sprintf("%s-%d", env.RunID()[:24], i)
		f.users = append(f.users, models.User{Identifier: identity, Phone: identity, Name: "consumer-fixture"})
	}
	if err := db.WithContext(ctx).Create(&f.users).Error; err != nil {
		t.Fatal(err)
	}
	f.group = models.Group{Name: "consumer-fixture", OwnerId: f.users[0].ID, MaxUser: 50}
	if err := db.WithContext(ctx).Create(&f.group).Error; err != nil {
		t.Fatal(err)
	}
	relations := []models.Relation{
		{OwnerId: f.users[0].ID, TargetId: f.users[1].ID, Type: models.FriendRelation},
		{OwnerId: f.users[1].ID, TargetId: f.users[0].ID, Type: models.FriendRelation},
		{OwnerId: f.users[0].ID, TargetId: f.group.ID, Type: models.GroupRelation},
	}
	if err := db.WithContext(ctx).Create(&relations).Error; err != nil {
		t.Fatal(err)
	}
	f.repository, err = database.NewMessageRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	f.handler, err = NewPersistenceHandler(f.repository)
	if err != nil {
		t.Fatal(err)
	}
	f.broker, err = env.RabbitMQ()
	if err != nil {
		t.Fatal(err)
	}
	f.management, err = rabbitmq.NewManagement("http://"+f.broker.ManagementAddress, "ichat_test_admin", conf.REDIS.Password)
	if err != nil {
		t.Fatal(err)
	}
	control, err := amqp.DialConfig(f.broker.AMQPURL(), amqp.Config{Heartbeat: time.Second, Dial: func(network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, address)
		if err == nil {
			deadline, _ := ctx.Deadline()
			conn.SetDeadline(deadline)
		}
		return conn, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	f.control, err = control.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := rabbitmq.EnsureLocal(ctx, f.control, f.management, f.broker.VHost); err != nil {
		t.Fatal(err)
	}
	o := rabbitmq.DefaultSessionOptions()
	o.Publisher.Slots = 1
	f.producer, err = rabbitmq.DialPublisher(ctx, f.broker.AMQPURL(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.producer.Close(context.Background()) })
	dsn := mysql.NewConfig()
	dsn.User, dsn.Passwd, dsn.DBName = conf.MYSQL.User, conf.MYSQL.Password, conf.MYSQL.DbName
	dsn.Net, dsn.Addr, dsn.ParseTime = "tcp", net.JoinHostPort(conf.MYSQL.Host, conf.MYSQL.Port), true
	dsn.Timeout, dsn.ReadTimeout, dsn.WriteTimeout = 3*time.Second, 3*time.Second, 3*time.Second
	f.dsn = dsn.FormatDSN()
	return f
}

func (f *persistenceFixture) command(t *testing.T, n int, target messaging.TargetType, media messaging.Media) messaging.Command {
	t.Helper()
	id := f.users[1].ID
	if target == messaging.Group {
		id = f.group.ID
	}
	command, err := messaging.Normalize(uint64(f.users[0].ID), messaging.SendRequest{
		ClientMessageID: fmt.Sprintf("8fd7c2ef-ae26-4bb5-91a5-%012d", n), TargetType: target,
		TargetID: strconv.FormatUint(uint64(id), 10), Media: media, Content: fmt.Sprintf("%s:%d:中🙂\x00", f.env.RunID(), n),
	})
	if err != nil {
		t.Fatal(err)
	}
	return command
}

func (f *persistenceFixture) publish(t *testing.T, command messaging.Command) rabbitmq.PublishResult {
	t.Helper()
	result, err := f.producer.Publish(f.ctx, command, time.Now())
	if err != nil || result.Outcome != rabbitmq.PublishAccepted {
		t.Fatal("fixture publish not accepted", err)
	}
	return result
}

func (f *persistenceFixture) open(t *testing.T) *rabbitmq.ConsumerChannel {
	t.Helper()
	o := rabbitmq.DefaultConnectionOptions()
	o.OperationTimeout = 5 * time.Second
	s, err := rabbitmq.DialConsumer(f.ctx, f.broker.AMQPURL(), f.management, f.broker.VHost, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	stream, err := s.OpenConsumer(f.ctx, rabbitmq.PersistQueue, 1)
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func (f *persistenceFixture) next(t *testing.T, stream *rabbitmq.ConsumerChannel) *rabbitmq.ConsumerDelivery {
	t.Helper()
	d, err := stream.Next(f.ctx)
	if err != nil {
		t.Fatal("delivery not available", err)
	}
	return d
}

func (f *persistenceFixture) close(t *testing.T, stream *rabbitmq.ConsumerChannel) {
	t.Helper()
	if err := stream.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
}

func (f *persistenceFixture) assertStored(t *testing.T, command messaging.Command, messages, events int) {
	t.Helper()
	for _, check := range []struct {
		query string
		want  int
	}{
		{"SELECT COUNT(*) FROM message WHERE id=?", messages},
		{"SELECT COUNT(*) FROM message_command WHERE command_id=?", 1},
		{"SELECT COUNT(*) FROM message_outbox WHERE command_id=?", events},
	} {
		var count int
		if err := f.pool.QueryRowContext(f.ctx, check.query, command.CommandID).Scan(&count); err != nil || count != check.want {
			t.Fatalf("durable set mismatch: want %d got %d, %v", check.want, count, err)
		}
	}
	if messages == 1 {
		var media, content string
		var seq uint64
		if err := f.pool.QueryRowContext(f.ctx, "SELECT media,content,seq FROM message WHERE id=?", command.CommandID).Scan(&media, &content, &seq); err != nil ||
			media != string(command.Media) || content != command.Content || seq == 0 {
			t.Fatal("stored message changed canonical content/media/seq", err)
		}
	}
}

func (f *persistenceFixture) assertQueueEmpty(t *testing.T) {
	t.Helper()
	// All consumer streams have been synchronously closed first. A successful
	// local ACK alone cannot justify this assertion.
	if _, ok, err := f.control.Get(rabbitmq.PersistQueue, false); err != nil || ok {
		t.Fatal("MQ responsibility was not released", err)
	}
}

func TestPersistenceConsumerIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	f := newPersistenceFixture(t, ctx)
	run := func(name string, test func(*testing.T)) {
		t.Helper()
		if !t.Run(name, test) {
			t.FailNow() // A retained delivery would contaminate subsequent cases.
		}
	}
	run("persist_media_replay_and_conflict", func(t *testing.T) {
		for i, media := range []messaging.Media{messaging.Text, messaging.Picture, messaging.Audio} {
			command := f.command(t, i+1, messaging.Private, media)
			f.publish(t, command)
			stream := f.open(t)
			result, err := f.handler.Handle(ctx, f.next(t, stream))
			if err != nil || result.Disposition != database.CommandApplied || result.Result.State != database.CommandPersisted {
				t.Fatal("fresh command did not persist", err)
			}
			f.publish(t, command)
			result, err = f.handler.Handle(ctx, f.next(t, stream))
			if err != nil || result.Disposition != database.CommandReplayed {
				t.Fatal("duplicate did not reuse terminal result", err)
			}
			conflict := command
			conflict.TargetType, conflict.TargetID = messaging.Group, strconv.FormatUint(uint64(f.group.ID), 10)
			conflict, err = messaging.Normalize(uint64(f.users[0].ID), messaging.SendRequest{ClientMessageID: command.ClientMessageID,
				TargetType: conflict.TargetType, TargetID: conflict.TargetID, Media: media, Content: command.Content})
			if err != nil {
				t.Fatal(err)
			}
			f.publish(t, conflict)
			result, err = f.handler.Handle(ctx, f.next(t, stream))
			if err != nil || result.Disposition != database.CommandConflict || result.Result != nil {
				t.Fatal("different target did not remain a conflict", err)
			}
			f.close(t, stream)
			f.assertStored(t, command, 1, 2)
		}
		group := f.command(t, 4, messaging.Group, messaging.Text)
		f.publish(t, group)
		stream := f.open(t)
		if _, err := f.handler.Handle(ctx, f.next(t, stream)); err != nil {
			t.Fatal(err)
		}
		f.close(t, stream)
		f.assertStored(t, group, 1, 2)
		f.assertQueueEmpty(t)
	})
	run("permission_changed_after_acceptance", func(t *testing.T) {
		command := f.command(t, 5, messaging.Group, messaging.Text)
		f.publish(t, command)
		if _, err := f.pool.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE owner_id=? AND target_id=? AND type=?",
			f.users[0].ID, f.group.ID, models.GroupRelation); err != nil {
			t.Fatal(err)
		}
		stream := f.open(t)
		result, err := f.handler.Handle(ctx, f.next(t, stream))
		if err != nil || result.Result == nil || result.Result.State != database.CommandRejected {
			t.Fatal("permission failure was not a durable business rejection", err)
		}
		f.assertStored(t, command, 0, 1)
		// The earlier successful command is replayed despite current membership.
		original := f.command(t, 4, messaging.Group, messaging.Text)
		f.publish(t, original)
		result, err = f.handler.Handle(ctx, f.next(t, stream))
		if err != nil || result.Disposition != database.CommandReplayed || result.Result.State != database.CommandPersisted {
			t.Fatal("replay incorrectly reauthorized an existing terminal result", err)
		}
		f.close(t, stream)
		f.assertStored(t, original, 1, 2)
		f.assertQueueEmpty(t)
	})
	run("competing_consumers_same_identity", func(t *testing.T) {
		command := f.command(t, 6, messaging.Private, messaging.Text)
		f.publish(t, command)
		f.publish(t, command)
		a, b := f.open(t), f.open(t)
		da, db := f.next(t, a), f.next(t, b)
		type outcome struct {
			execution database.CommandExecution
			err       error
		}
		done := make(chan outcome, 2)
		for _, d := range []*rabbitmq.ConsumerDelivery{da, db} {
			go func() { e, err := f.handler.Handle(ctx, d); done <- outcome{e, err} }()
		}
		seen := map[database.CommandDisposition]int{}
		for i := 0; i < 2; i++ {
			select {
			case got := <-done:
				if got.err != nil {
					t.Fatal(got.err)
				}
				seen[got.execution.Disposition]++
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		if seen[database.CommandApplied] != 1 || seen[database.CommandReplayed] != 1 {
			t.Fatal("competing deliveries did not share one terminal result")
		}
		f.close(t, a)
		f.close(t, b)
		f.assertStored(t, command, 1, 2)
		f.assertQueueEmpty(t)
	})
	for i, stage := range []string{"before_commit", "after_commit"} {
		run("kill_worker_"+stage, func(t *testing.T) {
			command := f.command(t, 10+i, messaging.Private, messaging.Text)
			receipt := f.publish(t, command)
			kill := f.crashWorker(t, stage)
			var count int
			if err := f.pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message WHERE id=?", command.CommandID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != i {
				t.Fatal("worker barrier did not match actual commit visibility")
			}
			kill()
			stream := f.open(t)
			delivery := f.next(t, stream)
			if !delivery.Message.Redelivered || delivery.Message.CorrelationID != receipt.PublishAttemptID {
				t.Fatal("crash did not preserve the original physical attempt")
			}
			result, err := f.handler.Handle(ctx, delivery)
			want := database.CommandApplied
			if stage == "after_commit" {
				want = database.CommandReplayed
			}
			if err != nil || result.Disposition != want {
				t.Fatal("crashed worker did not recover through idempotency", err)
			}
			f.close(t, stream)
			f.assertStored(t, command, 1, 2)
			f.assertQueueEmpty(t)
		})
	}
	run("commit_response_error_retains_delivery", func(t *testing.T) {
		command := f.command(t, 12, messaging.Private, messaging.Text)
		f.publish(t, command)
		connector, err := mysql.NewConnector(mustParsePersistenceDSN(t, f.dsn))
		if err != nil {
			t.Fatal(err)
		}
		// Real COMMIT succeeds; only its caller-visible result is replaced. This
		// is an error injection, not a claim of a real TCP response-loss test.
		pool := sql.OpenDB(persistenceCommitConnector{base: connector, after: func(err error) error {
			if err == nil {
				return io.ErrUnexpectedEOF
			}
			return err
		}})
		defer pool.Close()
		repository, err := database.NewMessageRepository(pool)
		if err != nil {
			t.Fatal(err)
		}
		handler, err := NewPersistenceHandler(repository)
		if err != nil {
			t.Fatal(err)
		}
		old := f.open(t)
		execution, err := handler.Handle(ctx, f.next(t, old))
		requirePersistenceFailure(t, execution, err, CommitUnknown)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal("commit diagnostic lost")
		}
		f.close(t, old)
		f.assertStored(t, command, 1, 2)
		fresh := f.open(t)
		delivery := f.next(t, fresh)
		if !delivery.Message.Redelivered {
			t.Fatal("unknown commit delivery was ACKed")
		}
		result, err := f.handler.Handle(ctx, delivery)
		if err != nil || result.Disposition != database.CommandReplayed {
			t.Fatal("unknown commit did not recover original result", err)
		}
		f.close(t, fresh)
		f.assertStored(t, command, 1, 2)
		f.assertQueueEmpty(t)
	})
	run("database_offline_preserves_original", func(t *testing.T) {
		command := f.command(t, 13, messaging.Private, messaging.Text)
		receipt := f.publish(t, command)
		stream := f.open(t)
		delivery := f.next(t, stream)
		if err := f.env.SetMySQLRunning(ctx, false); err != nil {
			t.Fatal(err)
		}
		operation, stop := context.WithTimeout(ctx, 2*time.Second)
		execution, err := f.handler.Handle(operation, delivery)
		stop()
		requirePersistenceFailure(t, execution, err, PersistenceFailed)
		f.close(t, stream)
		if err := f.env.SetMySQLRunning(ctx, true); err != nil {
			t.Fatal(err)
		}
		// Docker may assign a new published port when restarting this fixture.
		conf, err := f.env.Config()
		if err != nil {
			t.Fatal(err)
		}
		dsn := mustParsePersistenceDSN(t, f.dsn)
		dsn.Addr = net.JoinHostPort(conf.MYSQL.Host, conf.MYSQL.Port)
		pool, err := sql.Open("mysql", dsn.FormatDSN())
		if err != nil {
			t.Fatal(err)
		}
		f.cleanup(func() { pool.Close() })
		ready, readyCancel := context.WithTimeout(ctx, 30*time.Second)
		defer readyCancel()
		for pool.PingContext(ready) != nil {
			select {
			case <-ready.Done():
				t.Fatal("owned database did not recover")
			case <-time.After(200 * time.Millisecond):
			}
		}
		repository, err := database.NewMessageRepository(pool)
		if err != nil {
			t.Fatal(err)
		}
		handler, err := NewPersistenceHandler(repository)
		if err != nil {
			t.Fatal(err)
		}
		fresh := f.open(t)
		retry := f.next(t, fresh)
		if !retry.Message.Redelivered || retry.Message.CorrelationID != receipt.PublishAttemptID {
			t.Fatal("database failure lost the original attempt")
		}
		result, err := handler.Handle(ctx, retry)
		if err != nil || result.Disposition != database.CommandApplied {
			t.Fatal("database recovery failed", err)
		}
		f.close(t, fresh)
		f.pool = pool
		f.assertStored(t, command, 1, 2)
		f.assertQueueEmpty(t)
	})
	run("malformed_delivery_keeps_body_and_headers", func(t *testing.T) {
		if err := f.control.Confirm(false); err != nil {
			t.Fatal(err)
		}
		confirms := f.control.NotifyPublish(make(chan amqp.Confirmation, 1))
		raw := []byte("unparseable original body 中")
		if err := f.control.PublishWithContext(ctx, rabbitmq.CommandsExchange, rabbitmq.SubmitRoutingKey, true, false,
			amqp.Publishing{ContentType: "application/json", DeliveryMode: 2, Body: raw, Headers: amqp.Table{"audit": "original-header"}}); err != nil {
			t.Fatal(err)
		}
		select {
		case confirm := <-confirms:
			if !confirm.Ack {
				t.Fatal("fixture malformed publish not confirmed")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		old := f.open(t)
		execution, err := f.handler.Handle(ctx, f.next(t, old))
		requirePersistenceFailure(t, execution, err, InvalidDelivery)
		f.close(t, old)
		fresh := f.open(t)
		retry := f.next(t, fresh)
		if !retry.Message.Redelivered || !bytes.Equal(retry.Message.Body, raw) || retry.Message.Headers["audit"] != "original-header" {
			t.Fatal("malformed delivery lost original recovery data")
		}
		f.close(t, fresh) // No fixture ACK masquerading as isolation/audit storage.
	})
	var gaps int
	if err := f.pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation c WHERE
		c.last_seq<>(SELECT COUNT(*) FROM message m WHERE m.conversation_id=c.id) OR
		c.last_seq<>(SELECT COALESCE(MAX(m.seq),0) FROM message m WHERE m.conversation_id=c.id)`).Scan(&gaps); err != nil || gaps != 0 {
		t.Fatal("sequence range has a hole or duplicate", err)
	}
}

type persistenceHelperConfig struct {
	DSN, AMQPURL, ManagementURL, Password, VHost, Stage string
}

func (f *persistenceFixture) crashWorker(t *testing.T, stage string) func() {
	t.Helper()
	conf, err := f.env.Config()
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(persistenceHelperConfig{DSN: f.dsn, AMQPURL: f.broker.AMQPURL(), ManagementURL: "http://" + f.broker.ManagementAddress,
		Password: conf.REDIS.Password, VHost: f.broker.VHost, Stage: stage})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestPersistenceCrashHelper$", "-test.timeout=45s")
	command.Env = append(os.Environ(), "ICHAT_P4_CRASH_HELPER="+string(body))
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done, barrier := make(chan struct{}), make(chan struct{})
	var waitErr error
	go func() { waitErr = command.Wait(); close(done) }()
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "persistence-commit-barrier" {
				close(barrier)
				return
			}
		}
	}()
	kill := func() {
		command.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker process failed to exit after kill")
		}
	}
	t.Cleanup(kill)
	select {
	case <-barrier:
	case <-done:
		t.Fatalf("worker exited before commit barrier: %v", waitErr)
	case <-time.After(20 * time.Second):
		t.Fatal("worker did not reach actual transaction barrier")
	}
	return kill
}

func TestPersistenceCrashHelper(t *testing.T) {
	raw := os.Getenv("ICHAT_P4_CRASH_HELPER")
	if raw == "" {
		t.Skip("subprocess helper")
	}
	var conf persistenceHelperConfig
	if err := json.Unmarshal([]byte(raw), &conf); err != nil {
		t.Fatal("invalid helper configuration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	base, err := mysql.NewConnector(mustParsePersistenceDSN(t, conf.DSN))
	if err != nil {
		t.Fatal("helper connector failed")
	}
	barrier := func() { fmt.Println("persistence-commit-barrier"); <-ctx.Done() }
	connector := persistenceCommitConnector{base: base}
	if conf.Stage == "before_commit" {
		connector.before = barrier
	} else if conf.Stage == "after_commit" {
		connector.after = func(err error) error {
			if err == nil {
				barrier()
			}
			return err
		}
	} else {
		t.Fatal("invalid helper stage")
	}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	repository, err := database.NewMessageRepository(pool)
	if err != nil {
		t.Fatal("helper repository failed")
	}
	handler, err := NewPersistenceHandler(repository)
	if err != nil {
		t.Fatal(err)
	}
	management, err := rabbitmq.NewManagement(conf.ManagementURL, "ichat_test_admin", conf.Password)
	if err != nil {
		t.Fatal("helper management failed")
	}
	session, err := rabbitmq.DialConsumer(ctx, conf.AMQPURL, management, conf.VHost, rabbitmq.DefaultConnectionOptions())
	if err != nil {
		t.Fatal("helper consumer failed")
	}
	defer session.Close(context.Background())
	stream, err := session.OpenConsumer(ctx, rabbitmq.PersistQueue, 1)
	if err != nil {
		t.Fatal("helper stream failed")
	}
	delivery, err := stream.Next(ctx)
	if err != nil {
		t.Fatal("helper delivery failed")
	}
	handler.Handle(ctx, delivery)
	t.Fatal("helper must be killed before completing the handoff")
}

func mustParsePersistenceDSN(t *testing.T, dsn string) *mysql.Config {
	t.Helper()
	conf, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid fixture DSN")
	}
	return conf
}

// These driver wrappers exist only in integration test builds. They interpose
// at a real SQL transaction's Commit, with no production crash-control surface.
type persistenceCommitConnector struct {
	base   driver.Connector
	before func()
	after  func(error) error
}

func (c persistenceCommitConnector) Driver() driver.Driver { return c.base.Driver() }
func (c persistenceCommitConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return persistenceCommitConn{Conn: conn, connector: c}, nil
}

type persistenceCommitConn struct {
	driver.Conn
	connector persistenceCommitConnector
}

func (c persistenceCommitConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return persistenceCommitTx{Tx: tx, connector: c.connector}, nil
}

type persistenceCommitTx struct {
	driver.Tx
	connector persistenceCommitConnector
}

func (t persistenceCommitTx) Commit() error {
	if t.connector.before != nil {
		t.connector.before()
	}
	err := t.Tx.Commit()
	if t.connector.after != nil {
		return t.connector.after(err)
	}
	return err
}
