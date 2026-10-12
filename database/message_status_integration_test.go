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

func TestCommandStatusIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, err := testenv.Start(ctx, filepath.Join("..", "docker-compose.test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("command status fixture run=%s", env.RunID())
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
	if err := db.WithContext(ctx).AutoMigrate(&models.User{}, &models.Group{}, &models.Relation{}); err != nil {
		t.Fatal(err)
	}
	users := make([]models.User, 3)
	for i := range users {
		users[i].Phone = fmt.Sprintf("%s-%d", env.RunID()[:24], i)
		users[i].Identifier, users[i].Name = users[i].Phone, fmt.Sprintf("status-user-%d", i)
		if err := db.WithContext(ctx).Create(&users[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := NewRelationManager(db).AddFriendByPhone(ctx, users[0].ID, users[1].Phone); err != nil {
		t.Fatal(err)
	}
	groupWriter := NewGroupManager(db)
	if err := groupWriter.CreateGroup(ctx, "status-group", users[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	var group models.Group
	if err := db.WithContext(ctx).Where("name=?", "status-group").Take(&group).Error; err != nil {
		t.Fatal(err)
	}
	repo, err := NewMessageRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	options := mysqlDriver.NewConfig()
	options.User, options.Passwd, options.DBName = conf.MYSQL.User, conf.MYSQL.Password, conf.MYSQL.DbName
	options.Net, options.Addr = "tcp", net.JoinHostPort(conf.MYSQL.Host, conf.MYSQL.Port)
	options.ParseTime, options.InterpolateParams = true, true
	options.Loc = time.FixedZone("status-offset", 13*60*60)
	options.Params = map[string]string{"charset": "utf8mb4", "time_zone": "'+05:30'", "transaction_isolation": "'READ-COMMITTED'"}
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
	var isolation string
	if err := otherPool.QueryRowContext(ctx, "SELECT @@transaction_isolation").Scan(&isolation); err != nil || isolation != "READ-COMMITTED" {
		t.Fatalf("test session isolation=%s err=%v", isolation, err)
	}
	makeCommand := func(sender, key uint64, target string, kind messaging.TargetType) messaging.Command {
		t.Helper()
		cmd, err := messaging.Normalize(sender, messaging.SendRequest{ClientMessageID: fmt.Sprintf("00000000-0000-4000-8000-%012x", key), TargetID: target, TargetType: kind, Media: messaging.Text, Content: "  你好🙂\x00e\u0301  "})
		if err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	lookup := func(t *testing.T, repository *MessageRepository, cmd messaging.Command, want messaging.StatusState) messaging.CommandStatus {
		t.Helper()
		query, _ := messaging.NormalizeStatusQuery(mustStatusSender(t, cmd), cmd.ClientMessageID, cmd.PayloadHash)
		status, err := repository.GetCommandStatus(ctx, query.SenderID, strings.ToUpper(cmd.ClientMessageID), query.PayloadHash)
		if err != nil || status.State != want || status.ClientMessageID != cmd.ClientMessageID || status.PayloadHash != cmd.PayloadHash {
			t.Fatalf("status=%+v want=%s err=%v", status, want, err)
		}
		return status
	}
	private := makeCommand(uint64(users[0].ID), 1, fmt.Sprint(users[1].ID), messaging.Private)
	groupCmd := makeCommand(uint64(users[0].ID), 2, fmt.Sprint(group.ID), messaging.Group)
	rejected := makeCommand(uint64(users[2].ID), 3, fmt.Sprint(users[0].ID), messaging.Private)
	for _, cmd := range []messaging.Command{private, groupCmd, rejected} {
		if outcome, err := repo.PersistCommand(ctx, cmd); err != nil || outcome.Result == nil {
			t.Fatalf("seed command=%+v %v", outcome, err)
		}
	}

	t.Run("canonical_terminal_and_authenticated_scope", func(t *testing.T) {
		for _, cmd := range []messaging.Command{private, groupCmd} {
			status := lookup(t, otherRepo, cmd, messaging.StatusPersisted)
			want := persistenceReadMessage(t, ctx, pool, cmd)
			if status.Message == nil || !reflect.DeepEqual(*status.Message, want) || status.ReasonCode != "" || status.PollAfterMs != 0 {
				t.Fatalf("canonical DTO=%+v want=%+v", status, want)
			}
		}
		status := lookup(t, repo, rejected, messaging.StatusRejected)
		if status.Message != nil || status.ReasonCode != ReasonNotFriends || status.PollAfterMs != 0 {
			t.Fatalf("rejection=%+v", status)
		}
		for _, sender := range []uint64{uint64(users[1].ID), math.MaxUint64} {
			status, err := otherRepo.GetCommandStatus(ctx, sender, private.ClientMessageID, private.PayloadHash)
			if err != nil || status.State != messaging.StatusPendingOrUnknown || status.Message != nil || status.ReasonCode != "" || status.PollAfterMs != messaging.DefaultStatusPollAfterMs {
				t.Fatalf("cross-user status=%+v %v", status, err)
			}
		}
		ownSameKey := makeCommand(uint64(users[2].ID), 1, fmt.Sprint(users[0].ID), messaging.Private)
		if _, err := repo.PersistCommand(ctx, ownSameKey); err != nil {
			t.Fatal(err)
		}
		lookup(t, otherRepo, ownSameKey, messaging.StatusRejected)
		lookup(t, otherRepo, private, messaging.StatusPersisted)
	})

	t.Run("conflict_has_no_canonical_message_or_hash", func(t *testing.T) {
		conflict := private
		conflict.PayloadHash = strings.Repeat("0", 64)
		status := lookup(t, repo, conflict, messaging.StatusConflict)
		body, err := json.Marshal(status)
		if err != nil || status.Message != nil || status.ReasonCode != "" || strings.Contains(string(body), private.PayloadHash) || strings.Contains(string(body), private.Content) {
			t.Fatalf("conflict leaked canonical result: %s %v", body, err)
		}
		insertStatusQuarantine(t, ctx, pool, conflict, "conflicting-transport", "open", true)
		lookup(t, repo, conflict, messaging.StatusConflict)
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message WHERE id=?", 1, private.CommandID)
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message_outbox WHERE command_id=?", 2, private.CommandID)
	})

	t.Run("matching_unresolved_quarantine_only_and_terminal_priority", func(t *testing.T) {
		for i, state := range []string{"open", "replay_pending", "resolved"} {
			cmd := makeCommand(uint64(users[0].ID), uint64(10+i), fmt.Sprint(users[1].ID), messaging.Private)
			insertStatusQuarantine(t, ctx, pool, cmd, "quarantine-"+state, state, i != 1)
			want := messaging.StatusQuarantined
			if state == "resolved" {
				want = messaging.StatusPendingOrUnknown
			}
			status := lookup(t, otherRepo, cmd, want)
			if status.Message != nil || status.ReasonCode != "" || status.PollAfterMs != messaging.DefaultStatusPollAfterMs {
				t.Fatalf("quarantine diagnostics leaked: %+v", status)
			}
			body, _ := json.Marshal(status)
			if strings.Contains(string(body), "never-expose") || strings.Contains(string(body), "transport") || strings.Contains(string(body), "SQL_DRIVER_ERROR") {
				t.Fatalf("raw quarantine leaked: %s", body)
			}
			otherHash := cmd
			otherHash.PayloadHash = strings.Repeat("a", 64)
			lookup(t, otherRepo, otherHash, messaging.StatusPendingOrUnknown)
			if status, err := repo.GetCommandStatus(ctx, uint64(users[1].ID), cmd.ClientMessageID, cmd.PayloadHash); err != nil || status.State != messaging.StatusPendingOrUnknown {
				t.Fatalf("other sender saw quarantine: %+v %v", status, err)
			}
		}
		insertStatusQuarantine(t, ctx, pool, private, "persisted-with-quarantine", "open", true)
		insertStatusQuarantine(t, ctx, pool, rejected, "rejected-with-quarantine", "replay_pending", true)
		lookup(t, repo, private, messaging.StatusPersisted)
		lookup(t, repo, rejected, messaging.StatusRejected)
	})

	t.Run("terminal_status_survives_current_permission_changes", func(t *testing.T) {
		privateBefore := lookup(t, repo, private, messaging.StatusPersisted)
		groupBefore := lookup(t, repo, groupCmd, messaging.StatusPersisted)
		commandExec(t, ctx, pool, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE type=? AND ((owner_id=? AND target_id=?) OR (owner_id=? AND target_id=?))", models.FriendRelation, users[0].ID, users[1].ID, users[1].ID, users[0].ID)
		if err := groupWriter.DeleteGroup(ctx, users[0].ID, group.ID); err != nil {
			t.Fatal(err)
		}
		if got := lookup(t, otherRepo, private, messaging.StatusPersisted); !reflect.DeepEqual(got, privateBefore) {
			t.Fatal("permission change altered own terminal result")
		}
		if got := lookup(t, otherRepo, groupCmd, messaging.StatusPersisted); !reflect.DeepEqual(got, groupBefore) {
			t.Fatal("group deletion altered own terminal result")
		}
		commandExec(t, ctx, pool, "UPDATE relation SET deleted_at=NULL WHERE type=? AND ((owner_id=? AND target_id=?) OR (owner_id=? AND target_id=?))", models.FriendRelation, users[0].ID, users[1].ID, users[1].ID, users[0].ID)
	})

	t.Run("uncommitted_claim_never_blocks_or_looks_rejected", func(t *testing.T) {
		cmd := makeCommand(uint64(users[0].ID), 30, fmt.Sprint(users[1].ID), messaging.Private)
		barrier := &persistenceBarrierConnector{inner: connector, commandID: cmd.CommandID, entered: make(chan struct{}), release: make(chan struct{})}
		writer := sql.OpenDB(barrier)
		t.Cleanup(func() {
			if err := writer.Close(); err != nil {
				t.Error(err)
			}
		})
		writerRepo, _ := NewMessageRepository(writer)
		heldCtx, heldCancel := context.WithCancel(ctx)
		defer heldCancel()
		var once sync.Once
		release := func() { once.Do(func() { close(barrier.release) }) }
		done := make(chan error, 1)
		go func() { _, err := writerRepo.PersistCommand(heldCtx, cmd); done <- err }()
		joined := false
		defer func() {
			heldCancel()
			release()
			if !joined {
				select {
				case <-done:
				case <-ctx.Done():
				}
			}
		}()
		select {
		case <-barrier.entered:
		case err := <-done:
			joined = true
			t.Fatalf("writer failed: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
		status, err := repo.GetCommandStatus(readCtx, mustStatusSender(t, cmd), cmd.ClientMessageID, cmd.PayloadHash)
		readCancel()
		if err != nil || status.State != messaging.StatusPendingOrUnknown || status.ReasonCode != "" {
			t.Fatalf("uncommitted claim returned/blocking status=%+v %v", status, err)
		}
		release()
		err = <-done
		joined = true
		if err != nil {
			t.Fatal(err)
		}
		lookup(t, repo, cmd, messaging.StatusPersisted)
	})

	t.Run("command_and_quarantine_reads_share_one_snapshot", func(t *testing.T) {
		cmd := makeCommand(uint64(users[0].ID), 40, fmt.Sprint(users[1].ID), messaging.Private)
		insertStatusQuarantine(t, ctx, pool, cmd, "snapshot-transport", "open", true)
		barrier := &statusSnapshotConnector{inner: connector, entered: make(chan struct{}), release: make(chan struct{})}
		reader := sql.OpenDB(barrier)
		t.Cleanup(func() {
			if err := reader.Close(); err != nil {
				t.Error(err)
			}
		})
		readerRepo, _ := NewMessageRepository(reader)
		readCtx, readCancel := context.WithCancel(ctx)
		defer readCancel()
		type response struct {
			status messaging.CommandStatus
			err    error
		}
		done := make(chan response, 1)
		var once sync.Once
		release := func() { once.Do(func() { close(barrier.release) }) }
		sender := mustStatusSender(t, cmd)
		go func() {
			status, err := readerRepo.GetCommandStatus(readCtx, sender, cmd.ClientMessageID, cmd.PayloadHash)
			done <- response{status, err}
		}()
		joined := false
		defer func() {
			readCancel()
			release()
			if !joined {
				select {
				case <-done:
				case <-ctx.Done():
				}
			}
		}()
		select {
		case <-barrier.entered:
		case res := <-done:
			joined = true
			t.Fatalf("snapshot failed: %v", res.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if _, err := repo.PersistCommand(ctx, cmd); err != nil {
			t.Fatal(err)
		}
		commandExec(t, ctx, pool, "UPDATE message_quarantine SET state='resolved',resolved_at=UTC_TIMESTAMP(6) WHERE transport_id=?", "snapshot-transport")
		release()
		res := <-done
		joined = true
		if res.err != nil || res.status.State != messaging.StatusQuarantined {
			t.Fatalf("mixed snapshots lost command/quarantine responsibility: %+v %v", res.status, res.err)
		}
		lookup(t, otherRepo, cmd, messaging.StatusPersisted)
	})

	t.Run("sql_failures_cancellation_and_corruption_fail_closed", func(t *testing.T) {
		unknown := makeCommand(uint64(users[0].ID), 50, fmt.Sprint(users[1].ID), messaging.Private)
		for _, table := range []string{"message_command", "message", "message_quarantine"} {
			cmd := private
			if table == "message_quarantine" {
				cmd = unknown
			}
			commandExec(t, ctx, pool, "RENAME TABLE "+table+" TO status_unavailable")
			status, err := repo.GetCommandStatus(ctx, mustStatusSender(t, cmd), cmd.ClientMessageID, cmd.PayloadHash)
			commandExec(t, ctx, pool, "RENAME TABLE status_unavailable TO "+table)
			var sqlErr *mysqlDriver.MySQLError
			if !errors.As(err, &sqlErr) || sqlErr.Number != 1146 || !reflect.DeepEqual(status, messaging.CommandStatus{}) {
				t.Fatalf("SQL %s failure became status: %+v %v", table, status, err)
			}
		}
		commandExec(t, ctx, pool, "UPDATE message SET content='tampered' WHERE id=?", private.CommandID)
		status, err := repo.GetCommandStatus(ctx, mustStatusSender(t, private), private.ClientMessageID, private.PayloadHash)
		commandExec(t, ctx, pool, "UPDATE message SET content=? WHERE id=?", private.Content, private.CommandID)
		if !errors.Is(err, ErrCommandInvariant) || !reflect.DeepEqual(status, messaging.CommandStatus{}) {
			t.Fatalf("corrupt payload returned success: %+v %v", status, err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := repo.GetCommandStatus(canceled, mustStatusSender(t, private), private.ClientMessageID, private.PayloadHash); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if err := otherPool.Close(); err != nil {
			t.Fatal(err)
		}
		status, err = otherRepo.GetCommandStatus(ctx, mustStatusSender(t, unknown), unknown.ClientMessageID, unknown.PayloadHash)
		if err == nil || !reflect.DeepEqual(status, messaging.CommandStatus{}) {
			t.Fatalf("closed pool returned pending: %+v %v", status, err)
		}
	})
	commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message_command WHERE reason_code LIKE '__claim:%' OR state='processing'", 0)
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatalf("status lookup changed schema: %v", err)
	}
}

func mustStatusSender(t *testing.T, cmd messaging.Command) uint64 {
	t.Helper()
	sender, err := strconv.ParseUint(cmd.SenderID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return sender
}

func insertStatusQuarantine(t *testing.T, ctx context.Context, pool *sql.DB, command messaging.Command, transport, state string, withCommand bool) {
	t.Helper()
	var commandID, resolved any
	if withCommand {
		commandID = command.CommandID
	}
	if state == "resolved" {
		resolved = "2026-10-11 00:00:00.000000"
	}
	commandExec(t, ctx, pool, `INSERT INTO message_quarantine
 (transport_id,command_id,sender_id,client_message_id,payload_hash,raw_body,raw_headers,error_code,error_detail,state,created_at,resolved_at)
 VALUES (?,?,?,?,?,?,'{"diagnostic":"never-expose"}','SQL_DRIVER_ERROR','never-expose diagnostic',?,UTC_TIMESTAMP(6),?)`,
		transport, commandID, mustStatusSender(t, command), command.ClientMessageID, command.PayloadHash, []byte("never-expose original body"), state, resolved)
}

type statusSnapshotConnector struct {
	inner            driver.Connector
	entered, release chan struct{}
	once             sync.Once
}
type statusSnapshotConn struct {
	driver.Conn
	barrier *statusSnapshotConnector
}

func (c *statusSnapshotConnector) Driver() driver.Driver { return c.inner.Driver() }
func (c *statusSnapshotConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &statusSnapshotConn{Conn: conn, barrier: c}, nil
}
func (c *statusSnapshotConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
}
func (c *statusSnapshotConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	if query == selectStatusCommandSQL {
		c.barrier.once.Do(func() { close(c.barrier.entered) })
		select {
		case <-c.barrier.release:
		case <-ctx.Done():
			rows.Close()
			return nil, ctx.Err()
		}
	}
	return rows, nil
}
