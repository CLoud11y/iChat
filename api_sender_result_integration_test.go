//go:build integration

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
	amqp "github.com/rabbitmq/amqp091-go"
	"iChat/application"
	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/migrations"
	"iChat/models"
	"iChat/tests/clientsim"
	"iChat/tests/testenv"
	"iChat/utils"
	"iChat/workers"
)

type senderResultReceipt struct {
	entered, release, returned chan struct{}
	once, unblock, exit        sync.Once
	result                     rabbitmq.PublishResult
}

func (b *senderResultReceipt) open() { b.unblock.Do(func() { close(b.release) }) }

type senderResultPublisher struct {
	application.MQPublisher
	mu       sync.Mutex
	barriers map[string]*senderResultReceipt
}

func (p *senderResultPublisher) Publish(ctx context.Context, c messaging.Command, at time.Time) (rabbitmq.PublishResult, error) {
	result, err := p.MQPublisher.Publish(ctx, c, at)
	p.mu.Lock()
	barrier := p.barriers[c.CommandID]
	p.mu.Unlock()
	if err == nil && result.Outcome == rabbitmq.PublishAccepted && barrier != nil {
		barrier.once.Do(func() { barrier.result = result; close(barrier.entered) })
		<-barrier.release
		barrier.exit.Do(func() { close(barrier.returned) })
	}
	return result, err
}

// Shared owned infrastructure and real API/JWT/WebSocket setup. Tests choose
// whether to drive repositories explicitly or start production worker services.
type senderResultFixture struct {
	ctx            context.Context
	pool           *sql.DB
	client         *redis.Client
	users          []models.User
	group          models.Group
	publisher      *senderResultPublisher
	api            *apiService
	conn           *websocket.Conn
	token          string
	channel        *amqp.Channel
	management     *rabbitmq.Management
	address, vhost string
}

func newSenderResultFixture(t *testing.T) *senderResultFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	env, err := testenv.StartWithRabbitMQ(ctx, "docker-compose.test.yml")
	if env != nil {
		t.Cleanup(func() {
			c, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			if err := env.Close(c); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("production sender fixture run=%s", env.RunID())
	conf, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	base := apiTestConfig()
	base.MYSQL, base.REDIS = conf.MYSQL, conf.REDIS
	base.RABBITMQ.PublishTimeout = 2 * time.Second
	db, err := utils.OpenMySQL(base)
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
		t.Fatal("refusing writes outside owned schema")
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).AutoMigrate(&models.User{}, &models.Group{}, &models.Relation{}); err != nil {
		t.Fatal(err)
	}
	users := make([]models.User, 3)
	for i := range users {
		v := fmt.Sprintf("%s-%d", env.RunID()[:24], i)
		users[i] = models.User{Identifier: v, Phone: v, Name: "sender-result-fixture"}
	}
	if err := db.WithContext(ctx).Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	group := models.Group{Name: "sender-result-group", OwnerId: users[0].ID, MaxUser: 50}
	if err := db.WithContext(ctx).Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	relations := []models.Relation{{OwnerId: users[0].ID, TargetId: users[1].ID, Type: models.FriendRelation}, {OwnerId: users[1].ID, TargetId: users[0].ID, Type: models.FriendRelation}, {OwnerId: users[0].ID, TargetId: group.ID, Type: models.GroupRelation}}
	if err := db.WithContext(ctx).Create(&relations).Error; err != nil {
		t.Fatal(err)
	}
	client, err := utils.OpenRedis(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	tokens, err := utils.NewTokenService(base, utils.NewRedisTokenBlacklist(client))
	if err != nil {
		t.Fatal(err)
	}
	token, err := tokens.GenerateToken(users[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := env.RabbitMQ()
	if err != nil {
		t.Fatal(err)
	}
	management, err := rabbitmq.NewManagement("http://"+broker.ManagementAddress, "ichat_test_admin", conf.REDIS.Password)
	if err != nil {
		t.Fatal(err)
	}
	control, err := amqp.DialConfig(broker.AMQPURL(), amqp.Config{Heartbeat: time.Second, Dial: func(network, address string) (net.Conn, error) {
		c, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, address)
		if err == nil {
			deadline, _ := ctx.Deadline()
			c.SetDeadline(deadline)
		}
		return c, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	channel, err := control.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := rabbitmq.EnsureLocal(ctx, channel, management, broker.VHost); err != nil {
		t.Fatal(err)
	}
	pubOptions := rabbitmq.DefaultSupervisorOptions()
	pubOptions.Session.Publisher.Slots = 1
	pubOptions.Session.OperationTimeout = 2 * time.Second
	supervisor, err := rabbitmq.StartSupervisor(ctx, broker.AMQPURL(), management, broker.VHost, pubOptions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := supervisor.Close(c); err != nil {
			t.Error(err)
		}
	})
	ready, stop := context.WithTimeout(ctx, 20*time.Second)
	_, err = supervisor.WaitReady(ready)
	stop()
	if err != nil {
		t.Fatal(err)
	}
	publisher := &senderResultPublisher{MQPublisher: supervisor, barriers: make(map[string]*senderResultReceipt)}
	o := defaultAPIOptions(base)
	o.Address = "127.0.0.1:0"
	api, err := newAPIService(base, application.RoleDependencies{Publisher: publisher, Health: apiTestHealth{}}, o, defaultAPIDependencies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := api.Close(c); err != nil {
			t.Error(err)
		}
	})
	setup, stop := context.WithTimeout(ctx, 5*time.Second)
	err = api.Start(setup, ctx)
	stop()
	if err != nil {
		t.Fatal(err)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second, Subprotocols: []string{"ichat.v1"}}
	conn, response, err := dialer.Dial("ws://"+apiAddress(api)+"/auth/chat", http.Header{"Authorization": []string{"Bearer " + token}})
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetReadLimit(messaging.MaxBodyBytes)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var hello struct {
		Type string `json:"type"`
		Data struct {
			Version int    `json:"protocolVersion"`
			Session string `json:"sessionId"`
		} `json:"data"`
	}
	if err := conn.ReadJSON(&hello); err != nil || hello.Type != "ready" || hello.Data.Version != 1 || len(hello.Data.Session) != 32 || conn.Subprotocol() != "ichat.v1" {
		t.Fatal("invalid production ready", err)
	}
	return &senderResultFixture{ctx: ctx, pool: pool, client: client, users: users, group: group,
		publisher: publisher, api: api, conn: conn, token: token, channel: channel,
		management: management, address: broker.AMQPURL(), vhost: broker.VHost}
}

// Repositories and Outbox are explicitly driven in this suite.
func TestProductionSenderCommandResultIntegration(t *testing.T) {
	f := newSenderResultFixture(t)
	ctx, pool, client, users, group := f.ctx, f.pool, f.client, f.users, f.group
	publisher, api, conn, token, channel := f.publisher, f.api, f.conn, f.token, f.channel
	directory := t.TempDir()
	store, err := clientsim.OpenFileStore(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	initialStore := store
	t.Cleanup(func() {
		if err := initialStore.Close(); err != nil {
			t.Error(err)
		}
	})
	sender, err := clientsim.OpenSender(ctx, uint64(users[0].ID), store)
	if err != nil {
		t.Fatal(err)
	}
	httpOptions := clientsim.DefaultHTTPOptions("http://"+apiAddress(api), func(context.Context) (string, error) { return token, nil })
	httpOptions.MaxConcurrent = 1
	runner, err := clientsim.NewHTTPRunner(sender, httpOptions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runner.Close)
	repo, _ := database.NewMessageRepository(pool)
	outbox, _ := database.NewOutboxRepository(pool)
	notifications, _ := workers.NewRedisNotificationPublisher(client)
	loop, err := workers.NewOutboxLoop(outbox, notifications, workers.DefaultOutboxServiceOptions().Loop)
	if err != nil {
		t.Fatal(err)
	}
	original := map[string]messaging.CommandStatus{}
	local := func(t *testing.T, key string) clientsim.PendingEntry {
		t.Helper()
		entries, err := sender.Entries(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.ClientMessageID == key {
				return entry
			}
		}
		t.Fatal("local command absent")
		return clientsim.PendingEntry{}
	}
	readResult := func(t *testing.T, key string) []byte {
		t.Helper()
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			var frame struct {
				Type string          `json:"type"`
				Data json.RawMessage `json:"data"`
			}
			if json.Unmarshal(body, &frame) != nil {
				t.Fatal("invalid production frame")
			}
			switch frame.Type {
			case "ping":
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"pong","data":{}}`)); err != nil {
					t.Fatal(err)
				}
			case "sync.required", "message":
			case "command.result":
				value, err := messaging.DecodeCommandResult(body, uint64(users[0].ID))
				if err != nil || value.ClientMessageID != key {
					t.Fatal("unexpected production result", err)
				}
				return body
			default:
				t.Fatal("unexpected production frame type", frame.Type)
			}
		}
	}
	for i, mode := range []string{"persisted_before_HTTP", "rejected_before_HTTP", "reference_after_group_leave", "result_after_HTTP_timeout"} {
		if !t.Run(mode, func(t *testing.T) {
			target, kind := users[1].ID, messaging.Private
			if i == 1 {
				target = users[2].ID
			}
			if i == 2 {
				target = group.ID
				kind = messaging.Group
			}
			request := messaging.SendRequest{ClientMessageID: fmt.Sprintf("ABCD1234-1234-4234-8234-%012d", i+1), TargetType: kind, TargetID: "000" + strconv.FormatUint(uint64(target), 10), Media: messaging.Text, Content: fmt.Sprintf("sender-result-%d \r\n中🙂\x00", i), ClientSentAt: "2026-11-01T12:34:56.123456789+11:00"}
			entry, err := sender.Queue(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			command, err := messaging.Normalize(uint64(users[0].ID), request)
			if err != nil {
				t.Fatal(err)
			}
			barrier := &senderResultReceipt{entered: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
			publisher.mu.Lock()
			publisher.barriers[command.CommandID] = barrier
			publisher.mu.Unlock()
			// Release this scenario before outer API/resource cleanup.
			t.Cleanup(barrier.open)
			defer barrier.open()
			call := ctx
			var cancelCall context.CancelFunc
			if i == 3 {
				call, cancelCall = context.WithTimeout(ctx, 150*time.Millisecond)
				defer cancelCall()
			}
			done := make(chan error, 1)
			go func() { done <- runner.Submit(call, entry.ClientMessageID) }()
			select {
			case <-barrier.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("real broker confirmation not observed")
			}
			if i == 3 {
				select {
				case err := <-done:
					if err == nil || local(t, entry.ClientMessageID).State != clientsim.PublishUnknown {
						t.Fatal("lost HTTP result not retained", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("HTTP timeout did not settle")
				}
			}
			delivery, ok, err := channel.Get(rabbitmq.PersistQueue, false)
			if err != nil || !ok {
				t.Fatal("confirmed command absent from MQ", err)
			}
			envelope, err := messaging.DecodeEnvelope(delivery.Body)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := messaging.ValidateEnvelope(envelope)
			if err != nil || decoded != command || envelope.PublishAttemptID != barrier.result.PublishAttemptID {
				t.Fatal("real MQ envelope changed local request", err)
			}
			if _, err := repo.PersistCommand(ctx, decoded); err != nil {
				t.Fatal(err)
			}
			status, err := repo.GetCommandStatus(ctx, uint64(users[0].ID), command.ClientMessageID, command.PayloadHash)
			if err != nil {
				t.Fatal(err)
			}
			original[command.CommandID] = status
			// Fixture control ACK occurs only after the explicit repository call.
			// This does not claim a production consumer handled the delivery.
			if err := delivery.Ack(false); err != nil {
				t.Fatal(err)
			}
			if i == 2 {
				if _, err := pool.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE owner_id=? AND target_id=? AND type=?", users[0].ID, group.ID, models.GroupRelation); err != nil {
					t.Fatal(err)
				}
			}
			batch, err := loop.RunBatch(ctx)
			want := 2
			if i == 1 {
				want = 1
			}
			if err != nil || batch.Completed != want {
				t.Fatal("real Outbox notification failed", batch, err)
			}
			body := readResult(t, entry.ClientMessageID)
			value, err := messaging.DecodeCommandResult(body, uint64(users[0].ID))
			if err != nil {
				t.Fatal(err)
			}
			if i == 2 && value.Message != nil {
				t.Fatal("production group permission did not redact message")
			}
			needsQuery, err := runner.ObserveCommandResult(ctx, body)
			if err != nil || needsQuery != (i == 2) {
				t.Fatal("unexpected notification decision", err)
			}
			if i != 2 {
				got := local(t, entry.ClientMessageID)
				if got.State != clientsim.SendState(status.State) || !reflect.DeepEqual(got.Result, &status) || got.InFlight != (i != 3) {
					t.Fatal("notification did not persist before HTTP returned")
				}
				if needed, err := runner.ObserveCommandResult(ctx, body); err != nil || needed {
					t.Fatal("duplicate real notification changed result", err)
				}
			} else {
				if got := local(t, entry.ClientMessageID); got.State != clientsim.Submitting || got.Result != nil {
					t.Fatal("reference fabricated full terminal")
				}
			}
			if i != 3 {
				select {
				case <-done:
					t.Fatal("HTTP receipt returned before barrier release")
				default:
				}
			}
			barrier.open()
			select {
			case <-barrier.returned:
			case <-time.After(3 * time.Second):
				t.Fatal("API publisher callback not joined")
			}
			if i != 3 {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("real HTTP acceptance did not return")
				}
			}
			if i == 2 {
				if err := runner.Poll(ctx, entry.ClientMessageID); err != nil {
					t.Fatal(err)
				}
			}
			got := local(t, entry.ClientMessageID)
			if got.State != clientsim.SendState(status.State) || got.InFlight || got.Attempts != 1 || got.Request != request || !reflect.DeepEqual(got.Result, &status) {
				t.Fatal("late acceptance or status changed terminal")
			}
		}) {
			t.FailNow()
		}
	}
	runner.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = clientsim.OpenFileStore(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	finalStore := store
	t.Cleanup(func() {
		if err := finalStore.Close(); err != nil {
			t.Error(err)
		}
	})
	sender, err = clientsim.OpenSender(ctx, uint64(users[0].ID), store)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := sender.Entries(ctx)
	if err != nil || len(entries) != 4 {
		t.Fatal("disk terminal collection changed", err)
	}
	for _, entry := range entries {
		status, err := repo.GetCommandStatus(ctx, uint64(users[0].ID), entry.ClientMessageID, entry.PayloadHash)
		if err != nil || !reflect.DeepEqual(entry.Result, &status) || !reflect.DeepEqual(status, original[entry.CommandID]) {
			t.Fatal("disk/SQL terminal differs", err)
		}
	}
	for table, want := range map[string]int{"message_command": 4, "message": 3, "conversation": 2, "message_outbox": 7, "message_delivery_cursor": 0, "message_quarantine": 0} {
		var n int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != want {
			t.Fatal("wrong final collection", table, n, err)
		}
	}
	var completed int
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_outbox WHERE status='completed'").Scan(&completed); err != nil || completed != 7 {
		t.Fatal("Outbox not completed", completed, err)
	}
	if _, ok, err := channel.Get(rabbitmq.PersistQueue, true); err != nil || ok {
		t.Fatal("fixture MQ queue not empty", err)
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal("schema fingerprint changed", err)
	}
	conn.Close()
	if err := api.Close(ctx); err != nil {
		t.Fatal(err)
	}
	apiWait(t, api.Done())
	if _, err := runner.ObserveCommandResult(ctx, []byte(`{}`)); !errors.Is(err, clientsim.ErrHTTPClosed) {
		t.Fatal("closed runner accepted late notification", err)
	}
	if err := pool.PingContext(ctx); err != nil {
		t.Fatal("API closed borrowed fixture SQL", err)
	}
	t.Log("final 4 commands / 3 messages / 2 conversations / 7 completed Outbox / 0 ACK cursors / 0 quarantine; 4 durable local terminal records")
}
