//go:build integration

package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"iChat/messaging"
	"iChat/migrations"
	"iChat/models"
	"iChat/tests/testenv"
	"iChat/utils"
)

func TestConversationsIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, e := testenv.Start(ctx, filepath.Join("..", "docker-compose.test.yml"))
	if env != nil {
		t.Cleanup(func() {
			c, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			if e := env.Close(c); e != nil {
				t.Error(e)
			}
		})
	}
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("conversation discovery fixture run=%s", env.RunID())
	conf, e := env.Config()
	if e != nil {
		t.Fatal(e)
	}
	db, e := utils.OpenMySQL(conf)
	if e != nil {
		t.Fatal(e)
	}
	pool, e := db.DB()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { pool.Close() })
	var schema string
	if e := pool.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); e != nil || schema != "ichat_test_"+env.RunID() {
		t.Fatal("refusing writes outside owned schema")
	}
	if e := migrations.ApplyReliable(ctx, pool); e != nil {
		t.Fatal(e)
	}
	if e := db.WithContext(ctx).AutoMigrate(&models.User{}, &models.Group{}, &models.Relation{}); e != nil {
		t.Fatal(e)
	}
	uids := []uint{1, 2, 10, 11, 99}
	users := make([]models.User, len(uids))
	for i, id := range uids {
		v := fmt.Sprintf("%s-%d", env.RunID()[:24], id)
		users[i] = models.User{Identifier: v, Phone: v, Name: "conversations-fixture"}
		users[i].ID = id
	}
	if e := db.WithContext(ctx).Create(&users).Error; e != nil {
		t.Fatal(e)
	}
	groups := make([]models.Group, 5)
	for i, id := range []uint{1, 2, 10, 20, 30} {
		groups[i] = models.Group{Name: fmt.Sprint("discovery-", id), OwnerId: 1, MaxUser: 50}
		groups[i].ID = id
	}
	if e := db.WithContext(ctx).Create(&groups).Error; e != nil {
		t.Fatal(e)
	}
	relations := []models.Relation{{OwnerId: 1, TargetId: 2, Type: models.FriendRelation}, {OwnerId: 2, TargetId: 1, Type: models.FriendRelation}, {OwnerId: 1, TargetId: 10, Type: models.FriendRelation}, {OwnerId: 10, TargetId: 1, Type: models.FriendRelation}, {OwnerId: 2, TargetId: 11, Type: models.FriendRelation}, {OwnerId: 11, TargetId: 2, Type: models.FriendRelation}, {OwnerId: 1, TargetId: 1, Type: models.GroupRelation}, {OwnerId: 1, TargetId: 2, Type: models.GroupRelation}, {OwnerId: 2, TargetId: 2, Type: models.GroupRelation}, {OwnerId: 1, TargetId: 10, Type: models.GroupRelation}, {OwnerId: 11, TargetId: 20, Type: models.GroupRelation}, {OwnerId: 1, TargetId: 30, Type: models.GroupRelation}}
	if e := db.WithContext(ctx).Create(&relations).Error; e != nil {
		t.Fatal(e)
	}
	repo, _ := NewMessageRepository(pool)
	var commands []messaging.Command
	original := map[string]messaging.CommandStatus{}
	persist := func(sender, target uint64, kind messaging.TargetType) messaging.Command {
		t.Helper()
		c, e := messaging.Normalize(sender, messaging.SendRequest{ClientMessageID: fmt.Sprintf("b8fd7c2e-1234-4234-8234-%012d", len(commands)+1), TargetType: kind, TargetID: strconv.FormatUint(target, 10), Media: messaging.Text, Content: fmt.Sprintf("discovery-private-%d", len(commands))})
		if e != nil {
			t.Fatal(e)
		}
		if _, e := repo.PersistCommand(ctx, c); e != nil {
			t.Fatal(e)
		}
		commands = append(commands, c)
		status, e := repo.GetCommandStatus(ctx, sender, c.ClientMessageID, c.PayloadHash)
		if e != nil || status.State != messaging.StatusPersisted {
			t.Fatal(status, e)
		}
		original[c.CommandID] = status
		return c
	}
	for i := 0; i < 3; i++ {
		persist(1, 2, messaging.Private)
	}
	persist(1, 10, messaging.Private)
	persist(2, 11, messaging.Private)
	for i := 0; i < 2; i++ {
		persist(1, 2, messaging.Group)
	}
	persist(1, 10, messaging.Group)
	persist(11, 20, messaging.Group)
	for _, a := range []struct {
		u   uint64
		id  string
		seq messaging.Sequence
	}{{1, "p:1:2", 2}, {1, "g:2", 1}, {2, "p:1:2", 3}} {
		if _, e := repo.AcknowledgeDelivery(ctx, a.u, a.id, a.seq); e != nil {
			t.Fatal(e)
		}
	}
	list := func(reader uint64, cursor string, limit int) messaging.ConversationPage {
		t.Helper()
		p, e := repo.ListConversations(ctx, reader, cursor, limit)
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	ids := func(p messaging.ConversationPage) []string {
		r := make([]string, 0, len(p.Items))
		for _, v := range p.Items {
			r = append(r, v.ConversationID)
		}
		return r
	}
	item := func(p messaging.ConversationPage, id string) messaging.ConversationSummary {
		t.Helper()
		for _, v := range p.Items {
			if v.ConversationID == id {
				return v
			}
		}
		t.Fatal("conversation missing", id)
		return messaging.ConversationSummary{}
	}
	run := func(name string, fn func(*testing.T)) {
		if !t.Run(name, fn) {
			t.FailNow()
		}
	}
	wantIDs := []string{"g:10", "g:2", "p:1:10", "p:1:2"}
	run("current_permissions_order_and_per_user_ACK", func(t *testing.T) {
		p := list(1, "", 0)
		if !reflect.DeepEqual(ids(p), wantIDs) || p.HasMore {
			t.Fatal(p)
		}
		v := item(p, "p:1:2")
		if v.LastSeq != 3 || v.DeliveredSeq != 2 {
			t.Fatal(v)
		}
		if item(p, "g:10").DeliveredSeq != 0 || item(p, "p:1:10").DeliveredSeq != 0 || item(p, "g:2").DeliveredSeq != 1 {
			t.Fatal(p)
		}
		if got := item(list(2, "", 10), "p:1:2"); got.DeliveredSeq != 3 {
			t.Fatal("another user's ACK leaked", got)
		}
		if p := list(99, "", 1); p.Items == nil || len(p.Items) != 0 || p.NextCursor != "" || p.HasMore {
			t.Fatal(p)
		}
		// Owner status alone does not grant group history; group 20 and the
		// member's groups 1/30 with no committed conversation stay absent.
	})
	run("exclusive_cursor_lookahead_and_no_requirement_to_exist", func(t *testing.T) {
		var seen []string
		cursor := ""
		for i := range wantIDs {
			p := list(1, cursor, 1)
			if len(p.Items) != 1 || p.HasMore != (i < len(wantIDs)-1) || p.NextCursor != wantIDs[i] {
				t.Fatal(p)
			}
			seen = append(seen, p.Items[0].ConversationID)
			cursor = p.NextCursor
		}
		if !reflect.DeepEqual(seen, wantIDs) {
			t.Fatal(seen)
		}
		if p := list(1, cursor, 1); len(p.Items) != 0 || p.NextCursor != cursor || p.HasMore {
			t.Fatal(p)
		}
		if p := list(1, "p:1:15", 10); !reflect.DeepEqual(ids(p), []string{"p:1:2"}) {
			t.Fatal("nonexistent cursor was not an ASCII position", p)
		}
	})
	dsn := mysql.NewConfig()
	dsn.Net, dsn.Addr = "tcp", net.JoinHostPort(conf.MYSQL.Host, conf.MYSQL.Port)
	dsn.User, dsn.Passwd, dsn.DBName = conf.MYSQL.User, conf.MYSQL.Password, conf.MYSQL.DbName
	dsn.ParseTime, dsn.InterpolateParams = true, true
	dsn.Timeout, dsn.ReadTimeout, dsn.WriteTimeout = time.Second, 2*time.Second, 2*time.Second
	dsn.Params = map[string]string{"transaction_isolation": "'READ-COMMITTED'", "time_zone": "'+05:30'"}
	connector, e := mysql.NewConnector(dsn)
	if e != nil {
		t.Fatal("configure isolated discovery connector")
	}
	run("membership_watermark_and_ACK_share_one_snapshot", func(t *testing.T) {
		hook := &pageSnapshotConnector{inner: connector, query: selectConversationReaderSQL}
		hook.afterRead = func() {
			persist(1, 2, messaging.Private)
			if _, e := repo.AcknowledgeDelivery(ctx, 1, "p:1:2", 4); e != nil {
				t.Fatal(e)
			}
			if _, e := pool.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE owner_id=1 AND target_id=2 AND type=?", models.GroupRelation); e != nil {
				t.Fatal(e)
			}
		}
		snapshotPool := sql.OpenDB(hook)
		defer snapshotPool.Close()
		snapshot, _ := NewMessageRepository(snapshotPool)
		p, e := snapshot.ListConversations(ctx, 1, "", 10)
		if e != nil || !reflect.DeepEqual(ids(p), wantIDs) {
			t.Fatal("mixed membership snapshot", p, e)
		}
		if v := item(p, "p:1:2"); v.LastSeq != 3 || v.DeliveredSeq != 2 {
			t.Fatal("watermark/ACK mixed snapshots", v)
		}
		latest := list(1, "", 10)
		if !reflect.DeepEqual(ids(latest), []string{"g:10", "p:1:10", "p:1:2"}) {
			t.Fatal(latest)
		}
		if v := item(latest, "p:1:2"); v.LastSeq != 4 || v.DeliveredSeq != 4 {
			t.Fatal(v)
		}
		if _, e := pool.ExecContext(ctx, "UPDATE relation SET deleted_at=NULL WHERE owner_id=1 AND target_id=2 AND type=?", models.GroupRelation); e != nil {
			t.Fatal(e)
		}
	})
	run("new_ID_behind_page_cursor_is_found_on_next_full_scan", func(t *testing.T) {
		first := list(1, "", 2)
		if first.NextCursor != "g:2" || !first.HasMore {
			t.Fatal(first)
		}
		persist(1, 1, messaging.Group)
		next := list(1, first.NextCursor, 2)
		if !reflect.DeepEqual(ids(next), []string{"p:1:10", "p:1:2"}) || next.HasMore {
			t.Fatal(next)
		}
		all := list(1, "", 10)
		expected := append([]string{"g:1"}, wantIDs...)
		if !reflect.DeepEqual(ids(all), expected) {
			t.Fatal("periodic full scan lost new conversation", all)
		}
	})
	run("departed_deleted_and_wrong_type_membership_do_not_authorize", func(t *testing.T) {
		if _, e := pool.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE owner_id=1 AND target_id=1 AND type=?", models.GroupRelation); e != nil {
			t.Fatal(e)
		}
		if _, e := pool.ExecContext(ctx, "UPDATE `group` SET deleted_at=UTC_TIMESTAMP(6) WHERE id=10"); e != nil {
			t.Fatal(e)
		}
		if e := db.WithContext(ctx).Create(&models.Relation{OwnerId: 1, TargetId: 20, Type: models.FriendRelation}).Error; e != nil {
			t.Fatal(e)
		}
		if _, e := pool.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE type=? AND ((owner_id=1 AND target_id=2) OR (owner_id=2 AND target_id=1))", models.FriendRelation); e != nil {
			t.Fatal(e)
		}
		p := list(1, "g:1", 10)
		if !reflect.DeepEqual(ids(p), []string{"g:2", "p:1:10", "p:1:2"}) {
			t.Fatal("stale membership/owner/friend relation leaked group", p)
		}
		// An unauthorized old cursor is still an ordering position. Private
		// history remains visible to its participant after friendship removal.
		if _, e := pool.ExecContext(ctx, "UPDATE `user` SET deleted_at=UTC_TIMESTAMP(6) WHERE id=99"); e != nil {
			t.Fatal(e)
		}
		if p, e := repo.ListConversations(ctx, 99, "", 1); !errors.Is(e, ErrConversationForbidden) || !reflect.DeepEqual(p, messaging.ConversationPage{}) {
			t.Fatal(p, e)
		}
	})
	run("uncommitted_changes_do_not_block_or_hide_committed_rows", func(t *testing.T) {
		tx, e := pool.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		for _, q := range []string{"UPDATE conversation SET last_seq=5 WHERE id='p:1:2'", "UPDATE message_delivery_cursor SET delivered_seq=5 WHERE user_id=1 AND conversation_id='p:1:2'"} {
			if _, e := tx.ExecContext(ctx, q); e != nil {
				t.Fatal(e)
			}
		}
		if _, e := tx.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE owner_id=1 AND target_id=2 AND type=?", models.GroupRelation); e != nil {
			t.Fatal(e)
		}
		short, stop := context.WithTimeout(ctx, 500*time.Millisecond)
		defer stop()
		p, e := repo.ListConversations(short, 1, "", 10)
		if e != nil || !reflect.DeepEqual(ids(p), []string{"g:2", "p:1:10", "p:1:2"}) {
			t.Fatal("discovery blocked/skipped locked data", p, e)
		}
		if v := item(p, "p:1:2"); v.LastSeq != 4 || v.DeliveredSeq != 4 {
			t.Fatal("uncommitted progress leaked", v)
		}
		if e := tx.Rollback(); e != nil {
			t.Fatal(e)
		}
	})
	run("unsigned_metadata_boundary_without_cursor_arithmetic", func(t *testing.T) {
		// Explicit temporary metadata boundary fixture, not a claim that 2^64-1
		// messages were committed. Restore it before final business validation.
		defer func() {
			if _, e := pool.ExecContext(ctx, "UPDATE message_delivery_cursor SET delivered_seq=1 WHERE user_id=1 AND conversation_id='g:2'"); e != nil {
				t.Error(e)
			}
			if _, e := pool.ExecContext(ctx, "UPDATE conversation SET last_seq=2 WHERE id='g:2'"); e != nil {
				t.Error(e)
			}
		}()
		if _, e := pool.ExecContext(ctx, "UPDATE conversation SET last_seq=? WHERE id='g:2'", uint64(math.MaxUint64)); e != nil {
			t.Fatal(e)
		}
		if _, e := pool.ExecContext(ctx, "UPDATE message_delivery_cursor SET delivered_seq=? WHERE user_id=1 AND conversation_id='g:2'", uint64(math.MaxUint64)); e != nil {
			t.Fatal(e)
		}
		p := list(1, "", 1)
		v := item(p, "g:2")
		if v.LastSeq != messaging.Sequence(math.MaxUint64) || v.DeliveredSeq != v.LastSeq || !p.HasMore {
			t.Fatal(p)
		}
	})
	run("corrupt_cursor_cancellation_and_closed_pool_fail_without_partial_page", func(t *testing.T) {
		if _, e := pool.ExecContext(ctx, "UPDATE message_delivery_cursor SET delivered_seq=5 WHERE user_id=1 AND conversation_id='p:1:2'"); e != nil {
			t.Fatal(e)
		}
		p, e := repo.ListConversations(ctx, 1, "", 10)
		if !errors.Is(e, ErrConversationInvariant) || !reflect.DeepEqual(p, messaging.ConversationPage{}) {
			t.Fatal("returned partial page", p, e)
		}
		if _, e := pool.ExecContext(ctx, "UPDATE message_delivery_cursor SET delivered_seq=4 WHERE user_id=1 AND conversation_id='p:1:2'"); e != nil {
			t.Fatal(e)
		}
		blocked := sql.OpenDB(connector)
		defer blocked.Close()
		blocked.SetMaxOpenConns(1)
		holder, e := blocked.Conn(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer holder.Close()
		waiting, _ := NewMessageRepository(blocked)
		short, stop := context.WithTimeout(ctx, 30*time.Millisecond)
		p, e = waiting.ListConversations(short, 1, "", 1)
		stop()
		if !errors.Is(e, context.DeadlineExceeded) || !reflect.DeepEqual(p, messaging.ConversationPage{}) {
			t.Fatal(p, e)
		}
		holder.Close()
		if blocked.Stats().InUse != 0 {
			t.Fatal("waiting query retained pool connection")
		}
		blocked.Close()
		if p, e := waiting.ListConversations(ctx, 1, "", 1); e == nil || !reflect.DeepEqual(p, messaging.ConversationPage{}) {
			t.Fatal(p, e)
		}
	})
	for _, c := range commands {
		sender, _ := strconv.ParseUint(c.SenderID, 10, 64)
		status, e := repo.GetCommandStatus(ctx, sender, c.ClientMessageID, c.PayloadHash)
		if e != nil || !reflect.DeepEqual(status, original[c.CommandID]) {
			t.Fatal("discovery changed business terminal", c.CommandID, e)
		}
	}
	for table, want := range map[string]int{"message_command": 11, "message": 11, "conversation": 7, "message_outbox": 22, "message_delivery_cursor": 3, "message_quarantine": 0} {
		var count int
		if e := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); e != nil || count != want {
			t.Fatal("wrong final collection", table, count, e)
		}
	}
	var untouched int
	if e := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_outbox WHERE status='pending' AND attempts=0 AND lease_token IS NULL").Scan(&untouched); e != nil || untouched != 22 {
		t.Fatal(untouched, e)
	}
	p := list(1, "", 500)
	got := ids(p)
	if !sort.StringsAreSorted(got) || !reflect.DeepEqual(got, []string{"g:2", "p:1:10", "p:1:2"}) {
		t.Fatal(p)
	}
	if v := item(p, "p:1:2"); v.LastSeq != 4 || v.DeliveredSeq != 4 {
		t.Fatal(v)
	}
	if v := item(p, "g:2"); v.LastSeq != 2 || v.DeliveredSeq != 1 {
		t.Fatal(v)
	}
	if e := migrations.ApplyReliable(ctx, pool); e != nil {
		t.Fatal("schema changed", e)
	}
}
