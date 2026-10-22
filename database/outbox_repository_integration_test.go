//go:build integration

package database

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"iChat/messaging"
	"iChat/migrations"
	"iChat/tests/testenv"
)

func TestOutboxClaimIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	env, err := testenv.Start(ctx, filepath.Join("..", "docker-compose.test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("outbox claim fixture run=%s", env.RunID())
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := env.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	c, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	options := mysql.NewConfig()
	options.User, options.Passwd, options.DBName = c.MYSQL.User, c.MYSQL.Password, c.MYSQL.DbName
	options.Net, options.Addr = "tcp", net.JoinHostPort(c.MYSQL.Host, c.MYSQL.Port)
	options.ParseTime = true
	options.Timeout, options.ReadTimeout, options.WriteTimeout = 3*time.Second, 5*time.Second, 5*time.Second
	options.Loc = time.FixedZone("outbox-driver-offset", 13*60*60)
	options.Params = map[string]string{"charset": "utf8mb4", "time_zone": "'+05:30'"}
	connector, err := mysql.NewConnector(options)
	if err != nil {
		t.Fatal("configure fixture SQL failed")
	}
	pool := sql.OpenDB(connector)
	pool.SetMaxOpenConns(16)
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
	repo, _ := NewOutboxRepository(pool)
	serial := 0
	payloads := map[string][]byte{}
	seed := func(t *testing.T, status string, available, until time.Time, attempts uint32) string {
		t.Helper()
		serial++
		command := fmt.Sprintf("%064x", serial)
		id, _ := messaging.OutboxEventID(command, messaging.EventCommandResult)
		body := `{"schemaVersion":1,"fixture":"中文\u0000","large":"9007199254740993"}`
		var token, expiry, completed any
		if status == "leased" {
			token = "171c6c89-bfa6-4bd1-a738-cfa7d7a63164"
			expiry = until.Format(mysqlMicrosecondLayout)
		}
		if status == "completed" {
			completed = available.Format(mysqlMicrosecondLayout)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO message_outbox(event_id,event_type,command_id,payload,status,available_at,lease_token,lease_until,attempts,created_at,completed_at) VALUES (?,'command.result',?,?,?,?,?,?,?,UTC_TIMESTAMP(6),?)", id, command, body, status, available.Format(mysqlMicrosecondLayout), token, expiry, attempts, completed); err != nil {
			t.Fatal(err)
		}
		var stored []byte
		if err := pool.QueryRowContext(ctx, "SELECT payload FROM message_outbox WHERE event_id=?", id).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		payloads[id] = stored
		return id
	}
	var clock string
	if err := pool.QueryRowContext(ctx, "SELECT DATE_FORMAT(UTC_TIMESTAMP(6),'%Y-%m-%d %H:%i:%s.%f')").Scan(&clock); err != nil {
		t.Fatal(err)
	}
	now, _ := time.Parse(mysqlMicrosecondLayout, clock)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	due := seed(t, "pending", past, time.Time{}, 0)
	expired := seed(t, "leased", past, past, 7)
	futurePending := seed(t, "pending", future, time.Time{}, 0)
	futureLease := seed(t, "leased", past, future, 2)
	completed := seed(t, "completed", past, time.Time{}, 3)
	t.Run("eligibility_expiry_and_exact_stored_payload", func(t *testing.T) {
		leases, err := repo.Claim(ctx, OutboxClaimOptions{Limit: 10, Lease: 30 * time.Second})
		if err != nil || len(leases) != 2 {
			t.Fatal("unexpected eligible set", err, len(leases))
		}
		seen := map[string]bool{}
		for _, l := range leases {
			seen[l.EventID] = true
			if !bytes.Equal(l.Payload, payloads[l.EventID]) || !json.Valid(l.Payload) || !messaging.ValidPublishAttemptID(l.Token) || l.Until.Location().String() != "UTC" {
				t.Fatal("payload/token/UTC changed")
			}
			want := uint32(1)
			if l.EventID == expired {
				want = 8
			}
			if l.Attempts != want {
				t.Fatal("attempt count incorrect")
			}
		}
		if !seen[due] || !seen[expired] || leases[0].Token == leases[1].Token {
			t.Fatal("expired claim missing or token shared")
		}
		var old string
		if err := pool.QueryRowContext(ctx, "SELECT lease_token FROM message_outbox WHERE event_id=?", expired).Scan(&old); err != nil || old == "171c6c89-bfa6-4bd1-a738-cfa7d7a63164" {
			t.Fatal("expired token retained", err)
		}
		for id, want := range map[string]string{futurePending: "pending", futureLease: "leased", completed: "completed"} {
			var state string
			if err := pool.QueryRowContext(ctx, "SELECT status FROM message_outbox WHERE event_id=?", id).Scan(&state); err != nil || state != want {
				t.Fatal("ineligible row changed", err)
			}
		}
		if leases, err := repo.Claim(ctx, OutboxClaimOptions{Limit: 10, Lease: time.Minute}); err != nil || len(leases) != 0 {
			t.Fatal("active lease stolen", err)
		}
	})
	t.Run("skip_locked_and_uncommitted_rows", func(t *testing.T) {
		locked := seed(t, "pending", past, time.Time{}, 0)
		free := seed(t, "pending", past, time.Time{}, 0)
		tx, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		var id string
		if err := tx.QueryRowContext(ctx, "SELECT event_id FROM message_outbox WHERE event_id=? FOR UPDATE", locked).Scan(&id); err != nil {
			t.Fatal(err)
		}
		call, cancel := context.WithTimeout(ctx, 2*time.Second)
		leases, err := repo.Claim(call, OutboxClaimOptions{Limit: 10, Lease: time.Minute})
		cancel()
		if err != nil || len(leases) != 1 || leases[0].EventID != free {
			t.Fatal("locked task blocked or leaked into claim", err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		leases, err = repo.Claim(ctx, OutboxClaimOptions{Limit: 10, Lease: time.Minute})
		if err != nil || len(leases) != 1 || leases[0].EventID != locked {
			t.Fatal("released lock did not become eligible", err)
		}
		// A due row moved back to pending only inside an uncommitted transaction
		// must not be offered to another owner.
		tx, err = pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "UPDATE message_outbox SET status='pending',lease_token=NULL,lease_until=NULL WHERE event_id=?", free); err != nil {
			t.Fatal(err)
		}
		call, cancel = context.WithTimeout(ctx, 2*time.Second)
		leases, err = repo.Claim(call, OutboxClaimOptions{Limit: 10, Lease: time.Minute})
		cancel()
		if err != nil || len(leases) != 0 {
			t.Fatal("uncommitted pending state claimed", err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("concurrent_claimers_have_disjoint_committed_owners", func(t *testing.T) {
		want := map[string]bool{}
		for i := 0; i < 10; i++ {
			want[seed(t, "pending", past, time.Time{}, 0)] = true
		}
		var wg sync.WaitGroup
		results := make(chan []OutboxLease, 10)
		failures := make(chan error, 10)
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				leases, err := repo.Claim(ctx, OutboxClaimOptions{Limit: 1, Lease: time.Minute})
				results <- leases
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
		tokens := map[string]bool{}
		accept := func(l OutboxLease) {
			if !want[l.EventID] || tokens[l.Token] {
				t.Fatal("duplicate owner")
			}
			delete(want, l.EventID)
			tokens[l.Token] = true
			var stored string
			if err := pool.QueryRowContext(ctx, "SELECT lease_token FROM message_outbox WHERE event_id=? AND status='leased'", l.EventID).Scan(&stored); err != nil || stored != l.Token {
				t.Fatal("returned lease was not committed", err)
			}
		}
		for batch := range results {
			if len(batch) > 1 {
				t.Fatal("claim exceeded its limit")
			}
			for _, l := range batch {
				accept(l)
			}
		}
		// SKIP LOCKED may yield an empty batch while another short transaction
		// scans/locks candidates. After those join, drain the remaining due set.
		for i := 0; len(want) > 0 && i < 10; i++ {
			leases, err := repo.Claim(ctx, OutboxClaimOptions{Limit: 1, Lease: time.Minute})
			if err != nil || len(leases) != 1 {
				t.Fatal("unlocked due set was not recoverable", err)
			}
			accept(leases[0])
		}
		if len(want) != 0 {
			t.Fatal("due set incomplete")
		}
	})
	for _, table := range []string{"message", "message_command", "conversation", "message_delivery_cursor", "message_quarantine"} {
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"`").Scan(&count); err != nil || count != 0 {
			t.Fatal("claim wrote another table", table, err)
		}
	}
	var count int
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_outbox").Scan(&count); err != nil || count != 17 {
		t.Fatal("outbox set changed", count, err)
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal("schema fingerprint changed", err)
	}
}
