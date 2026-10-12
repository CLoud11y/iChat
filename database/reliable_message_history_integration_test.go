//go:build integration

package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
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
	"sync"
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

func TestReliableMessageHistoryIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, err := testenv.Start(ctx, filepath.Join("..", "docker-compose.test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("reliable message history fixture run=%s", env.RunID())
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
		t.Fatalf("refusing fixture writes in %q: %v", schema, err)
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).AutoMigrate(&models.User{}, &models.Group{}, &models.Relation{}); err != nil {
		t.Fatal(err)
	}
	users := make([]models.User, 4)
	for i := range users {
		users[i].Phone = fmt.Sprintf("%s-%d", env.RunID()[:24], i)
		users[i].Identifier, users[i].Name = users[i].Phone, fmt.Sprintf("history-user-%d", i)
		if err := db.WithContext(ctx).Create(&users[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := NewRelationManager(db).AddFriendByPhone(ctx, users[0].ID, users[1].Phone); err != nil {
		t.Fatal(err)
	}
	groups := NewGroupManager(db)
	if err := groups.CreateGroup(ctx, "reliable-history", users[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	var group models.Group
	if err := db.WithContext(ctx).Where("name=?", "reliable-history").Take(&group).Error; err != nil {
		t.Fatal(err)
	}
	repo, _ := NewMessageRepository(pool)
	options := mysqlDriver.NewConfig()
	options.User, options.Passwd, options.DBName = conf.MYSQL.User, conf.MYSQL.Password, conf.MYSQL.DbName
	options.Net, options.Addr = "tcp", net.JoinHostPort(conf.MYSQL.Host, conf.MYSQL.Port)
	options.ParseTime, options.InterpolateParams = true, true
	options.Loc = time.FixedZone("history-offset", 13*60*60)
	options.Params = map[string]string{"charset": "utf8mb4", "time_zone": "'+05:30'", "transaction_isolation": "'READ-COMMITTED'"}
	connector, err := mysqlDriver.NewConnector(options)
	if err != nil {
		t.Fatal(err)
	}
	reader := sql.OpenDB(connector)
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	readRepo, _ := NewMessageRepository(reader)
	var isolation string
	if err := reader.QueryRowContext(ctx, "SELECT @@transaction_isolation").Scan(&isolation); err != nil || isolation != "READ-COMMITTED" {
		t.Fatalf("session isolation %s: %v", isolation, err)
	}
	var key uint64
	makeCommand := func(sender uint, target uint, kind messaging.TargetType, media messaging.Media) messaging.Command {
		t.Helper()
		key++
		command, err := messaging.Normalize(uint64(sender), messaging.SendRequest{ClientMessageID: fmt.Sprintf("00000000-0000-4000-8000-%012x", key), TargetID: fmt.Sprint(target), TargetType: kind, Media: media, Content: fmt.Sprintf("  中🙂\x00e\u0301-%d  ", key)})
		if err != nil {
			t.Fatal(err)
		}
		return command
	}
	persist := func(command messaging.Command) {
		t.Helper()
		outcome, err := repo.PersistCommand(ctx, command)
		if err != nil || outcome.Result == nil || outcome.Result.State != CommandPersisted {
			t.Fatalf("persist=%+v %v", outcome, err)
		}
	}
	var private, groupCommands []messaging.Command
	for i := 0; i < 6; i++ {
		sender, target := users[0].ID, users[1].ID
		if i%2 == 1 {
			sender, target = target, sender
		}
		command := makeCommand(sender, target, messaging.Private, []messaging.Media{messaging.Text, messaging.Picture, messaging.Audio}[i%3])
		persist(command)
		private = append(private, command)
	}
	for _, media := range []messaging.Media{messaging.Text, messaging.Picture, messaging.Audio} {
		command := makeCommand(users[0].ID, group.ID, messaging.Group, media)
		persist(command)
		groupCommands = append(groupCommands, command)
	}
	conversation := private[0].ConversationID
	uid := uint64(users[0].ID)
	assertMessage := func(t *testing.T, got messaging.MessageDTO, command messaging.Command) {
		t.Helper()
		want := persistenceReadMessage(t, ctx, pool, command)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("message=%+v want=%+v", got, want)
		}
	}

	t.Run("full_forward_and_backward_pages_ignore_delivery_cursor", func(t *testing.T) {
		commandExec(t, ctx, pool, "INSERT INTO message_delivery_cursor(user_id,conversation_id,delivered_seq,updated_at) VALUES(?,?,?,UTC_TIMESTAMP(6))", uid, conversation, 6)
		after := "0"
		index := 0
		for {
			page, err := readRepo.SyncMessages(ctx, uint64(users[1].ID), conversation, after, 2)
			if err != nil || page.HighWatermark != 6 || len(page.Items) != 2 {
				t.Fatalf("sync=%+v %v", page, err)
			}
			for _, message := range page.Items {
				assertMessage(t, message, private[index])
				index++
			}
			after = page.NextAfterSeq.String()
			if !page.HasMore {
				break
			}
		}
		if index != 6 {
			t.Fatal("incomplete sync")
		}
		before := ""
		index = 6
		for {
			page, err := readRepo.HistoryMessages(ctx, uid, conversation, before, 2)
			if err != nil || page.HighWatermark != 6 || len(page.Items) != 2 {
				t.Fatalf("history=%+v %v", page, err)
			}
			index -= len(page.Items)
			for i, message := range page.Items {
				assertMessage(t, message, private[index+i])
			}
			before = page.NextBeforeSeq.String()
			if !page.HasMore {
				break
			}
		}
		if index != 0 {
			t.Fatal("incomplete history")
		}
		page, err := readRepo.SyncMessages(ctx, uid, conversation, "6", 2)
		if err != nil || page.Items == nil || len(page.Items) != 0 || page.NextAfterSeq != 6 || page.HasMore {
			t.Fatalf("exhausted=%+v %v", page, err)
		}
		history, err := readRepo.HistoryMessages(ctx, uid, conversation, "2", 500)
		if err != nil || len(history.Items) != 1 || history.NextBeforeSeq != 1 || history.HasMore {
			t.Fatalf("exclusive before=%+v %v", history, err)
		}
	})

	t.Run("authorization_empty_conversations_and_group_history", func(t *testing.T) {
		if _, err := readRepo.SyncMessages(ctx, uint64(users[2].ID), conversation, "0", 2); !errors.Is(err, ErrConversationForbidden) {
			t.Fatal(err)
		}
		if _, err := readRepo.HistoryMessages(ctx, uint64(users[2].ID), groupCommands[0].ConversationID, "", 2); !errors.Is(err, ErrConversationForbidden) {
			t.Fatal(err)
		}
		empty := fmt.Sprintf("p:%d:%d", users[0].ID, users[3].ID)
		page, err := readRepo.SyncMessages(ctx, uid, empty, "0", 2)
		if err != nil || page.HighWatermark != 0 || page.Items == nil || len(page.Items) != 0 {
			t.Fatalf("empty=%+v %v", page, err)
		}
		if err := groups.JoinGroup(ctx, users[2].ID, group.ID); err != nil {
			t.Fatal(err)
		}
		page, err = readRepo.SyncMessages(ctx, uint64(users[2].ID), groupCommands[0].ConversationID, "0", 500)
		if err != nil || len(page.Items) != 3 {
			t.Fatalf("joined history=%+v %v", page, err)
		}
		for i, message := range page.Items {
			assertMessage(t, message, groupCommands[i])
		}
		commandExec(t, ctx, pool, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE type=? AND owner_id=? AND target_id=?", models.GroupRelation, users[2].ID, group.ID)
		if _, err := readRepo.SyncMessages(ctx, uint64(users[2].ID), groupCommands[0].ConversationID, "0", 2); !errors.Is(err, ErrConversationForbidden) {
			t.Fatal("former member read history", err)
		}
		commandExec(t, ctx, pool, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE type=? AND ((owner_id=? AND target_id=?) OR (owner_id=? AND target_id=?))", models.FriendRelation, users[0].ID, users[1].ID, users[1].ID, users[0].ID)
		if _, err := readRepo.HistoryMessages(ctx, uid, conversation, "", 2); err != nil {
			t.Fatal("private participant lost retained history", err)
		}
		commandExec(t, ctx, pool, "UPDATE relation SET deleted_at=NULL WHERE type=? AND ((owner_id=? AND target_id=?) OR (owner_id=? AND target_id=?))", models.FriendRelation, users[0].ID, users[1].ID, users[1].ID, users[0].ID)
	})

	t.Run("watermark_and_messages_share_snapshot_during_new_commit", func(t *testing.T) {
		command := makeCommand(users[1].ID, users[0].ID, messaging.Private, messaging.Text)
		barrier := &pageSnapshotConnector{inner: connector, query: selectReadConversationSQL, afterRead: func() { persist(command) }}
		barrierPool := sql.OpenDB(barrier)
		defer barrierPool.Close()
		barrierRepo, _ := NewMessageRepository(barrierPool)
		page, err := barrierRepo.SyncMessages(ctx, uid, conversation, "4", 2)
		if err != nil || page.HighWatermark != 6 || page.NextAfterSeq != 6 || page.HasMore || len(page.Items) != 2 {
			t.Fatalf("mixed snapshot=%+v %v", page, err)
		}
		page, err = readRepo.SyncMessages(ctx, uid, conversation, "6", 2)
		if err != nil || page.HighWatermark != 7 || len(page.Items) != 1 {
			t.Fatalf("later commit missing=%+v %v", page, err)
		}
		assertMessage(t, page.Items[0], command)
	})

	t.Run("membership_and_messages_share_snapshot_during_revocation", func(t *testing.T) {
		groupConversation := groupCommands[0].ConversationID
		for _, deleteGroup := range []bool{false, true} {
			beforeWatermark := messaging.Sequence(len(groupCommands))
			command := makeCommand(users[0].ID, group.ID, messaging.Group, messaging.Text)
			barrier := &pageSnapshotConnector{inner: connector, query: selectReadMembershipSQL, afterRead: func() {
				// Commit a new message after the authorization read. READ COMMITTED
				// would leak the newer watermark/items into this older read view.
				persist(command)
				if deleteGroup {
					if err := groups.DeleteGroup(ctx, users[0].ID, group.ID); err != nil {
						t.Fatal(err)
					}
				} else {
					commandExec(t, ctx, pool, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE owner_id=? AND target_id=? AND type=?", users[0].ID, group.ID, models.GroupRelation)
				}
			}}
			barrierPool := sql.OpenDB(barrier)
			barrierRepo, _ := NewMessageRepository(barrierPool)
			page, err := barrierRepo.HistoryMessages(ctx, uid, groupConversation, "", 500)
			barrierPool.Close()
			if err != nil || page.HighWatermark != beforeWatermark || len(page.Items) != len(groupCommands) {
				t.Fatalf("authorized snapshot revoked mid-page=%+v %v", page, err)
			}
			for i, message := range page.Items {
				assertMessage(t, message, groupCommands[i])
			}
			groupCommands = append(groupCommands, command)
			if _, err := readRepo.HistoryMessages(ctx, uid, groupConversation, "", 2); !errors.Is(err, ErrConversationForbidden) {
				t.Fatal("next page not reauthorized", err)
			}
			if !deleteGroup {
				commandExec(t, ctx, pool, "UPDATE relation SET deleted_at=NULL WHERE owner_id=? AND target_id=? AND type=?", users[0].ID, group.ID, models.GroupRelation)
			}
		}
	})

	t.Run("new_conversation_after_authorization_is_visible_next_page", func(t *testing.T) {
		if err := groups.CreateGroup(ctx, "empty-snapshot-group", users[0].ID, ""); err != nil {
			t.Fatal(err)
		}
		var emptyGroup models.Group
		if err := db.WithContext(ctx).Where("name=?", "empty-snapshot-group").Take(&emptyGroup).Error; err != nil {
			t.Fatal(err)
		}
		command := makeCommand(users[0].ID, emptyGroup.ID, messaging.Group, messaging.Picture)
		barrier := &pageSnapshotConnector{inner: connector, query: selectReadMembershipSQL, afterRead: func() { persist(command) }}
		barrierPool := sql.OpenDB(barrier)
		defer barrierPool.Close()
		barrierRepo, _ := NewMessageRepository(barrierPool)
		page, err := barrierRepo.SyncMessages(ctx, uid, command.ConversationID, "0", 2)
		if err != nil || page.HighWatermark != 0 || page.Items == nil || len(page.Items) != 0 || page.NextAfterSeq != 0 {
			t.Fatalf("new conversation leaked into old snapshot=%+v %v", page, err)
		}
		page, err = readRepo.SyncMessages(ctx, uid, command.ConversationID, "0", 2)
		if err != nil || page.HighWatermark != 1 || len(page.Items) != 1 {
			t.Fatalf("new conversation not recovered=%+v %v", page, err)
		}
		assertMessage(t, page.Items[0], command)
	})

	t.Run("uncommitted_sequence_is_not_visible_or_blocking", func(t *testing.T) {
		command := makeCommand(users[0].ID, users[1].ID, messaging.Private, messaging.Text)
		barrier := &persistenceBarrierConnector{inner: connector, commandID: command.CommandID, entered: make(chan struct{}), release: make(chan struct{})}
		writer := sql.OpenDB(barrier)
		defer writer.Close()
		writerRepo, _ := NewMessageRepository(writer)
		heldCtx, heldCancel := context.WithCancel(ctx)
		defer heldCancel()
		done := make(chan error, 1)
		go func() { _, err := writerRepo.PersistCommand(heldCtx, command); done <- err }()
		var once sync.Once
		release := func() { once.Do(func() { close(barrier.release) }) }
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
			t.Fatal("writer failed", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
		page, err := readRepo.SyncMessages(readCtx, uid, conversation, "7", 2)
		readCancel()
		if err != nil || page.HighWatermark != 7 || len(page.Items) != 0 || page.NextAfterSeq != 7 {
			t.Fatalf("uncommitted seq leaked/blocked=%+v %v", page, err)
		}
		release()
		err = <-done
		joined = true
		if err != nil {
			t.Fatal(err)
		}
		page, err = readRepo.SyncMessages(ctx, uid, conversation, "7", 2)
		if err != nil || page.HighWatermark != 8 || len(page.Items) != 1 || page.NextAfterSeq != 8 {
			t.Fatalf("commit not recovered=%+v %v", page, err)
		}
	})

	t.Run("unsigned_upper_boundary_without_cursor_overflow", func(t *testing.T) {
		// Deliberately move two existing fixture records to the upper boundary.
		// This tests only the local range, not a fabricated complete history.
		commandExec(t, ctx, pool, "UPDATE message SET seq=? WHERE id=?", uint64(math.MaxUint64), private[5].CommandID)
		commandExec(t, ctx, pool, "UPDATE message SET seq=? WHERE id=?", uint64(math.MaxUint64-1), private[4].CommandID)
		commandExec(t, ctx, pool, "UPDATE conversation SET last_seq=? WHERE id=?", uint64(math.MaxUint64), conversation)
		defer func() {
			commandExec(t, ctx, pool, "UPDATE message SET seq=6 WHERE id=?", private[5].CommandID)
			commandExec(t, ctx, pool, "UPDATE message SET seq=5 WHERE id=?", private[4].CommandID)
			commandExec(t, ctx, pool, "UPDATE conversation SET last_seq=8 WHERE id=?", conversation)
		}()
		page, err := readRepo.SyncMessages(ctx, uid, conversation, fmt.Sprint(uint64(math.MaxUint64-1)), 1)
		if err != nil || page.NextAfterSeq != messaging.Sequence(math.MaxUint64) || len(page.Items) != 1 || page.HasMore {
			t.Fatalf("unsigned sync=%+v %v", page, err)
		}
		history, err := readRepo.HistoryMessages(ctx, uid, conversation, "", 1)
		if err != nil || history.NextBeforeSeq != messaging.Sequence(math.MaxUint64) || len(history.Items) != 1 || !history.HasMore {
			t.Fatalf("unsigned latest history=%+v %v", history, err)
		}
	})

	t.Run("gaps_corruption_sql_failure_cancel_and_closed_pool", func(t *testing.T) {
		commandExec(t, ctx, pool, "UPDATE message SET seq=99 WHERE id=?", private[2].CommandID)
		page, err := readRepo.SyncMessages(ctx, uid, conversation, "0", 2)
		if !errors.Is(err, ErrCommandInvariant) || !reflect.DeepEqual(page, messaging.SyncPage{}) {
			t.Fatalf("lookahead gap advanced cursor=%+v %v", page, err)
		}
		history, histErr := readRepo.HistoryMessages(ctx, uid, conversation, "5", 2)
		commandExec(t, ctx, pool, "UPDATE message SET seq=3 WHERE id=?", private[2].CommandID)
		if !errors.Is(histErr, ErrCommandInvariant) || !reflect.DeepEqual(history, messaging.HistoryPage{}) {
			t.Fatalf("history gap=%+v %v", history, histErr)
		}
		commandExec(t, ctx, pool, "UPDATE message SET content='tampered' WHERE id=?", private[0].CommandID)
		page, err = readRepo.SyncMessages(ctx, uid, conversation, "0", 2)
		commandExec(t, ctx, pool, "UPDATE message SET content=? WHERE id=?", private[0].Content, private[0].CommandID)
		if !errors.Is(err, ErrCommandInvariant) || !reflect.DeepEqual(page, messaging.SyncPage{}) {
			t.Fatalf("corrupt content=%+v %v", page, err)
		}
		for _, table := range []string{"conversation", "message", "message_command", "relation"} {
			commandExec(t, ctx, pool, "RENAME TABLE "+table+" TO history_unavailable")
			conv := conversation
			if table == "relation" {
				conv = groupCommands[0].ConversationID
			}
			page, err = readRepo.SyncMessages(ctx, uid, conv, "0", 2)
			commandExec(t, ctx, pool, "RENAME TABLE history_unavailable TO "+table)
			var sqlErr *mysqlDriver.MySQLError
			if !errors.As(err, &sqlErr) || sqlErr.Number != 1146 || !reflect.DeepEqual(page, messaging.SyncPage{}) {
				t.Fatalf("SQL %s fault=%+v %v", table, page, err)
			}
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := readRepo.SyncMessages(canceled, uid, conversation, "0", 2); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		if page, err := readRepo.SyncMessages(ctx, uid, conversation, "0", 2); err == nil || !reflect.DeepEqual(page, messaging.SyncPage{}) {
			t.Fatalf("closed pool=%+v %v", page, err)
		}
	})
	commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message_command WHERE reason_code LIKE '__claim:%' OR state='processing'", 0)
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatalf("read changed schema: %v", err)
	}
}

// The callback runs after a real SELECT establishes a read view, using another
// pool for changes. Only test builds contain this deterministic interleaving.
type pageSnapshotConnector struct {
	inner     driver.Connector
	query     string
	afterRead func()
	once      sync.Once
}
type pageSnapshotConn struct {
	driver.Conn
	barrier *pageSnapshotConnector
}

func (c *pageSnapshotConnector) Driver() driver.Driver { return c.inner.Driver() }
func (c *pageSnapshotConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &pageSnapshotConn{conn, c}, nil
}
func (c *pageSnapshotConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c *pageSnapshotConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err == nil && query == c.barrier.query {
		c.barrier.once.Do(c.barrier.afterRead)
	}
	return rows, err
}
