//go:build integration

package database

import (
	"context"
	"errors"
	"fmt"
	"iChat/messaging"
	"iChat/migrations"
	"iChat/models"
	"iChat/tests/testenv"
	"iChat/utils"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

func TestMessagePermissionsIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, err := testenv.Start(ctx, filepath.Join("..", "docker-compose.test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("message permissions fixture run=%s", env.RunID())
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := env.Close(closeCtx); err != nil {
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
	// Only the existing identity models are created for this test fixture. The
	// versioned reliable message table must never be passed to AutoMigrate.
	if err := db.WithContext(ctx).AutoMigrate(&models.User{}, &models.Group{}, &models.Relation{}); err != nil {
		t.Fatal(err)
	}
	commandExec(t, ctx, pool, "CREATE TABLE command_test_effect (command_id char(64) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY, content varbinary(4096) NOT NULL) ENGINE=InnoDB")
	var users []models.User
	for i := 0; i < 20; i++ {
		user := models.User{Name: fmt.Sprintf("permission-%d", i), Phone: fmt.Sprintf("permission-phone-%d", i), Identifier: fmt.Sprintf("permission-id-%d", i), Password: "fixture"}
		if err := db.WithContext(ctx).Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		users = append(users, user)
	}
	groups, relations := NewGroupManager(db), NewRelationManager(db)
	newGroup := func(name string, owner models.User) models.Group {
		t.Helper()
		if err := groups.CreateGroup(ctx, name, owner.ID, "fixture"); err != nil {
			t.Fatal(err)
		}
		var group models.Group
		if err := db.WithContext(ctx).Where("name = ?", name).Take(&group).Error; err != nil {
			t.Fatal(err)
		}
		return group
	}
	for _, pair := range [][2]int{{0, 1}, {4, 5}, {11, 12}} {
		if err := relations.AddFriendByPhone(ctx, users[pair[0]].ID, users[pair[1]].Phone); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.WithContext(ctx).Create(&models.Relation{OwnerId: users[2].ID, TargetId: users[3].ID, Type: models.FriendRelation}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Where("owner_id=? AND target_id=? AND type=?", users[5].ID, users[4].ID, models.FriendRelation).Delete(&models.Relation{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Delete(&users[6]).Error; err != nil {
		t.Fatal(err)
	}
	group := newGroup("allowed", users[8])
	if err := groups.JoinGroup(ctx, users[9].ID, group.ID); err != nil {
		t.Fatal(err)
	}
	deletedGroup := newGroup("deleted", users[8])
	if err := groups.DeleteGroup(ctx, users[8].ID, deletedGroup.ID); err != nil {
		t.Fatal(err)
	}
	noOwnerMembership := newGroup("owner-is-not-membership", users[8])
	if err := db.WithContext(ctx).Where("owner_id=? AND target_id=? AND type=?", users[8].ID, noOwnerMembership.ID, models.GroupRelation).Delete(&models.Relation{}).Error; err != nil {
		t.Fatal(err)
	}
	repo, err := NewMessageRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	makeCommand := func(sender uint, client uint64, target uint, kind messaging.TargetType) messaging.Command {
		t.Helper()
		cmd, err := messaging.Normalize(uint64(sender), messaging.SendRequest{ClientMessageID: fmt.Sprintf("00000000-0000-4000-8000-%012x", client), TargetType: kind, TargetID: strconv.FormatUint(uint64(target), 10), Media: messaging.Text, Content: "permission fixture"})
		if err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	assertRejected := func(t *testing.T, cmd messaging.Command, reason string) CommandExecution {
		t.Helper()
		outcome, err := repo.ExecuteAuthorizedCommand(ctx, cmd, func(context.Context, CommandTransaction, messaging.Command) (CommandDecision, error) {
			t.Error("permission rejection invoked message writer")
			return CommandDecision{}, errors.New("unexpected writer")
		})
		if err != nil || outcome.Result == nil || outcome.Result.State != CommandRejected || outcome.Result.ReasonCode != reason {
			t.Fatalf("rejected=%+v err=%v want=%s", outcome, err, reason)
		}
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message WHERE id=?", 0, cmd.CommandID)
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM command_test_effect WHERE command_id=?", 0, cmd.CommandID)
		return outcome
	}
	assertReplay := func(t *testing.T, cmd messaging.Command, original CommandExecution) {
		t.Helper()
		outcome, err := repo.ExecuteAuthorizedCommand(ctx, cmd, func(context.Context, CommandTransaction, messaging.Command) (CommandDecision, error) {
			t.Error("replay re-executed permission or message writer")
			return CommandDecision{}, errors.New("unexpected replay writer")
		})
		if err != nil || outcome.Disposition != CommandReplayed || outcome.Result == nil || !reflect.DeepEqual(outcome.Result, original.Result) {
			t.Fatalf("replay=%+v err=%v original=%+v", outcome, err, original)
		}
	}

	private := makeCommand(users[0].ID, 101, users[1].ID, messaging.Private)
	privateResult, err := repo.ExecuteAuthorizedCommand(ctx, private, permissionMessageFixture)
	if err != nil || privateResult.Result == nil || privateResult.Result.State != CommandPersisted {
		t.Fatalf("allowed private=%+v err=%v", privateResult, err)
	}
	groupCommand := makeCommand(users[9].ID, 102, group.ID, messaging.Group)
	groupResult, err := repo.ExecuteAuthorizedCommand(ctx, groupCommand, permissionMessageFixture)
	if err != nil || groupResult.Result == nil || groupResult.Result.State != CommandPersisted {
		t.Fatalf("allowed group=%+v err=%v", groupResult, err)
	}
	t.Run("rejection_matrix", func(t *testing.T) {
		for i, tc := range []struct {
			sender, target uint
			kind           messaging.TargetType
			reason         string
		}{
			{users[0].ID, users[0].ID, messaging.Private, ReasonSelfSend},
			{users[2].ID, users[3].ID, messaging.Private, ReasonNotFriends},
			{users[4].ID, users[5].ID, messaging.Private, ReasonNotFriends},
			{users[0].ID, users[7].ID, messaging.Private, ReasonNotFriends},
			{users[0].ID, 999999, messaging.Private, ReasonTargetNotFound},
			{users[0].ID, users[6].ID, messaging.Private, ReasonTargetNotFound},
			{999999, users[1].ID, messaging.Private, ReasonSenderNotFound},
			{users[6].ID, group.ID, messaging.Group, ReasonSenderNotFound},
			{users[10].ID, group.ID, messaging.Group, ReasonNotGroupMember},
			{users[8].ID, noOwnerMembership.ID, messaging.Group, ReasonNotGroupMember},
			{users[8].ID, deletedGroup.ID, messaging.Group, ReasonTargetNotFound},
			{users[8].ID, 999999, messaging.Group, ReasonTargetNotFound},
		} {
			assertRejected(t, makeCommand(tc.sender, uint64(200+i), tc.target, tc.kind), tc.reason)
		}
	})

	t.Run("replay_is_independent_of_current_permissions", func(t *testing.T) {
		if err := revokeFriendFixture(ctx, db, users[0].ID, users[1].ID); err != nil {
			t.Fatal(err)
		}
		assertReplay(t, private, privateResult)
		conflict, err := messaging.Normalize(uint64(users[0].ID), messaging.SendRequest{ClientMessageID: private.ClientMessageID, TargetType: messaging.Private, TargetID: private.TargetID, Media: messaging.Text, Content: "different"})
		if err != nil {
			t.Fatal(err)
		}
		outcome, err := repo.ExecuteAuthorizedCommand(ctx, conflict, permissionMessageFixture)
		if err != nil || outcome.Disposition != CommandConflict || outcome.Result != nil {
			t.Fatalf("conflict changed original: %+v %v", outcome, err)
		}
		rejectedCmd := makeCommand(users[0].ID, 301, users[1].ID, messaging.Private)
		rejected := assertRejected(t, rejectedCmd, ReasonNotFriends)
		commandExec(t, ctx, pool, "UPDATE relation SET deleted_at=NULL WHERE type=? AND ((owner_id=? AND target_id=?) OR (owner_id=? AND target_id=?))", models.FriendRelation, users[0].ID, users[1].ID, users[1].ID, users[0].ID)
		assertReplay(t, rejectedCmd, rejected)
		// A fresh client key gets the new permission state. A test-only business
		// terminal avoids allocating a second production sequence in P2.4.
		outcome, err = repo.ExecuteAuthorizedCommand(ctx, makeCommand(users[0].ID, 302, users[1].ID, messaging.Private), rejectingTestHandler("TEST_ALLOWED"))
		if err != nil || outcome.Result == nil || outcome.Result.ReasonCode != "TEST_ALLOWED" {
			t.Fatalf("fresh key did not see restored friendship: %+v %v", outcome, err)
		}
		if err := groups.DeleteGroup(ctx, users[8].ID, group.ID); err != nil {
			t.Fatal(err)
		}
		assertReplay(t, groupCommand, groupResult)
		assertRejected(t, makeCommand(users[9].ID, 303, group.ID, messaging.Group), ReasonTargetNotFound)
	})

	t.Run("private_send_serializes_revocation", func(t *testing.T) {
		cmd := makeCommand(users[11].ID, 401, users[12].ID, messaging.Private)
		outcome := holdAuthorizedCommand(t, ctx, repo, cmd, func() {
			blocked, stop := context.WithTimeout(ctx, 500*time.Millisecond)
			defer stop()
			err := revokeFriendFixture(blocked, db, users[11].ID, users[12].ID)
			if err == nil || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("friend revocation did not wait for send: %v", err)
			}
		})
		if err := revokeFriendFixture(ctx, db, users[11].ID, users[12].ID); err != nil {
			t.Fatal(err)
		}
		assertReplay(t, cmd, outcome)
		assertRejected(t, makeCommand(users[11].ID, 402, users[12].ID, messaging.Private), ReasonNotFriends)
	})

	t.Run("group_send_serializes_delete_and_join", func(t *testing.T) {
		group := newGroup("held-send", users[14])
		cmd := makeCommand(users[14].ID, 403, group.ID, messaging.Group)
		outcome := holdAuthorizedCommand(t, ctx, repo, cmd, func() {
			for _, operation := range []func(context.Context) error{
				func(ctx context.Context) error { return groups.DeleteGroup(ctx, users[14].ID, group.ID) },
				func(ctx context.Context) error { return groups.JoinGroup(ctx, users[15].ID, group.ID) },
			} {
				blocked, stop := context.WithTimeout(ctx, 500*time.Millisecond)
				err := operation(blocked)
				stop()
				if err == nil || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("group writer did not wait for send: %v", err)
				}
			}
		})
		if err := groups.JoinGroup(ctx, users[15].ID, group.ID); err != nil {
			t.Fatalf("join after releasing permission locks: %v", err)
		}
		if err := groups.DeleteGroup(ctx, users[14].ID, group.ID); err != nil {
			t.Fatal(err)
		}
		assertReplay(t, cmd, outcome)
		assertRejected(t, makeCommand(users[14].ID, 404, group.ID, messaging.Group), ReasonTargetNotFound)
		if err := groups.JoinGroup(ctx, users[16].ID, group.ID); !errors.Is(err, ErrGroupNotFound) {
			t.Fatalf("join accepted a deleted group: %v", err)
		}
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM relation WHERE owner_id=? AND target_id=? AND type=?", 0, users[16].ID, group.ID, models.GroupRelation)
	})

	t.Run("membership_change_serializes_with_send", func(t *testing.T) {
		group := newGroup("membership-revoke", users[14])
		if err := groups.JoinGroup(ctx, users[15].ID, group.ID); err != nil {
			t.Fatal(err)
		}
		cmd := makeCommand(users[15].ID, 405, group.ID, messaging.Group)
		holdAuthorizedCommand(t, ctx, repo, cmd, func() {
			blocked, stop := context.WithTimeout(ctx, 500*time.Millisecond)
			defer stop()
			// No leave endpoint exists yet. This test SQL changes the exact
			// membership row to verify its shared lock lasts through commit.
			_, err := pool.ExecContext(blocked, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE owner_id=? AND target_id=? AND type=?", users[15].ID, group.ID, models.GroupRelation)
			if err == nil || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("membership removal did not wait for send: %v", err)
			}
		})
		commandExec(t, ctx, pool, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE owner_id=? AND target_id=? AND type=?", users[15].ID, group.ID, models.GroupRelation)
		assertRejected(t, makeCommand(users[15].ID, 406, group.ID, messaging.Group), ReasonNotGroupMember)
	})

	t.Run("permission_sql_failure_rolls_back_claim", func(t *testing.T) {
		cmd := makeCommand(users[0].ID, 501, users[1].ID, messaging.Private)
		commandExec(t, ctx, pool, "RENAME TABLE relation TO permission_unavailable")
		outcome, err := repo.ExecuteAuthorizedCommand(ctx, cmd, permissionMessageFixture)
		commandExec(t, ctx, pool, "RENAME TABLE permission_unavailable TO relation")
		var mysqlError *mysqlDriver.MySQLError
		if err == nil || !errors.As(err, &mysqlError) || mysqlError.Number != 1146 || outcome.Result != nil {
			t.Fatalf("SQL error became a rejection: %+v %v", outcome, err)
		}
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message_command WHERE command_id=?", 0, cmd.CommandID)
		outcome, err = repo.ExecuteAuthorizedCommand(ctx, cmd, rejectingTestHandler("TEST_ALLOWED"))
		if err != nil || outcome.Disposition != CommandApplied {
			t.Fatalf("SQL recovery did not allow original key: %+v %v", outcome, err)
		}
	})

	t.Run("contact_writer_validation_and_reverse_competition", func(t *testing.T) {
		if err := groups.CreateGroup(ctx, "orphan-owner", 999999, "fixture"); !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("missing owner=%v", err)
		}
		if err := groups.JoinGroup(ctx, users[0].ID, 999999); !errors.Is(err, ErrGroupNotFound) {
			t.Fatalf("missing group=%v", err)
		}
		if err := groups.JoinGroup(ctx, 999999, noOwnerMembership.ID); !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("missing join user=%v", err)
		}
		start, results := make(chan struct{}), make(chan error, 16)
		for i := 0; i < 16; i++ {
			from, to := users[18], users[19]
			if i%2 == 1 {
				from, to = to, from
			}
			go func(from, to models.User) {
				<-start
				results <- relations.AddFriendByPhone(ctx, from.ID, to.Phone)
			}(from, to)
		}
		close(start)
		created := 0
		for range 16 {
			if err := <-results; err == nil {
				created++
			} else if !errors.Is(err, ErrRelationExists) {
				t.Errorf("reverse friendship competition: %v", err)
			}
		}
		if created != 1 {
			t.Fatalf("friendship commits=%d", created)
		}
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM relation WHERE type=? AND ((owner_id=? AND target_id=?) OR (owner_id=? AND target_id=?)) AND deleted_at IS NULL", 2, models.FriendRelation, users[18].ID, users[19].ID, users[19].ID, users[18].ID)
	})
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatalf("permission path changed reliable schema: %v", err)
	}
}

// Test-only single-message conversations isolate permission behavior. Sequence
// allocation for general production traffic and Outbox remains P2.5.
func permissionMessageFixture(ctx context.Context, tx CommandTransaction, command messaging.Command) (CommandDecision, error) {
	if command.TargetType == messaging.Private {
		return persistedTestHandler(ctx, tx, command)
	}
	if err := insertCommandTestEffect(ctx, tx, command); err != nil {
		return CommandDecision{}, err
	}
	sender, _ := strconv.ParseUint(command.SenderID, 10, 64)
	target, _ := strconv.ParseUint(command.TargetID, 10, 64)
	if _, err := tx.ExecContext(ctx, "INSERT INTO conversation (id,type,group_id,last_seq,updated_at) VALUES (?,'group',?,1,UTC_TIMESTAMP(6))", command.ConversationID, target); err != nil {
		return CommandDecision{}, err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO message
 (id,client_message_id,sender_id,receiver_id,type,media,content,conversation_id,seq,server_created_at)
 VALUES (?,?,?,?,'group','text',?,?,1,UTC_TIMESTAMP(6))`, command.CommandID, command.ClientMessageID, sender, target, command.Content, command.ConversationID)
	return CommandDecision{State: CommandPersisted}, err
}

func holdAuthorizedCommand(t *testing.T, ctx context.Context, repo *MessageRepository, command messaging.Command, whileHeld func()) CommandExecution {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	type result struct {
		outcome CommandExecution
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := repo.ExecuteAuthorizedCommand(ctx, command, func(ctx context.Context, tx CommandTransaction, cmd messaging.Command) (CommandDecision, error) {
			close(entered)
			select {
			case <-release:
				return permissionMessageFixture(ctx, tx, cmd)
			case <-ctx.Done():
				return CommandDecision{}, ctx.Err()
			}
		})
		done <- result{outcome, err}
	}()
	joined := false
	defer func() {
		open()
		if !joined {
			select {
			case <-done:
			case <-ctx.Done():
			}
		}
	}()
	select {
	case <-entered:
	case res := <-done:
		joined = true
		t.Fatalf("permission handler did not enter: %v", res.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	whileHeld()
	open()
	res := <-done
	joined = true
	if res.err != nil || res.outcome.Result == nil || res.outcome.Result.State != CommandPersisted {
		t.Fatalf("held send=%+v err=%v", res.outcome, res.err)
	}
	return res.outcome
}

func revokeFriendFixture(ctx context.Context, db *gorm.DB, a, b uint) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockContactUsers(tx, a, b); err != nil {
			return err
		}
		low, high := a, b
		if low > high {
			low, high = high, low
		}
		for _, pair := range [][2]uint{{low, high}, {high, low}} {
			if err := tx.Where("owner_id=? AND target_id=? AND type=?", pair[0], pair[1], models.FriendRelation).Delete(&models.Relation{}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
