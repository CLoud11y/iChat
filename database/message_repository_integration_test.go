//go:build integration

package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iChat/messaging"
	"iChat/migrations"
	"iChat/tests/testenv"
	"iChat/utils"
	"net"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

func TestCommandRepositoryIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, err := testenv.Start(ctx, filepath.Join("..", "docker-compose.test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("command repository fixture run=%s", env.RunID())
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
	// A test-only transactional effect records whether handlers execute once or
	// roll back. It is not part of the production schema.
	commandExec(t, ctx, pool, "CREATE TABLE command_test_effect (command_id char(64) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY, content varbinary(4096) NOT NULL) ENGINE=InnoDB")
	repo, err := NewMessageRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	// Ownership must also work with CLIENT_FOUND_ROWS and a non-UTC DSN location.
	options := mysqlDriver.NewConfig()
	options.User, options.Passwd, options.DBName = conf.MYSQL.User, conf.MYSQL.Password, conf.MYSQL.DbName
	options.Net, options.Addr = "tcp", net.JoinHostPort(conf.MYSQL.Host, conf.MYSQL.Port)
	options.ParseTime, options.ClientFoundRows = true, true
	options.Loc = time.FixedZone("test-offset", -7*60*60)
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
	repositories := []*MessageRepository{repo, otherRepo}

	t.Run("persisted_replay_and_conflict", func(t *testing.T) {
		command := reliableTestCommand(t, 1, 1, "2", "  你好🙂\x00  ")
		var calls atomic.Int64
		handler := func(ctx context.Context, tx CommandTransaction, cmd messaging.Command) (CommandDecision, error) {
			calls.Add(1)
			if err := insertCommandTestEffect(ctx, tx, cmd); err != nil {
				return CommandDecision{}, err
			}
			// Fixed seq=1 is only a test fixture for this one new conversation.
			// Actual locked sequence allocation and Outbox are P2.5.
			if _, err := tx.ExecContext(ctx, "INSERT INTO conversation (id,type,private_user_a,private_user_b,last_seq,updated_at) VALUES (?,'private',1,2,1,UTC_TIMESTAMP(6))", cmd.ConversationID); err != nil {
				return CommandDecision{}, err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO message
 (id,client_message_id,sender_id,receiver_id,type,media,content,conversation_id,seq,server_created_at)
 VALUES (?,?,1,2,'private','text',?,?,1,UTC_TIMESTAMP(6))`, cmd.CommandID, cmd.ClientMessageID, cmd.Content, cmd.ConversationID)
			return CommandDecision{State: CommandPersisted}, err
		}
		applied, err := repo.ExecuteCommand(ctx, command, handler)
		if err != nil || applied.Disposition != CommandApplied || applied.Result == nil || applied.Result.State != CommandPersisted {
			t.Fatalf("applied=%+v err=%v", applied, err)
		}
		original := *applied.Result
		for _, candidate := range []messaging.Command{command, reliableTestCommand(t, 1, 1, "2", "changed"), reliableTestCommand(t, 1, 1, "3", command.Content)} {
			outcome, err := otherRepo.ExecuteCommand(ctx, candidate, handler)
			if err != nil {
				t.Fatal(err)
			}
			if candidate.PayloadHash == command.PayloadHash {
				if outcome.Disposition != CommandReplayed || outcome.Result == nil || !reflect.DeepEqual(*outcome.Result, original) {
					t.Fatalf("replay did not preserve canonical result: %+v", outcome)
				}
			} else if outcome.Disposition != CommandConflict || outcome.Result != nil {
				t.Fatalf("conflicting request received original success: %+v", outcome)
			}
		}
		metadataChanged := command
		metadataChanged.ClientSentAt = "2026-10-11T00:00:00.123456789Z"
		outcome, err := repo.ExecuteCommand(ctx, metadataChanged, handler)
		if err != nil || outcome.Result == nil || !reflect.DeepEqual(*outcome.Result, original) {
			t.Fatalf("metadata-only retry changed result: %+v err=%v", outcome, err)
		}
		if calls.Load() != 1 {
			t.Fatalf("handler executed %d times", calls.Load())
		}
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message WHERE id=?", 1, command.CommandID)
		var content, createdText string
		if err := pool.QueryRowContext(ctx, "SELECT content FROM message WHERE id=?", command.CommandID).Scan(&content); err != nil || content != command.Content {
			t.Fatalf("original content changed: %q err=%v", content, err)
		}
		if err := pool.QueryRowContext(ctx, "SELECT DATE_FORMAT(created_at,'%Y-%m-%d %H:%i:%s.%f') FROM message_command WHERE command_id=?", command.CommandID).Scan(&createdText); err != nil {
			t.Fatal(err)
		}
		if original.CreatedAt.Location() != time.UTC || original.CreatedAt.Format("2006-01-02 15:04:05.000000") != createdText || original.CompletedAt.Nanosecond()%1000 != 0 {
			t.Fatalf("UTC/microsecond timestamp mismatch: %+v text=%s", original, createdText)
		}
	})

	t.Run("rejected_result_is_terminal", func(t *testing.T) {
		command := reliableTestCommand(t, 3, 2, "4", "hello")
		outcome, err := repo.ExecuteCommand(ctx, command, rejectingTestHandler("NOT_FRIENDS"))
		if err != nil || outcome.Result == nil || outcome.Result.State != CommandRejected || outcome.Result.ReasonCode != "NOT_FRIENDS" {
			t.Fatalf("rejection=%+v err=%v", outcome, err)
		}
		original := *outcome.Result
		outcome, err = otherRepo.ExecuteCommand(ctx, command, func(context.Context, CommandTransaction, messaging.Command) (CommandDecision, error) {
			t.Fatal("permission change re-executed a rejected command")
			return CommandDecision{State: CommandPersisted}, nil
		})
		if err != nil || outcome.Disposition != CommandReplayed || outcome.Result == nil || !reflect.DeepEqual(*outcome.Result, original) {
			t.Fatalf("rejected replay=%+v err=%v", outcome, err)
		}
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message WHERE id=?", 0, command.CommandID)
	})

	t.Run("independent_keys_and_uint64_senders", func(t *testing.T) {
		for _, command := range []messaging.Command{
			reliableTestCommand(t, 5, 3, "6", "same content"),
			reliableTestCommand(t, 5, 4, "6", "same content"),
			reliableTestCommand(t, 18446744073709551615, 3, "6", "same content"),
		} {
			outcome, err := repo.ExecuteCommand(ctx, command, rejectingTestHandler("TARGET_NOT_FOUND"))
			if err != nil || outcome.Disposition != CommandApplied || outcome.Result == nil || strconv.FormatUint(outcome.Result.SenderID, 10) != command.SenderID {
				t.Fatalf("independent key=%+v err=%v", outcome, err)
			}
		}
	})

	t.Run("parallel_same_key", func(t *testing.T) {
		command := reliableTestCommand(t, 7, 5, "8", "concurrent")
		commands := make([]messaging.Command, 16)
		for i := range commands {
			commands[i] = command
		}
		assertCommandCompetition(t, ctx, repositories, commands, persistedTestHandler, 15, 0)
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message_command WHERE command_id=?", 1, command.CommandID)
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM command_test_effect WHERE command_id=?", 1, command.CommandID)
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message WHERE id=?", 1, command.CommandID)
	})

	t.Run("parallel_conflicting_hashes", func(t *testing.T) {
		commands := make([]messaging.Command, 16)
		for i := range commands {
			content := "red"
			if i%2 == 1 {
				content = "blue"
			}
			commands[i] = reliableTestCommand(t, 9, 6, "10", content)
		}
		assertCommandCompetition(t, ctx, repositories, commands, persistedTestHandler, 7, 8)
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM command_test_effect WHERE command_id=?", 1, commands[0].CommandID)
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message WHERE id=?", 1, commands[0].CommandID)
	})

	t.Run("uncommitted_claim_and_rollback_recovery", func(t *testing.T) {
		command := reliableTestCommand(t, 11, 7, "12", "recover")
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseHandler()
		firstDone := make(chan error, 1)
		cause := errors.New("injected rollback after transactional side effect")
		go func() {
			_, err := repo.ExecuteCommand(ctx, command, func(ctx context.Context, tx CommandTransaction, cmd messaging.Command) (CommandDecision, error) {
				if err := insertCommandTestEffect(ctx, tx, cmd); err != nil {
					return CommandDecision{}, err
				}
				close(entered)
				select {
				case <-release:
					return CommandDecision{}, cause
				case <-ctx.Done():
					return CommandDecision{}, ctx.Err()
				}
			})
			firstDone <- err
		}()
		select {
		case <-entered:
		case err := <-firstDone:
			t.Fatalf("first handler failed to enter: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		// Other connections see neither the provisional claim nor its SQL effect.
		commandCount(t, ctx, otherPool, "SELECT COUNT(*) FROM message_command WHERE command_id=?", 0, command.CommandID)
		commandCount(t, ctx, otherPool, "SELECT COUNT(*) FROM command_test_effect WHERE command_id=?", 0, command.CommandID)
		waitCtx, waitCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		outcome, err := otherRepo.ExecuteCommand(waitCtx, command, func(context.Context, CommandTransaction, messaging.Command) (CommandDecision, error) {
			t.Error("waiter executed before the first transaction ended")
			return CommandDecision{State: CommandRejected, ReasonCode: "NOT_FRIENDS"}, nil
		})
		waitCancel()
		if err == nil || outcome.Result != nil || outcome.Disposition != "" {
			t.Fatalf("waiter treated an uncommitted claim as terminal: %+v err=%v", outcome, err)
		}
		releaseHandler()
		if err := <-firstDone; !errors.Is(err, cause) {
			t.Fatalf("first transaction error=%v", err)
		}
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message_command WHERE command_id=?", 0, command.CommandID)
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM command_test_effect WHERE command_id=?", 0, command.CommandID)
		outcome, err = otherRepo.ExecuteCommand(ctx, command, rejectingTestHandler("NOT_FRIENDS"))
		if err != nil || outcome.Disposition != CommandApplied {
			t.Fatalf("rollback did not release logical key: %+v err=%v", outcome, err)
		}
	})

	t.Run("handler_failures_leave_no_terminal_or_effect", func(t *testing.T) {
		for i, stage := range []string{"error", "panic", "invalid_decision", "sql_error", "cancel", "tampered_claim"} {
			t.Run(stage, func(t *testing.T) {
				command := reliableTestCommand(t, 13, uint64(20+i), "14", stage)
				handlerCtx, handlerCancel := context.WithCancel(ctx)
				defer handlerCancel()
				cause := errors.New("injected business handler error")
				var outcome CommandExecution
				var recovered any
				func() {
					defer func() { recovered = recover() }()
					outcome, err = repo.ExecuteCommand(handlerCtx, command, func(ctx context.Context, tx CommandTransaction, cmd messaging.Command) (CommandDecision, error) {
						if err := insertCommandTestEffect(ctx, tx, cmd); err != nil {
							return CommandDecision{}, err
						}
						switch stage {
						case "error":
							return CommandDecision{}, cause
						case "panic":
							panic(cause)
						case "invalid_decision":
							return CommandDecision{State: "processing"}, nil
						case "sql_error":
							_, err := tx.ExecContext(ctx, "INSERT INTO no_such_table VALUES (1)")
							return CommandDecision{}, err
						case "cancel":
							handlerCancel()
						case "tampered_claim":
							_, err := tx.ExecContext(ctx, "UPDATE message_command SET reason_code='WRONG_RESULT' WHERE command_id=?", cmd.CommandID)
							if err != nil {
								return CommandDecision{}, err
							}
						}
						return CommandDecision{State: CommandRejected, ReasonCode: "NOT_FRIENDS"}, nil
					})
				}()
				if stage == "panic" {
					if recovered != cause {
						t.Fatalf("panic=%v", recovered)
					}
				} else if err == nil || outcome.Result != nil || outcome.Disposition != "" {
					t.Fatalf("handler failure returned success: %+v err=%v", outcome, err)
				}
				commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message_command WHERE command_id=?", 0, command.CommandID)
				commandCount(t, ctx, pool, "SELECT COUNT(*) FROM command_test_effect WHERE command_id=?", 0, command.CommandID)
				outcome, err = repo.ExecuteCommand(ctx, command, rejectingTestHandler("NOT_FRIENDS"))
				if err != nil || outcome.Disposition != CommandApplied {
					t.Fatalf("recovery=%+v err=%v", outcome, err)
				}
			})
		}
	})

	t.Run("other_unique_key_collision_is_not_replay", func(t *testing.T) {
		command := reliableTestCommand(t, 15, 8, "16", "collision")
		commandExec(t, ctx, pool, `INSERT INTO message_command
 (sender_id,client_message_id,command_id,payload_hash,state,reason_code,created_at,completed_at)
 VALUES (99,?,?,?,'rejected','NOT_FRIENDS',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`, command.ClientMessageID, command.CommandID, command.PayloadHash)
		outcome, err := repo.ExecuteCommand(ctx, command, func(context.Context, CommandTransaction, messaging.Command) (CommandDecision, error) {
			t.Fatal("identity collision invoked handler")
			return CommandDecision{}, nil
		})
		if !errors.Is(err, ErrCommandInvariant) || outcome.Result != nil {
			t.Fatalf("unexpected unique key treated as replay: %+v err=%v", outcome, err)
		}
		commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message_command WHERE command_id=? AND sender_id=99", 1, command.CommandID)
	})

	t.Run("committed_claim_marker_is_corruption", func(t *testing.T) {
		command := reliableTestCommand(t, 17, 9, "18", "bad marker")
		commandExec(t, ctx, pool, `INSERT INTO message_command
 (sender_id,client_message_id,command_id,payload_hash,state,reason_code,created_at,completed_at)
 VALUES (17,?,?,?,'rejected',?,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`, command.ClientMessageID, command.CommandID, command.PayloadHash, commandClaimPrefix+"bad")
		outcome, err := repo.ExecuteCommand(ctx, command, func(context.Context, CommandTransaction, messaging.Command) (CommandDecision, error) {
			t.Fatal("committed marker became a business result")
			return CommandDecision{}, nil
		})
		if !errors.Is(err, ErrCommandInvariant) || outcome.Result != nil {
			t.Fatalf("committed marker=%+v err=%v", outcome, err)
		}
		// This intentionally corrupt test row is removed only in this run's schema.
		commandExec(t, ctx, pool, "DELETE FROM message_command WHERE command_id=?", command.CommandID)
	})
	commandCount(t, ctx, pool, "SELECT COUNT(*) FROM message_command WHERE reason_code LIKE '__claim:%' OR state='processing'", 0)
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatalf("repository changed versioned schema: %v", err)
	}
}

func reliableTestCommand(t *testing.T, sender, client uint64, target, content string) messaging.Command {
	t.Helper()
	command, err := messaging.Normalize(sender, messaging.SendRequest{
		ClientMessageID: fmt.Sprintf("00000000-0000-4000-8000-%012x", client), TargetType: messaging.Private,
		TargetID: target, Media: messaging.Text, Content: content,
	})
	if err != nil {
		t.Fatal(err)
	}
	return command
}

func insertCommandTestEffect(ctx context.Context, tx CommandTransaction, command messaging.Command) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO command_test_effect (command_id,content) VALUES (?,?)", command.CommandID, []byte(command.Content))
	return err
}

func rejectingTestHandler(reason string) CommandHandler {
	return func(ctx context.Context, tx CommandTransaction, command messaging.Command) (CommandDecision, error) {
		return CommandDecision{State: CommandRejected, ReasonCode: reason}, insertCommandTestEffect(ctx, tx, command)
	}
}

// One fixed seq=1 message in a new conversation demonstrates exactly-once
// transactional effects. It does not implement the P2.5 sequence allocator.
func persistedTestHandler(ctx context.Context, tx CommandTransaction, command messaging.Command) (CommandDecision, error) {
	if err := insertCommandTestEffect(ctx, tx, command); err != nil {
		return CommandDecision{}, err
	}
	sender, _ := strconv.ParseUint(command.SenderID, 10, 64)
	target, _ := strconv.ParseUint(command.TargetID, 10, 64)
	low, high := sender, target
	if low > high {
		low, high = high, low
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO conversation (id,type,private_user_a,private_user_b,last_seq,updated_at) VALUES (?,'private',?,?,1,UTC_TIMESTAMP(6))", command.ConversationID, low, high); err != nil {
		return CommandDecision{}, err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO message
 (id,client_message_id,sender_id,receiver_id,type,media,content,conversation_id,seq,server_created_at)
 VALUES (?,?,?,?,'private','text',?,?,1,UTC_TIMESTAMP(6))`, command.CommandID, command.ClientMessageID, sender, target, command.Content, command.ConversationID)
	return CommandDecision{State: CommandPersisted}, err
}

func assertCommandCompetition(t *testing.T, ctx context.Context, repositories []*MessageRepository, commands []messaging.Command, handler CommandHandler, replayCount, conflictCount int) {
	t.Helper()
	var calls atomic.Int64
	start := make(chan struct{})
	type callResult struct {
		command messaging.Command
		outcome CommandExecution
		err     error
	}
	results := make(chan callResult, len(commands))
	for i, command := range commands {
		go func(repo *MessageRepository, command messaging.Command) {
			<-start
			outcome, err := repo.ExecuteCommand(ctx, command, func(ctx context.Context, tx CommandTransaction, cmd messaging.Command) (CommandDecision, error) {
				calls.Add(1)
				return handler(ctx, tx, cmd)
			})
			results <- callResult{command: command, outcome: outcome, err: err}
		}(repositories[i%len(repositories)], command)
	}
	close(start)
	counts := make(map[CommandDisposition]int)
	var canonical *CommandResult
	var collected []callResult
	for range commands {
		select {
		case result := <-results:
			collected = append(collected, result)
			if result.err != nil {
				t.Errorf("concurrent execution: %v", result.err)
				continue
			}
			counts[result.outcome.Disposition]++
			if result.outcome.Disposition == CommandApplied {
				canonical = result.outcome.Result
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if counts[CommandApplied] != 1 || counts[CommandReplayed] != replayCount || counts[CommandConflict] != conflictCount || calls.Load() != 1 || canonical == nil {
		t.Fatalf("counts=%v calls=%d canonical=%+v", counts, calls.Load(), canonical)
	}
	for _, result := range collected {
		if result.command.PayloadHash == canonical.PayloadHash {
			if result.outcome.Result == nil || !reflect.DeepEqual(*result.outcome.Result, *canonical) {
				t.Fatalf("same hash changed canonical result: %+v", result.outcome)
			}
		} else if result.outcome.Disposition != CommandConflict || result.outcome.Result != nil {
			t.Fatalf("different hash returned success: %+v", result.outcome)
		}
	}
}

func commandExec(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func commandCount(t *testing.T, ctx context.Context, db *sql.DB, query string, want int, args ...any) {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil || count != want {
		t.Fatalf("query %s count=%d want=%d err=%v", strings.TrimSpace(query), count, want, err)
	}
}
