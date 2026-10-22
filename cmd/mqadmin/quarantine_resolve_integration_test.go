//go:build integration

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/migrations"
	"iChat/models"
	"iChat/tests/testenv"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestQuarantineResolveCLIIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, err := testenv.Start(ctx, filepath.Join("..", "..", "docker-compose.test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("quarantine resolution fixture run=%s", env.RunID())
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := env.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	conf, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "resolve.yml")
	contents := fmt.Sprintf("MYSQL:\n  user: %q\n  password: %q\n  host: %q\n  port: %q\n  dbname: %q\nRABBITMQ: invalid\nRUNTIME:\n  role: invalid\nREDIS: invalid\n", conf.MYSQL.User, conf.MYSQL.Password, conf.MYSQL.Host, conf.MYSQL.Port, conf.MYSQL.DbName)
	if err := os.WriteFile(file, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"USER": conf.MYSQL.User, "PASSWORD": conf.MYSQL.Password, "HOST": conf.MYSQL.Host, "PORT": conf.MYSQL.Port, "DBNAME": conf.MYSQL.DbName} {
		t.Setenv("ICHAT_MYSQL_"+key, value)
	}
	t.Setenv("ICHAT_CONFIG_FILE", file)
	options, err := loadQuarantineMySQL(file)
	if err != nil {
		t.Fatal(err)
	}
	options.Loc = time.FixedZone("resolution-driver-offset", 13*60*60)
	options.Params["time_zone"], options.Params["transaction_isolation"] = "'+05:30'", "'READ-COMMITTED'"
	connector, err := mysqlDriver.NewConnector(options)
	if err != nil {
		t.Fatal("configure fixture SQL failed")
	}
	pool := sql.OpenDB(connector)
	pool.SetMaxOpenConns(12)
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	var schema string
	if err := pool.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); err != nil || schema != "ichat_test_"+env.RunID() {
		t.Fatal("refusing writes outside owned schema")
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: pool}), &gorm.Config{})
	if err != nil {
		t.Fatal("open fixture account store failed")
	}
	if err := db.WithContext(ctx).AutoMigrate(&models.User{}, &models.Relation{}, &models.Group{}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint{12, 34} {
		user := models.User{Model: gorm.Model{ID: id}, Identifier: fmt.Sprintf("resolution-%d", id), Phone: fmt.Sprintf("resolution-%d", id), Name: "resolution"}
		if err := db.WithContext(ctx).Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	}
	repo, _ := database.NewQuarantineRepository(pool)
	commands, _ := database.NewMessageRepository(pool)
	inputFor := func(t *testing.T, label string, n int) rabbitmq.QuarantineInput {
		return adminQuarantineInput(t, label, true, false, fmt.Sprintf("00000000-0000-4000-8000-%012d", n))
	}
	save := func(t *testing.T, input rabbitmq.QuarantineInput) database.QuarantineDetail {
		t.Helper()
		if _, err := repo.Save(ctx, input); err != nil {
			t.Fatal(err)
		}
		detail, err := repo.Get(ctx, input.Envelope().TransportID())
		if err != nil {
			t.Fatal(err)
		}
		return detail
	}
	commandFor := func(t *testing.T, input rabbitmq.QuarantineInput) messaging.Command {
		t.Helper()
		source, _ := input.Envelope().Source()
		wire, err := messaging.DecodeEnvelope(source.Body)
		if err != nil {
			t.Fatal(err)
		}
		c, err := messaging.ValidateEnvelope(wire)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	persist := func(t *testing.T, input rabbitmq.QuarantineInput, want database.CommandState) {
		t.Helper()
		execution, err := commands.PersistCommand(ctx, commandFor(t, input))
		if err != nil || execution.Result == nil || execution.Result.State != want {
			t.Fatal("production terminal fixture failed", err)
		}
	}
	// Exact snapshots of every business column; resolution must not modify any.
	business := func(t *testing.T) map[string][][]string {
		t.Helper()
		result := make(map[string][][]string)
		for _, table := range []string{"conversation", "message", "message_command", "message_outbox", "message_delivery_cursor"} {
			rows, err := pool.QueryContext(ctx, "SELECT * FROM `"+table+"` ORDER BY 1,2")
			if err != nil {
				t.Fatal(err)
			}
			columns, err := rows.Columns()
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				values := make([]sql.RawBytes, len(columns))
				targets := make([]any, len(columns))
				for i := range targets {
					targets[i] = &values[i]
				}
				if err := rows.Scan(targets...); err != nil {
					t.Fatal(err)
				}
				record := make([]string, len(values))
				for i, value := range values {
					record[i] = "<NULL>"
					if value != nil {
						record[i] = "hex:" + hex.EncodeToString(value)
					}
				}
				result[table] = append(result[table], record)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	resolve := func(t *testing.T, id, previous string) (database.QuarantineResolveResult, error) {
		t.Helper()
		args := []string{"quarantine", "resolve", "--transport-id", id, "--confirm"}
		if previous != "" {
			args = append(args, "--expect-replay-attempt", previous)
		}
		var output bytes.Buffer
		err := run(args, &output)
		var result database.QuarantineResolveResult
		if output.Len() != 0 {
			if decodeErr := json.Unmarshal(output.Bytes(), &result); decodeErr != nil {
				t.Fatal(decodeErr)
			}
		}
		return result, err
	}
	checkAudit := func(t *testing.T, before database.QuarantineDetail, resolved bool) {
		t.Helper()
		after, err := repo.Get(ctx, before.Summary.TransportID)
		if err != nil {
			t.Fatal(err)
		}
		if resolved && (after.Summary.State != "resolved" || after.Summary.ResolvedAt == nil || after.Summary.ResolvedAt.Location().String() != "UTC") {
			t.Fatal("missing UTC resolution")
		}
		if resolved {
			after.Summary.State, after.Summary.ResolvedAt = before.Summary.State, before.Summary.ResolvedAt
		}
		a, _ := before.Input.EncodeAudit()
		b, _ := after.Input.EncodeAudit()
		if !bytes.Equal(a, b) || !reflect.DeepEqual(before.Summary, after.Summary) {
			t.Fatal("resolution changed immutable audit or retained replay attempt")
		}
	}
	t.Run("rejected_is_a_verified_terminal", func(t *testing.T) {
		input := inputFor(t, "rejected", 1)
		before := save(t, input)
		persist(t, input, database.CommandRejected)
		snapshot := business(t)
		result, err := resolve(t, before.Summary.TransportID, "")
		if err != nil || result.Outcome != "resolved" || result.TerminalState != database.CommandRejected || result.ReasonCode != database.ReasonNotFriends {
			t.Fatal("rejected terminal not resolved", err)
		}
		checkAudit(t, before, true)
		if !reflect.DeepEqual(snapshot, business(t)) {
			t.Fatal("resolution rewrote rejected terminal or outbox")
		}
	})
	if err := database.NewRelationManager(db).AddFriendByPhone(ctx, 12, "resolution-34"); err != nil {
		t.Fatal(err)
	}
	var persistedInput rabbitmq.QuarantineInput
	t.Run("persisted_and_repeated_resolution_preserve_timestamp", func(t *testing.T) {
		persistedInput = inputFor(t, "persisted", 2)
		before := save(t, persistedInput)
		persist(t, persistedInput, database.CommandPersisted)
		snapshot := business(t)
		first, err := resolve(t, before.Summary.TransportID, "")
		if err != nil || first.Outcome != "resolved" || first.TerminalState != database.CommandPersisted {
			t.Fatal(err)
		}
		second, err := resolve(t, before.Summary.TransportID, "")
		if err != nil || second.Outcome != "already_resolved" || first.ResolvedAt == nil || second.ResolvedAt == nil || !first.ResolvedAt.Equal(*second.ResolvedAt) {
			t.Fatal("repeat changed resolution timestamp", err)
		}
		checkAudit(t, before, true)
		if !reflect.DeepEqual(snapshot, business(t)) {
			t.Fatal("resolution changed persisted business columns")
		}
	})
	t.Run("pending_attempt_requires_matching_terminal_and_selection", func(t *testing.T) {
		input := inputFor(t, "pending", 3)
		before := save(t, input)
		id := before.Summary.TransportID
		attempt := "171c6c89-bfa6-4bd1-a738-cfa7d7a63164"
		if _, err := pool.ExecContext(ctx, "UPDATE message_quarantine SET state='replay_pending',replay_attempt_id=? WHERE transport_id=?", attempt, id); err != nil {
			t.Fatal(err)
		}
		before, err = repo.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := resolve(t, id, ""); !errors.Is(err, database.ErrQuarantineResolveChanged) {
			t.Fatal("missing expected attempt accepted", err)
		}
		if _, err := resolve(t, id, attempt); !errors.Is(err, database.ErrQuarantineTerminalPending) {
			t.Fatal("publisher/replay state substituted for terminal", err)
		}
		checkAudit(t, before, false)
		persist(t, input, database.CommandPersisted)
		snapshot := business(t)
		result, err := resolve(t, id, attempt)
		if err != nil || result.Outcome != "resolved" || result.ReplayAttemptID == nil || *result.ReplayAttemptID != attempt {
			t.Fatal(err)
		}
		checkAudit(t, before, true)
		if !reflect.DeepEqual(snapshot, business(t)) {
			t.Fatal("resolution changed business state")
		}
	})
	t.Run("hash_conflict_and_unknown_identity_stay_open", func(t *testing.T) {
		conflict := inputFor(t, "different-payload", 2)
		before := save(t, conflict)
		snapshot := business(t)
		if _, err := resolve(t, before.Summary.TransportID, ""); !errors.Is(err, database.ErrQuarantineTerminalConflict) {
			t.Fatal("different hash falsely resolved", err)
		}
		checkAudit(t, before, false)
		invalid := save(t, adminQuarantineInput(t, "unidentified", false, false))
		if _, err := resolve(t, invalid.Summary.TransportID, ""); !errors.Is(err, database.ErrQuarantineResolveInput) {
			t.Fatal("bad wire falsely resolved", err)
		}
		checkAudit(t, invalid, false)
		if !reflect.DeepEqual(snapshot, business(t)) {
			t.Fatal("conflict changed canonical business result")
		}
	})
	t.Run("persisted_message_corruption_is_not_a_terminal_proof", func(t *testing.T) {
		// New physical source, same canonical command as the successful case.
		input := inputFor(t, "persisted", 2)
		before := save(t, input)
		command := commandFor(t, persistedInput)
		if _, err := pool.ExecContext(ctx, "UPDATE message SET content='fixture corrupt' WHERE id=?", command.CommandID); err != nil {
			t.Fatal(err)
		}
		snapshot := business(t)
		if _, err := resolve(t, before.Summary.TransportID, ""); !errors.Is(err, database.ErrCommandInvariant) {
			t.Fatal("corrupt message treated as persisted", err)
		}
		checkAudit(t, before, false)
		if !reflect.DeepEqual(snapshot, business(t)) {
			t.Fatal("resolver repaired or rewrote corrupt business data")
		}
		if _, err := pool.ExecContext(ctx, "UPDATE message SET content=? WHERE id=?", command.Content, command.CommandID); err != nil {
			t.Fatal(err)
		}
		if _, err := resolve(t, before.Summary.TransportID, ""); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("concurrent_resolvers_converge_without_late_replay_overwrite", func(t *testing.T) {
		input := inputFor(t, "concurrent", 4)
		before := save(t, input)
		persist(t, input, database.CommandPersisted)
		snapshot := business(t)
		var wg sync.WaitGroup
		results := make(chan database.QuarantineResolveResult, 8)
		failures := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				result, err := repo.Resolve(ctx, database.QuarantineResolveSelection{TransportID: before.Summary.TransportID})
				results <- result
				failures <- err
			}()
		}
		wg.Wait()
		close(results)
		close(failures)
		for err := range failures {
			if err != nil {
				t.Fatal(err)
			}
		}
		changed, existing := 0, 0
		var stamp *time.Time
		for result := range results {
			if result.Outcome == "resolved" {
				changed++
			} else if result.Outcome == "already_resolved" {
				existing++
			} else {
				t.Fatal("unexpected concurrent result")
			}
			if result.ResolvedAt == nil {
				t.Fatal("missing resolution time")
			}
			if stamp != nil && !stamp.Equal(*result.ResolvedAt) {
				t.Fatal("concurrent resolution rewrote timestamp")
			}
			stamp = result.ResolvedAt
		}
		if changed != 1 || existing != 7 {
			t.Fatal("concurrent resolvers did not converge", changed, existing)
		}
		identity, _ := input.Identity()
		receipt := rabbitmq.PublishResult{Outcome: rabbitmq.PublishAccepted, CommandID: identity.CommandID, PublishAttemptID: "171c6c89-bfa6-4bd1-a738-cfa7d7a63164", ConnectionEpoch: 1, ChannelEpoch: 1, DeliveryTag: 1}
		if err := repo.RecordReplay(ctx, before, receipt); !errors.Is(err, database.ErrQuarantineReplayChanged) {
			t.Fatal("late replay reopened resolved row", err)
		}
		checkAudit(t, before, true)
		if !reflect.DeepEqual(snapshot, business(t)) {
			t.Fatal("concurrent resolution duplicated business writes")
		}
	})
	for table, want := range map[string]int{"message_quarantine": 7, "message_command": 4, "message": 3, "message_outbox": 7, "conversation": 1, "message_delivery_cursor": 0} {
		var got int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"`").Scan(&got); err != nil || got != want {
			t.Fatal("unexpected final set", table, got, want, err)
		}
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal("schema fingerprint changed", err)
	}
}
