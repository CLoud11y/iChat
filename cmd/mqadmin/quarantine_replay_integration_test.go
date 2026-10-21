//go:build integration

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"iChat/application"
	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/migrations"
	"iChat/tests/testenv"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestQuarantineReplayCLIIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, err := testenv.StartWithRabbitMQ(ctx, filepath.Join("..", "..", "docker-compose.test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("quarantine replay fixture run=%s", env.RunID())
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
	broker, err := env.RabbitMQ()
	if err != nil {
		t.Fatal(err)
	}
	// The isolated fixture uses one generated infrastructure password for its
	// SQL user and management admin. Application AMQP has a separate password.
	m, err := rabbitmq.NewManagement("http://"+broker.ManagementAddress, "ichat_test_admin", conf.MYSQL.Password)
	if err != nil {
		t.Fatal(err)
	}
	control, err := amqp.DialConfig(broker.AMQPURL(), amqp.Config{Recovery: nil, Heartbeat: 2 * time.Second})
	if err != nil {
		t.Fatal("open fixture control failed")
	}
	t.Cleanup(func() { _ = control.CloseDeadline(time.Now().Add(3 * time.Second)) })
	ch, err := control.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := rabbitmq.EnsureLocal(ctx, ch, m, broker.VHost); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "replay.yml")
	contents := fmt.Sprintf("MYSQL:\n  user: %q\n  password: %q\n  host: %q\n  port: %q\n  dbname: %q\nRABBITMQ:\n  url: %q\n  vhost: %q\n  managementURL: %q\n  managementUsername: ichat_test_admin\n  managementPassword: %q\nRUNTIME:\n  role: invalid\nREDIS: invalid\n", conf.MYSQL.User, conf.MYSQL.Password, conf.MYSQL.Host, conf.MYSQL.Port, conf.MYSQL.DbName, broker.AMQPURL(), broker.VHost, "http://"+broker.ManagementAddress, conf.MYSQL.Password)
	if err := os.WriteFile(file, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"URL", "VHOST", "MANAGEMENTURL", "MANAGEMENTUSERNAME", "MANAGEMENTPASSWORD", "PUBLISHERSLOTS", "PUBLISHTIMEOUT", "CONNECTTIMEOUT", "WRITETIMEOUT", "CLOSETIMEOUT", "HEARTBEAT", "RETRYMIN", "RETRYMAX", "CONSUMERWORKERS", "CONSUMERPREFETCH", "CONSUMEROPERATIONTIMEOUT"} {
		name := "ICHAT_RABBITMQ_" + key
		old, present := os.LookupEnv(name)
		_ = os.Unsetenv(name)
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(name, old)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
	for key, value := range map[string]string{"USER": conf.MYSQL.User, "PASSWORD": conf.MYSQL.Password, "HOST": conf.MYSQL.Host, "PORT": conf.MYSQL.Port, "DBNAME": conf.MYSQL.DbName} {
		t.Setenv("ICHAT_MYSQL_"+key, value)
	}
	t.Setenv("ICHAT_CONFIG_FILE", file)
	options, err := loadQuarantineMySQL(file)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := sql.Open("mysql", options.FormatDSN())
	if err != nil {
		t.Fatal("open fixture SQL failed")
	}
	t.Cleanup(func() { _ = pool.Close() })
	var schema string
	if err := pool.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); err != nil || schema != "ichat_test_"+env.RunID() {
		t.Fatal("refusing fixture writes outside owned schema")
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal(err)
	}
	repo, _ := database.NewQuarantineRepository(pool)
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
	queueEmpty := func(t *testing.T) {
		t.Helper()
		for _, queue := range []string{rabbitmq.PersistQueue, rabbitmq.QuarantineQueue} {
			if _, ok, err := ch.Get(queue, false); err != nil || ok {
				t.Fatal("unexpected queued message", queue, err)
			}
		}
	}
	checkAudit := func(t *testing.T, before database.QuarantineDetail, attempt, state string) {
		t.Helper()
		after, err := repo.Get(ctx, before.Summary.TransportID)
		if err != nil || after.Summary.State != state || after.Summary.ResolvedAt != nil {
			t.Fatal("unexpected replay state", err)
		}
		if attempt == "" {
			if after.Summary.ReplayAttemptID != nil {
				t.Fatal("unconfirmed replay was recorded")
			}
		} else if after.Summary.ReplayAttemptID == nil || *after.Summary.ReplayAttemptID != attempt {
			t.Fatal("confirmed attempt lost")
		}
		a, _ := before.Input.EncodeAudit()
		b, _ := after.Input.EncodeAudit()
		if !bytes.Equal(a, b) {
			t.Fatal("replay changed immutable audit document")
		}
		// Remove the two allowed replay fields; every other summary field and
		// decoded/revalidated raw SQL field must remain exactly the same.
		after.Summary.State, after.Summary.ReplayAttemptID = before.Summary.State, before.Summary.ReplayAttemptID
		if !reflect.DeepEqual(before.Summary, after.Summary) {
			t.Fatal("replay changed non-replay metadata")
		}
	}
	t.Run("confirmed_wrapper_and_dlx_replays", func(t *testing.T) {
		for _, dlx := range []bool{false, true} {
			input := adminQuarantineInput(t, fmt.Sprintf("confirmed-%t", dlx), true, dlx)
			before := save(t, input)
			id := before.Summary.TransportID
			var previous string
			for i := 0; i < 2; i++ {
				args := []string{"quarantine", "replay", "--transport-id", id, "--confirm"}
				if previous != "" {
					args = append(args, "--expect-replay-attempt", previous)
				}
				var output bytes.Buffer
				if err := run(args, &output); err != nil {
					t.Fatal(err)
				}
				var result application.QuarantineReplayResult
				if json.Unmarshal(output.Bytes(), &result) != nil || result.Publish != rabbitmq.PublishAccepted || result.AttemptRecord != "recorded" || result.AttemptID == previous {
					t.Fatal("confirmed replay receipt lost")
				}
				deadline := time.Now().Add(5 * time.Second)
				var delivery amqp.Delivery
				for {
					got, ok, err := ch.Get(rabbitmq.PersistQueue, false)
					if err != nil {
						t.Fatal(err)
					}
					if ok {
						delivery = got
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("confirmed publication missing")
					}
					time.Sleep(20 * time.Millisecond)
				}
				source, _ := input.Envelope().Source()
				original, _ := messaging.DecodeEnvelope(source.Body)
				wire, err := messaging.DecodeEnvelope(delivery.Body)
				if err != nil || wire.PublishAttemptID != result.AttemptID || wire.PublishAttemptID == original.PublishAttemptID || delivery.CorrelationId != result.AttemptID || delivery.MessageId != original.CommandID || delivery.ContentType != "application/json" || delivery.DeliveryMode != amqp.Persistent || delivery.Expiration != "" || len(delivery.Headers) != 0 {
					t.Fatal("physical replay identity/properties changed", err)
				}
				wire.PublishAttemptID = original.PublishAttemptID
				if wire != original {
					t.Fatal("replay changed original business fields/hash/timestamps")
				}
				if err := delivery.Ack(false); err != nil {
					t.Fatal(err)
				}
				checkAudit(t, before, result.AttemptID, "replay_pending")
				if i == 1 {
					receipt := rabbitmq.PublishResult{Outcome: rabbitmq.PublishAccepted, CommandID: original.CommandID, PublishAttemptID: result.AttemptID, ConnectionEpoch: 1, ChannelEpoch: 1, DeliveryTag: 1}
					if err := repo.RecordReplay(ctx, before, receipt); !errors.Is(err, database.ErrQuarantineReplayChanged) {
						t.Fatal("stale snapshot overwrote a later replay", err)
					}
				}
				previous = result.AttemptID
				output.Reset()
				if err := run([]string{"quarantine", "replay", "--transport-id", id, "--confirm"}, &output); !errors.Is(err, database.ErrQuarantineReplayChanged) || output.Len() != 0 {
					t.Fatal("repeat without latest attempt was published", err)
				}
				queueEmpty(t)
			}
		}
	})
	t.Run("invalid_and_resolved_records_never_publish", func(t *testing.T) {
		for _, valid := range []bool{false, true} {
			input := adminQuarantineInput(t, fmt.Sprintf("blocked-%t", valid), valid, false)
			before := save(t, input)
			if valid {
				if _, err := pool.ExecContext(ctx, "UPDATE message_quarantine SET state='resolved',resolved_at='2026-10-22 02:03:04.000000' WHERE transport_id=?", before.Summary.TransportID); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			if err := run([]string{"quarantine", "replay", "--transport-id", before.Summary.TransportID, "--confirm"}, &output); err == nil || output.Len() != 0 {
				t.Fatal("invalid/resolved record replayed", err)
			}
			queueEmpty(t)
		}
	})
	t.Run("mandatory_return_does_not_record_attempt", func(t *testing.T) {
		before := save(t, adminQuarantineInput(t, "returned-replay", true, false))
		var output bytes.Buffer
		err := runQuarantineReplay([]string{"--transport-id", before.Summary.TransportID, "--confirm"}, &output, openQuarantineReplayStore, func(ctx context.Context, file string) (quarantineReplayPublisher, error) {
			p, err := dialQuarantineReplayPublisher(ctx, file)
			if err != nil {
				return nil, err
			}
			// Drift only after the real read-only topology check and socket
			// setup. Production mandatory return handling decides the outcome.
			if err := ch.QueueUnbind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, nil); err != nil {
				_ = p.Close(ctx)
				return nil, err
			}
			return p, nil
		})
		if bindErr := ch.QueueBind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, false, nil); bindErr != nil {
			t.Fatal(bindErr)
		}
		var result application.QuarantineReplayResult
		if err == nil || json.Unmarshal(output.Bytes(), &result) != nil || result.Publish != rabbitmq.PublishUnroutable || result.AttemptRecord != "not_attempted" || !messaging.ValidPublishAttemptID(result.AttemptID) {
			t.Fatal("actual return was treated as acceptance", err)
		}
		checkAudit(t, before, "", "open")
		queueEmpty(t)
	})
	for _, table := range []string{"conversation", "message", "message_command", "message_outbox", "message_delivery_cursor"} {
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"`").Scan(&count); err != nil || count != 0 {
			t.Fatal("replay CLI wrote business state", table, err)
		}
	}
	var count int
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_quarantine").Scan(&count); err != nil || count != 5 {
		t.Fatal("unexpected audit set", err)
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal("schema fingerprint changed", err)
	}
	if err := rabbitmq.VerifyLocal(ctx, m, broker.VHost); err != nil {
		t.Fatal("fixture topology not restored", err)
	}
}
