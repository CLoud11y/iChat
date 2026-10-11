//go:build integration

package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"iChat/messaging"
	"iChat/migrations"
	"iChat/models"
	"iChat/tests/testenv"
	"iChat/utils"
	"math"
	"net"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

func TestReliableMessagePersistenceIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, err := testenv.Start(ctx, filepath.Join("..", "docker-compose.test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("reliable persistence fixture run=%s", env.RunID())
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := env.Close(cleanupCtx); err != nil {
			t.Error(err)
		}
	})
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
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	var schema string
	if err := pool.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); err != nil || schema != "ichat_test_"+env.RunID() {
		t.Fatalf("refusing writes in schema %q: %v", schema, err)
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// Identity fixtures only. Never use AutoMigrate for the reliable message table.
	if err := db.WithContext(ctx).AutoMigrate(&models.User{}, &models.Group{}, &models.Relation{}); err != nil {
		t.Fatal(err)
	}
	users := make([]models.User, 12)
	for i := range users {
		users[i].Phone = fmt.Sprintf("%s-%d", env.RunID()[:24], i)
		users[i].Identifier = users[i].Phone
		users[i].Name = fmt.Sprintf("persistence-user-%d", i)
		if err := db.WithContext(ctx).Create(&users[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	friendWriter, groupWriter := NewRelationManager(db), NewGroupManager(db)
	for _, pair := range [][2]int{{0, 1}, {2, 3}, {4, 5}, {6, 7}, {8, 9}} {
		if err := friendWriter.AddFriendByPhone(ctx, users[pair[0]].ID, users[pair[1]].Phone); err != nil {
			t.Fatal(err)
		}
	}
	if err := groupWriter.CreateGroup(ctx, "persistence-group", users[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	var group models.Group
	if err := db.WithContext(ctx).Where("name=?", "persistence-group").Take(&group).Error; err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{1, 2, 3} {
		if err := groupWriter.JoinGroup(ctx, users[i].ID, group.ID); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := NewMessageRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	options := mysqlDriver.NewConfig()
	options.User, options.Passwd, options.DBName = conf.MYSQL.User, conf.MYSQL.Password, conf.MYSQL.DbName
	options.Net, options.Addr = "tcp", net.JoinHostPort(conf.MYSQL.Host, conf.MYSQL.Port)
	options.ParseTime, options.ClientFoundRows, options.InterpolateParams = true, true, true
	options.Loc = time.FixedZone("test-offset", 13*60*60)
	options.Params = map[string]string{"charset": "utf8mb4", "time_zone": "'+05:30'"}
	connector, err := mysqlDriver.NewConnector(options)
	if err != nil {
		t.Fatal(err)
	}
	otherPool := sql.OpenDB(connector)
	t.Cleanup(func() {
		if err := otherPool.Close(); err != nil {
			t.Error(err)
		}
	})
	otherRepo, err := NewMessageRepository(otherPool)
	if err != nil {
		t.Fatal(err)
	}
	command := func(sender int, key uint64, target uint, kind messaging.TargetType) messaging.Command {
		t.Helper()
		cmd, err := messaging.Normalize(uint64(users[sender].ID), messaging.SendRequest{
			ClientMessageID: fmt.Sprintf("00000000-0000-4000-8000-%012x", key),
			TargetType:      kind, TargetID: strconv.FormatUint(uint64(target), 10), Media: messaging.Text, Content: "  你好🙂\r\n\x00e\u0301  ",
		})
		if err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	apply := func(t *testing.T, repository *MessageRepository, cmd messaging.Command) CommandExecution {
		t.Helper()
		outcome, err := repository.PersistCommand(ctx, cmd)
		if err != nil || outcome.Disposition != CommandApplied || outcome.Result == nil || outcome.Result.State != CommandPersisted {
			t.Fatalf("persist command=%+v err=%v", outcome, err)
		}
		return outcome
	}

	t.Run("canonical_message_events_and_metadata_replay", func(t *testing.T) {
		for i, metadata := range []struct {
			media        messaging.Media
			sent, stored string
		}{
			{messaging.Text, "2026-10-10T14:02:03.999999999Z", "2026-10-10 14:02:03.999999"},
			{messaging.Picture, "", ""},
			{messaging.Audio, "9999-12-31T23:59:59.999999999Z", "9999-12-31 23:59:59.999999"},
			{messaging.Text, "0001-01-01T00:00:00.123456789Z", "0001-01-01 00:00:00.123456"},
		} {
			cmd := command(0, uint64(1+i), users[1].ID, messaging.Private)
			cmd.Media, cmd.ClientSentAt = metadata.media, metadata.sent
			cmd, err = messaging.Normalize(uint64(users[0].ID), messaging.SendRequest{ClientMessageID: cmd.ClientMessageID, TargetType: cmd.TargetType, TargetID: cmd.TargetID, Media: cmd.Media, Content: cmd.Content, ClientSentAt: cmd.ClientSentAt})
			if err != nil {
				t.Fatal(err)
			}
			outcome := apply(t, otherRepo, cmd)
			message := persistenceReadMessage(t, ctx, pool, cmd)
			if uint64(message.Seq) != uint64(i+1) {
				t.Fatalf("seq=%s want=%d", message.Seq, i+1)
			}
			persistenceCheckEvents(t, ctx, pool, cmd, outcome, &message)
			var clientStamp sql.NullString
			if err := pool.QueryRowContext(ctx, "SELECT DATE_FORMAT(client_sent_at,'%Y-%m-%d %H:%i:%s.%f') FROM message WHERE id=?", cmd.CommandID).Scan(&clientStamp); err != nil || clientStamp.String != metadata.stored || clientStamp.Valid != (metadata.sent != "") {
				t.Fatalf("client time=%+v err=%v want=%s", clientStamp, err, metadata.stored)
			}
			if parsed, err := time.Parse(time.RFC3339Nano, message.ServerCreatedAt); err != nil || parsed.Location() != time.UTC || parsed.Nanosecond()%1000 != 0 || parsed.Before(outcome.Result.CreatedAt) || parsed.After(outcome.Result.CompletedAt) {
				t.Fatalf("message timestamp=%s result=%+v err=%v", message.ServerCreatedAt, outcome.Result, err)
			}
			cmd.ClientSentAt = "2030-01-01T00:00:00Z"
			replay, err := repo.PersistCommand(ctx, cmd)
			if err != nil || replay.Disposition != CommandReplayed || !reflect.DeepEqual(replay.Result, outcome.Result) {
				t.Fatalf("replay=%+v err=%v", replay, err)
			}
			if got := persistenceReadMessage(t, ctx, pool, cmd); !reflect.DeepEqual(got, message) {
				t.Fatalf("retry changed message: %+v", got)
			}
			persistenceCheckEvents(t, ctx, pool, cmd, replay, &message)
		}
		persistenceCheckSequence(t, ctx, pool, command(0, 1, users[1].ID, messaging.Private).ConversationID, 4)
	})

	t.Run("rejected_commands_only_have_result_event", func(t *testing.T) {
		for i, tc := range []struct {
			sender int
			target uint
			kind   messaging.TargetType
			reason string
		}{
			{10, users[0].ID, messaging.Private, ReasonNotFriends},
			{10, group.ID, messaging.Group, ReasonNotGroupMember},
			{0, users[0].ID, messaging.Private, ReasonSelfSend},
			{0, 999999, messaging.Group, ReasonTargetNotFound},
		} {
			cmd := command(tc.sender, uint64(20+i), tc.target, tc.kind)
			outcome, err := repo.PersistCommand(ctx, cmd)
			if err != nil || outcome.Disposition != CommandApplied || outcome.Result == nil || outcome.Result.State != CommandRejected || outcome.Result.ReasonCode != tc.reason {
				t.Fatalf("reject=%+v err=%v", outcome, err)
			}
			persistenceCheckEvents(t, ctx, pool, cmd, outcome, nil)
			commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message WHERE id=?", 0, cmd.CommandID)
			commandCount(t, ctx, pool, "SELECT COUNT(*) FROM conversation WHERE id=?", 0, cmd.ConversationID)
			replay, err := otherRepo.PersistCommand(ctx, cmd)
			if err != nil || replay.Disposition != CommandReplayed || !reflect.DeepEqual(replay.Result, outcome.Result) {
				t.Fatalf("rejected replay=%+v %v", replay, err)
			}
			persistenceCheckEvents(t, ctx, pool, cmd, replay, nil)
		}
		if err := friendWriter.AddFriendByPhone(ctx, users[10].ID, users[0].Phone); err != nil {
			t.Fatal(err)
		}
		original := command(10, 20, users[0].ID, messaging.Private)
		outcome, err := repo.PersistCommand(ctx, original)
		if err != nil || outcome.Result == nil || outcome.Result.State != CommandRejected || outcome.Disposition != CommandReplayed {
			t.Fatalf("permission restore changed rejection: %+v %v", outcome, err)
		}
		apply(t, repo, command(10, 24, users[0].ID, messaging.Private))
	})

	t.Run("concurrent_same_key_and_content_conflict", func(t *testing.T) {
		for _, conflict := range []bool{false, true} {
			key := uint64(30)
			if conflict {
				key++
			}
			commands := make([]messaging.Command, 16)
			for i := range commands {
				commands[i] = command(2, key, users[3].ID, messaging.Private)
				if conflict && i%2 == 1 {
					request := messaging.SendRequest{ClientMessageID: commands[i].ClientMessageID, TargetType: messaging.Private, TargetID: commands[i].TargetID, Media: messaging.Text, Content: "different"}
					commands[i], err = messaging.Normalize(uint64(users[2].ID), request)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			outcomes := persistenceCompete(t, ctx, []*MessageRepository{repo, otherRepo}, commands)
			applied, replayed, conflicts := 0, 0, 0
			for _, outcome := range outcomes {
				switch outcome.Disposition {
				case CommandApplied:
					applied++
				case CommandReplayed:
					replayed++
				case CommandConflict:
					conflicts++
				}
			}
			wantReplay, wantConflict := 15, 0
			if conflict {
				wantReplay, wantConflict = 7, 8
			}
			if applied != 1 || replayed != wantReplay || conflicts != wantConflict {
				t.Fatalf("competition applied=%d replay=%d conflict=%d", applied, replayed, conflicts)
			}
			commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message WHERE id=?", 1, commands[0].CommandID)
			commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message_outbox WHERE command_id=?", 2, commands[0].CommandID)
		}
		persistenceCheckSequence(t, ctx, pool, command(2, 30, users[3].ID, messaging.Private).ConversationID, 2)
	})

	t.Run("concurrent_private_directions_and_group_sequences", func(t *testing.T) {
		for _, kind := range []messaging.TargetType{messaging.Private, messaging.Group} {
			commands := make([]messaging.Command, 24)
			for i := range commands {
				if kind == messaging.Private {
					sender, target := 8, 9
					if i%2 == 1 {
						sender, target = target, sender
					}
					commands[i] = command(sender, uint64(100+i), users[target].ID, kind)
				} else {
					commands[i] = command(i%4, uint64(200+i), group.ID, kind)
				}
			}
			for _, outcome := range persistenceCompete(t, ctx, []*MessageRepository{repo, otherRepo}, commands) {
				if outcome.Disposition != CommandApplied || outcome.Result == nil || outcome.Result.State != CommandPersisted {
					t.Fatalf("independent key did not persist: %+v", outcome)
				}
			}
			persistenceCheckSequence(t, ctx, pool, commands[0].ConversationID, 24)
			for _, cmd := range commands {
				message := persistenceReadMessage(t, ctx, pool, cmd)
				persistenceCheckEvents(t, ctx, pool, cmd, CommandExecution{Result: &CommandResult{State: CommandPersisted}}, &message)
			}
		}
	})

	t.Run("each_write_failure_rolls_back_all_records_and_sequence", func(t *testing.T) {
		for i, stage := range []string{"message", "counter", "message_event", "result_event", "terminal"} {
			t.Run(stage, func(t *testing.T) {
				cmd := command(4, uint64(300+i), users[5].ID, messaging.Private)
				faultRepo, fault := persistenceFaultRepository(t, connector, cmd.CommandID, stage)
				outcome, err := faultRepo.PersistCommand(ctx, cmd)
				var sqlErr *mysqlDriver.MySQLError
				wantNumber := uint16(3819)
				if stage == "counter" {
					wantNumber = 1048
				}
				if !fault.faulted || !errors.As(err, &sqlErr) || sqlErr.Number != wantNumber || outcome.Result != nil || outcome.Disposition != "" {
					t.Fatalf("failed %s returned result: %+v err=%v", stage, outcome, err)
				}
				for _, query := range []string{"SELECT COUNT(*) FROM message_command WHERE command_id=?", "SELECT COUNT(*) FROM message WHERE id=?", "SELECT COUNT(*) FROM message_outbox WHERE command_id=?"} {
					commandCount(t, ctx, pool, query, 0, cmd.CommandID)
				}
				if i == 0 {
					commandCount(t, ctx, pool, "SELECT COUNT(*) FROM conversation WHERE id=?", 0, cmd.ConversationID)
				} else {
					persistenceCheckSequence(t, ctx, pool, cmd.ConversationID, uint64(i))
				}
				outcome = apply(t, otherRepo, cmd)
				message := persistenceReadMessage(t, ctx, pool, cmd)
				if uint64(message.Seq) != uint64(i+1) {
					t.Fatalf("rollback consumed sequence: %s", message.Seq)
				}
				persistenceCheckEvents(t, ctx, pool, cmd, outcome, &message)
			})
		}
	})

	t.Run("rejection_event_failure_does_not_commit_terminal", func(t *testing.T) {
		cmd := command(11, 400, group.ID, messaging.Group)
		faultRepo, fault := persistenceFaultRepository(t, connector, cmd.CommandID, "result_event")
		outcome, err := faultRepo.PersistCommand(ctx, cmd)
		if !fault.faulted || err == nil || outcome.Result != nil {
			t.Fatalf("rejection without event committed: %+v %v", outcome, err)
		}
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message_command WHERE command_id=?", 0, cmd.CommandID)
		outcome, err = otherRepo.PersistCommand(ctx, cmd)
		if err != nil || outcome.Disposition != CommandApplied || outcome.Result == nil || outcome.Result.ReasonCode != ReasonNotGroupMember {
			t.Fatalf("reject retry=%+v %v", outcome, err)
		}
		persistenceCheckEvents(t, ctx, pool, cmd, outcome, nil)
		persistenceCheckSequence(t, ctx, pool, cmd.ConversationID, 24)
	})

	t.Run("uncommitted_writes_and_cancellation_never_expose_a_later_sequence", func(t *testing.T) {
		for i, abort := range []bool{false, true} {
			t.Run(fmt.Sprintf("cancel_%t", abort), func(t *testing.T) {
				cmd := command(6, uint64(500+i*2), users[7].ID, messaging.Private)
				next := command(7, uint64(501+i*2), users[6].ID, messaging.Private)
				persistenceBarrierTest(t, ctx, connector, pool, repo, cmd, next, abort, uint64(i*2))
			})
		}
		persistenceCheckSequence(t, ctx, pool, command(6, 500, users[7].ID, messaging.Private).ConversationID, 4)
	})

	t.Run("uint64_sequence_ceiling_is_explicit_and_json_safe", func(t *testing.T) {
		if err := groupWriter.CreateGroup(ctx, "sequence-ceiling", users[6].ID, ""); err != nil {
			t.Fatal(err)
		}
		var ceilingGroup models.Group
		if err := db.WithContext(ctx).Where("name=?", "sequence-ceiling").Take(&ceilingGroup).Error; err != nil {
			t.Fatal(err)
		}
		first := command(6, 600, ceilingGroup.ID, messaging.Group)
		apply(t, repo, first)
		// Deliberate test-only boundary fixture, not an actual sequence history.
		commandExec(t, ctx, pool, "UPDATE conversation SET last_seq=? WHERE id=?", uint64(math.MaxUint64-1), first.ConversationID)
		last := command(6, 601, ceilingGroup.ID, messaging.Group)
		outcome := apply(t, otherRepo, last)
		message := persistenceReadMessage(t, ctx, pool, last)
		if uint64(message.Seq) != math.MaxUint64 {
			t.Fatalf("max sequence=%s", message.Seq)
		}
		persistenceCheckEvents(t, ctx, pool, last, outcome, &message)
		exhausted := command(6, 602, ceilingGroup.ID, messaging.Group)
		outcome, err := repo.PersistCommand(ctx, exhausted)
		if !errors.Is(err, ErrConversationExhausted) || outcome.Result != nil {
			t.Fatalf("sequence overflow=%+v %v", outcome, err)
		}
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message_command WHERE command_id=?", 0, exhausted.CommandID)
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message_outbox WHERE command_id=?", 0, exhausted.CommandID)
	})
	commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message_command WHERE reason_code LIKE '__claim:%' OR state='processing'", 0)
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatalf("persistence changed versioned schema: %v", err)
	}
}

func persistenceReadMessage(t *testing.T, ctx context.Context, pool *sql.DB, command messaging.Command) messaging.MessageDTO {
	t.Helper()
	var message messaging.MessageDTO
	var stamp string
	err := pool.QueryRowContext(ctx, `SELECT id,client_message_id,conversation_id,seq,sender_id,receiver_id,type,media,content,
 DATE_FORMAT(server_created_at,'%Y-%m-%d %H:%i:%s.%f') FROM message WHERE id=?`, command.CommandID).Scan(
		&message.MessageID, &message.ClientMessageID, &message.ConversationID, &message.Seq, &message.SenderID, &message.TargetID,
		&message.TargetType, &message.Media, &message.Content, &stamp)
	if err != nil {
		t.Fatal(err)
	}
	created, err := time.Parse(mysqlMicrosecondLayout, stamp)
	if err != nil {
		t.Fatal(err)
	}
	message.ServerCreatedAt = created.Format(time.RFC3339Nano)
	want, err := messaging.NewMessageDTO(command, message.Seq, created)
	if err != nil || !reflect.DeepEqual(message, want) {
		t.Fatalf("message=%+v want=%+v err=%v", message, want, err)
	}
	return message
}

func persistenceCheckEvents(t *testing.T, ctx context.Context, pool *sql.DB, command messaging.Command, outcome CommandExecution, message *messaging.MessageDTO) {
	t.Helper()
	rows, err := pool.QueryContext(ctx, `SELECT event_id,event_type,conversation_id,message_id,payload,status,attempts,lease_token,lease_until,completed_at
 FROM message_outbox WHERE command_id=? ORDER BY event_type`, command.CommandID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		var id, kind, status string
		var conversationID, messageID, leaseToken, leaseUntil, completedAt sql.NullString
		var payload []byte
		var attempts uint32
		if err := rows.Scan(&id, &kind, &conversationID, &messageID, &payload, &status, &attempts, &leaseToken, &leaseUntil, &completedAt); err != nil {
			t.Fatal(err)
		}
		wantID, err := messaging.OutboxEventID(command.CommandID, messaging.OutboxEventType(kind))
		if err != nil || id != wantID || status != "pending" || attempts != 0 || leaseToken.Valid || leaseUntil.Valid || completedAt.Valid {
			t.Fatalf("outbox id=%s kind=%s status=%s attempts=%d err=%v", id, kind, status, attempts, err)
		}
		if message == nil {
			if conversationID.Valid || messageID.Valid || kind != string(messaging.EventCommandResult) {
				t.Fatal("rejection generated a message event/reference")
			}
		} else if conversationID.String != message.ConversationID || messageID.String != message.MessageID {
			t.Fatal("outbox message reference mismatch")
		}
		switch messaging.OutboxEventType(kind) {
		case messaging.EventCommandResult:
			var result messaging.CommandResultEvent
			if err := json.Unmarshal(payload, &result); err != nil {
				t.Fatal(err)
			}
			want := messaging.CommandResultEvent{SchemaVersion: messaging.SchemaVersion, EventID: id, CommandID: command.CommandID,
				SenderID: command.SenderID, ClientMessageID: command.ClientMessageID, PayloadHash: command.PayloadHash,
				State: string(outcome.Result.State), ReasonCode: outcome.Result.ReasonCode, Message: message}
			if !reflect.DeepEqual(result, want) {
				t.Fatalf("result event=%+v want=%+v", result, want)
			}
		case messaging.EventMessagePersisted:
			var hint messaging.MessagePersistedEvent
			if err := json.Unmarshal(payload, &hint); err != nil {
				t.Fatal(err)
			}
			want := messaging.MessagePersistedEvent{SchemaVersion: messaging.SchemaVersion, EventID: id, CommandID: command.CommandID,
				ConversationID: message.ConversationID, MessageID: message.MessageID, Seq: message.Seq,
				SenderID: message.SenderID, TargetID: message.TargetID, TargetType: message.TargetType}
			if !reflect.DeepEqual(hint, want) || strings.Contains(string(payload), `"content"`) {
				t.Fatalf("message hint=%+v want=%+v", hint, want)
			}
		default:
			t.Fatalf("unknown event %s", kind)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	wantCount := 1
	if message != nil {
		wantCount = 2
	}
	if count != wantCount {
		t.Fatalf("outbox count=%d want=%d", count, wantCount)
	}
}

func persistenceCheckSequence(t *testing.T, ctx context.Context, pool *sql.DB, conversationID string, want uint64) {
	t.Helper()
	var lastSeq uint64
	if err := pool.QueryRowContext(ctx, "SELECT last_seq FROM conversation WHERE id=?", conversationID).Scan(&lastSeq); err != nil || lastSeq != want {
		t.Fatalf("last_seq=%d want=%d err=%v", lastSeq, want, err)
	}
	rows, err := pool.QueryContext(ctx, "SELECT seq FROM message WHERE conversation_id=? ORDER BY seq", conversationID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var count uint64
	for rows.Next() {
		var seq uint64
		if err := rows.Scan(&seq); err != nil {
			t.Fatal(err)
		}
		count++
		if seq != count {
			t.Fatalf("sequence gap: got=%d next=%d", seq, count)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("message count=%d highWatermark=%d", count, want)
	}
}

func persistenceCompete(t *testing.T, ctx context.Context, repositories []*MessageRepository, commands []messaging.Command) []CommandExecution {
	t.Helper()
	outcomes, failures := make([]CommandExecution, len(commands)), make([]error, len(commands))
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := range commands {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			<-start
			outcomes[i], failures[i] = repositories[i%len(repositories)].PersistCommand(ctx, commands[i])
		}(i)
	}
	close(start)
	workers.Wait()
	for i, err := range failures {
		if err != nil {
			t.Fatalf("competition request %d failed: %v", i, err)
		}
		if outcomes[i].Disposition == CommandConflict && outcomes[i].Result != nil {
			t.Fatal("conflict returned canonical successful result")
		}
	}
	return outcomes
}

func persistenceFaultRepository(t *testing.T, connector driver.Connector, commandID, stage string) (*MessageRepository, *persistenceBarrierConnector) {
	t.Helper()
	fault := &persistenceBarrierConnector{inner: connector, commandID: commandID, faultStage: stage}
	pool := sql.OpenDB(fault)
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	repo, err := NewMessageRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	return repo, fault
}

// This adapter delegates every SQL operation to the real MySQL driver. The
// barrier pauses after both Outbox INSERTs, before terminal UPDATE/Commit. The
// fault mode changes a bound value so MySQL itself returns a CHECK/NOT NULL
// error; it never changes the schema or substitutes an in-memory SQL result.
// InterpolateParams is enabled on this test connector so ExecContext performs
// the actual write rather than returning driver.ErrSkip for a prepared fallback.
type persistenceBarrierConnector struct {
	inner            driver.Connector
	commandID        string
	faultStage       string
	faulted          bool
	entered, release chan struct{}
	once             sync.Once
}

type persistenceBarrierConn struct {
	driver.Conn
	barrier *persistenceBarrierConnector
}

func (c *persistenceBarrierConnector) Driver() driver.Driver { return c.inner.Driver() }
func (c *persistenceBarrierConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &persistenceBarrierConn{Conn: conn, barrier: c}, nil
}
func (c *persistenceBarrierConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
}
func (c *persistenceBarrierConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}
func (c *persistenceBarrierConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	args = append([]driver.NamedValue(nil), args...)
	faultIndex, faultValue := -1, any(nil)
	switch c.barrier.faultStage {
	case "message":
		if query == insertReliableMessageSQL && args[0].Value == c.barrier.commandID {
			faultIndex, faultValue = 0, ""
		}
	case "counter":
		if query == advanceConversationSQL {
			faultIndex = 0
		}
	case "message_event", "result_event":
		kind := messaging.EventMessagePersisted
		if c.barrier.faultStage == "result_event" {
			kind = messaging.EventCommandResult
		}
		if query == insertMessageOutboxSQL && fmt.Sprint(args[1].Value) == string(kind) && args[2].Value == c.barrier.commandID {
			faultIndex, faultValue = 1, "invalid.event"
		}
	case "terminal":
		if query == finishCommandSQL && args[6].Value == c.barrier.commandID {
			faultIndex, faultValue = 0, "processing"
		}
	}
	if faultIndex >= 0 {
		args[faultIndex].Value = faultValue
		c.barrier.faulted = true
	}
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	if err != nil {
		return result, err
	}
	if c.barrier.entered != nil && query == insertMessageOutboxSQL && fmt.Sprint(args[1].Value) == string(messaging.EventCommandResult) && args[2].Value == c.barrier.commandID {
		c.barrier.once.Do(func() { close(c.barrier.entered) })
		select {
		case <-c.barrier.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return result, nil
}

func persistenceBarrierTest(t *testing.T, ctx context.Context, connector driver.Connector, reader *sql.DB, repo *MessageRepository, command, next messaging.Command, abort bool, baseline uint64) {
	t.Helper()
	barrier := &persistenceBarrierConnector{inner: connector, commandID: command.CommandID, entered: make(chan struct{}), release: make(chan struct{})}
	pool := sql.OpenDB(barrier)
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	heldRepo, err := NewMessageRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	heldCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var once sync.Once
	open := func() { once.Do(func() { close(barrier.release) }) }
	type response struct {
		outcome CommandExecution
		err     error
	}
	done := make(chan response, 1)
	go func() { outcome, err := heldRepo.PersistCommand(heldCtx, command); done <- response{outcome, err} }()
	joined := false
	defer func() {
		cancel()
		open()
		if !joined {
			select {
			case <-done:
			case <-ctx.Done():
			}
		}
	}()
	select {
	case <-barrier.entered:
	case response := <-done:
		joined = true
		t.Fatalf("SQL barrier did not enter: %v", response.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for _, query := range []string{"SELECT COUNT(*) FROM message_command WHERE command_id=?", "SELECT COUNT(*) FROM message WHERE id=?", "SELECT COUNT(*) FROM message_outbox WHERE command_id=?"} {
		commandCount(t, ctx, reader, query, 0, command.CommandID)
	}
	if baseline == 0 {
		commandCount(t, ctx, reader, "SELECT COUNT(*) FROM conversation WHERE id=?", 0, command.ConversationID)
	} else {
		persistenceCheckSequence(t, ctx, reader, command.ConversationID, baseline)
	}
	waitCtx, waitCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	waiting, err := repo.PersistCommand(waitCtx, next)
	waitCancel()
	if !errors.Is(err, context.DeadlineExceeded) || waiting.Result != nil {
		t.Fatalf("later sequence bypassed uncommitted writer: %+v %v", waiting, err)
	}
	if abort {
		cancel()
	}
	open()
	responseValue := <-done
	joined = true
	if abort {
		if !errors.Is(responseValue.err, context.Canceled) || responseValue.outcome.Result != nil {
			t.Fatalf("canceled writer committed: %+v %v", responseValue.outcome, responseValue.err)
		}
		for _, query := range []string{"SELECT COUNT(*) FROM message_command WHERE command_id=?", "SELECT COUNT(*) FROM message WHERE id=?", "SELECT COUNT(*) FROM message_outbox WHERE command_id=?"} {
			commandCount(t, ctx, reader, query, 0, command.CommandID)
		}
		responseValue.outcome, responseValue.err = repo.PersistCommand(ctx, command)
	}
	if responseValue.err != nil || responseValue.outcome.Result == nil || responseValue.outcome.Result.State != CommandPersisted {
		t.Fatalf("held/retried writer=%+v %v", responseValue.outcome, responseValue.err)
	}
	message := persistenceReadMessage(t, ctx, reader, command)
	if uint64(message.Seq) != baseline+1 {
		t.Fatalf("first sequence=%s baseline=%d", message.Seq, baseline)
	}
	persistenceCheckEvents(t, ctx, reader, command, responseValue.outcome, &message)
	outcome, err := repo.PersistCommand(ctx, next)
	if err != nil || outcome.Result == nil || outcome.Result.State != CommandPersisted {
		t.Fatalf("later retry=%+v %v", outcome, err)
	}
	persistenceCheckSequence(t, ctx, reader, command.ConversationID, baseline+2)
}
