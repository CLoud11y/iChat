//go:build integration

package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"iChat/messaging"
	"iChat/migrations"
	"iChat/tests/testenv"
)

func TestOutboxSettlementIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	env, err := testenv.Start(ctx, filepath.Join("..", "docker-compose.test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("outbox settlement fixture run=%s", env.RunID())
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
	o := mysql.NewConfig()
	o.User, o.Passwd, o.DBName = c.MYSQL.User, c.MYSQL.Password, c.MYSQL.DbName
	o.Net, o.Addr = "tcp", net.JoinHostPort(c.MYSQL.Host, c.MYSQL.Port)
	o.ParseTime = true
	o.Timeout, o.ReadTimeout, o.WriteTimeout = 3*time.Second, 5*time.Second, 5*time.Second
	o.Loc = time.FixedZone("settlement-driver-offset", 13*60*60)
	o.Params = map[string]string{"charset": "utf8mb4", "time_zone": "'+05:30'"}
	connector, err := mysql.NewConnector(o)
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
	repo, _ := NewOutboxRepository(pool)
	serial := 0
	policy := OutboxRetryOptions{Base: 10 * time.Minute, Max: 40 * time.Minute}
	dbNow := func(t *testing.T) time.Time {
		t.Helper()
		var s string
		if err := pool.QueryRowContext(ctx, "SELECT DATE_FORMAT(UTC_TIMESTAMP(6),'%Y-%m-%d %H:%i:%s.%f')").Scan(&s); err != nil {
			t.Fatal(err)
		}
		stamp, err := time.Parse(mysqlMicrosecondLayout, s)
		if err != nil {
			t.Fatal(err)
		}
		return stamp
	}
	seed := func(t *testing.T, duration time.Duration) OutboxLease {
		t.Helper()
		serial++
		command := fmt.Sprintf("%064x", serial)
		id, _ := messaging.OutboxEventID(command, messaging.EventCommandResult)
		if _, err := pool.ExecContext(ctx, "INSERT INTO message_outbox(event_id,event_type,command_id,payload,status,available_at,attempts,last_error,created_at) VALUES (?,'command.result',?,?,'pending',UTC_TIMESTAMP(6),0,'NOTIFICATION_FAILED',UTC_TIMESTAMP(6))", id, command, `{"fixture":"中文\u0000","large":"9007199254740993"}`); err != nil {
			t.Fatal(err)
		}
		leases, err := repo.Claim(ctx, OutboxClaimOptions{Limit: 10, Lease: duration})
		if err != nil || len(leases) != 1 || leases[0].EventID != id {
			t.Fatal("could not claim fixture event", err)
		}
		return leases[0]
	}
	immutable := func(t *testing.T, id string) []string {
		t.Helper()
		var row [8]string
		if err := pool.QueryRowContext(ctx, "SELECT event_id,event_type,command_id,COALESCE(conversation_id,''),COALESCE(message_id,''),CAST(payload AS CHAR),DATE_FORMAT(created_at,'%Y-%m-%d %H:%i:%s.%f'),attempts FROM message_outbox WHERE event_id=?", id).Scan(&row[0], &row[1], &row[2], &row[3], &row[4], &row[5], &row[6], &row[7]); err != nil {
			t.Fatal(err)
		}
		return row[:]
	}
	check := func(t *testing.T, lease OutboxLease, state string) OutboxSettlement {
		t.Helper()
		var status string
		var token, until, completed, last sql.NullString
		var available string
		if err := pool.QueryRowContext(ctx, "SELECT status,lease_token,DATE_FORMAT(lease_until,'%Y-%m-%d %H:%i:%s.%f'),DATE_FORMAT(completed_at,'%Y-%m-%d %H:%i:%s.%f'),last_error,DATE_FORMAT(available_at,'%Y-%m-%d %H:%i:%s.%f') FROM message_outbox WHERE event_id=?", lease.EventID).Scan(&status, &token, &until, &completed, &last, &available); err != nil {
			t.Fatal(err)
		}
		if status != state || token.Valid || until.Valid {
			t.Fatal("lease metadata was not cleared")
		}
		result := OutboxSettlement{EventID: lease.EventID, Status: status}
		if state == "completed" {
			if !completed.Valid || last.Valid {
				t.Fatal("completion fields invalid")
			}
			stamp, err := time.Parse(mysqlMicrosecondLayout, completed.String)
			if err != nil {
				t.Fatal(err)
			}
			result.CompletedAt = &stamp
		} else {
			if completed.Valid || !last.Valid || last.String != string(OutboxRetryPublishFailed) {
				t.Fatal("retry diagnostics/state invalid")
			}
			stamp, err := time.Parse(mysqlMicrosecondLayout, available)
			if err != nil {
				t.Fatal(err)
			}
			result.AvailableAt = &stamp
		}
		return result
	}
	t.Run("complete_clears_lease_and_rejects_duplicate_or_failed_callback", func(t *testing.T) {
		lease := seed(t, time.Minute)
		before := immutable(t, lease.EventID)
		result, err := repo.Complete(ctx, lease)
		if err != nil || result.Status != "completed" || result.CompletedAt == nil || result.CompletedAt.Location().String() != "UTC" {
			t.Fatal(err)
		}
		stored := check(t, lease, "completed")
		if !result.CompletedAt.Equal(*stored.CompletedAt) {
			t.Fatal("completion receipt did not match SQL")
		}
		for _, retry := range []bool{false, true} {
			var err error
			if retry {
				_, err = repo.Retry(ctx, lease, policy, OutboxRetryPublishFailed)
			} else {
				_, err = repo.Complete(ctx, lease)
			}
			if !errors.Is(err, ErrOutboxLeaseLost) {
				t.Fatal("late callback changed completion", err)
			}
		}
		after := check(t, lease, "completed")
		if !after.CompletedAt.Equal(*stored.CompletedAt) || !reflect.DeepEqual(before, immutable(t, lease.EventID)) {
			t.Fatal("terminal/immutable fields changed")
		}
	})
	t.Run("retry_sets_database_based_jitter_and_remains_unavailable", func(t *testing.T) {
		lease := seed(t, time.Minute)
		before := immutable(t, lease.EventID)
		start := dbNow(t)
		result, err := repo.Retry(ctx, lease, policy, OutboxRetryPublishFailed)
		end := dbNow(t)
		if err != nil || result.Status != "pending" || result.AvailableAt == nil || result.AvailableAt.Before(start.Add(5*time.Minute)) || result.AvailableAt.After(end.Add(10*time.Minute)) {
			t.Fatal("retry delay not bounded by database time", err)
		}
		stored := check(t, lease, "pending")
		if !result.AvailableAt.Equal(*stored.AvailableAt) || !reflect.DeepEqual(before, immutable(t, lease.EventID)) {
			t.Fatal("retry mutated event or receipt")
		}
		if leases, err := repo.Claim(ctx, OutboxClaimOptions{Limit: 10, Lease: time.Minute}); err != nil || len(leases) != 0 {
			t.Fatal("backoff ignored", err)
		}
		if _, err := repo.Complete(ctx, lease); !errors.Is(err, ErrOutboxLeaseLost) {
			t.Fatal("old retry token still settled", err)
		}
	})
	eventually := func(t *testing.T, f func() bool) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for !f() {
			if time.Now().After(deadline) {
				t.Fatal("fixture barrier not reached")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	lock := func(t *testing.T, id string) *sql.Tx {
		t.Helper()
		tx, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback() })
		var got string
		if err := tx.QueryRowContext(ctx, "SELECT event_id FROM message_outbox WHERE event_id=? FOR UPDATE", id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return tx
	}
	t.Run("lock_wait_samples_fresh_clock_and_reclaimed_owner_is_fenced", func(t *testing.T) {
		lease := seed(t, time.Second)
		tx := lock(t, lease.EventID)
		type outcome struct {
			result OutboxSettlement
			err    error
		}
		done := make(chan outcome, 1)
		go func() { r, e := repo.Complete(ctx, lease); done <- outcome{r, e} }()
		eventually(t, func() bool {
			var n int
			err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.processlist WHERE DB=? AND INFO LIKE 'SELECT event_id,status,%FROM message_outbox WHERE%FOR UPDATE'", o.DBName).Scan(&n)
			return err == nil && n == 1
		})
		eventually(t, func() bool { return !dbNow(t).Before(lease.Until) })
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-done:
			if !errors.Is(got.err, ErrOutboxLeaseLost) || got.result != (OutboxSettlement{}) {
				t.Fatal("expired owner settled after waiting", got.err)
			}
		case <-time.After(4 * time.Second):
			t.Fatal("settlement did not join")
		}
		leases, err := repo.Claim(ctx, OutboxClaimOptions{Limit: 10, Lease: time.Minute})
		if err != nil || len(leases) != 1 {
			t.Fatal("expired lease not recoverable", err)
		}
		next := leases[0]
		if next.EventID != lease.EventID || next.Token == lease.Token || next.Attempts != 2 {
			t.Fatal("reclaim did not change owner")
		}
		if _, err := repo.Complete(ctx, lease); !errors.Is(err, ErrOutboxLeaseLost) {
			t.Fatal("old owner overwrote new claim", err)
		}
		if _, err := repo.Retry(ctx, lease, policy, OutboxRetryPublishFailed); !errors.Is(err, ErrOutboxLeaseLost) {
			t.Fatal("old failure overwrote new claim", err)
		}
		if _, err := repo.Complete(ctx, next); err != nil {
			t.Fatal(err)
		}
		check(t, next, "completed")
	})
	t.Run("concurrent_success_and_failure_have_one_winner", func(t *testing.T) {
		lease := seed(t, time.Minute)
		before := immutable(t, lease.EventID)
		start := make(chan struct{})
		errs := make(chan error, 2)
		results := make(chan OutboxSettlement, 2)
		var wg sync.WaitGroup
		for _, retry := range []bool{false, true} {
			wg.Add(1)
			go func(retry bool) {
				defer wg.Done()
				<-start
				var r OutboxSettlement
				var e error
				if retry {
					r, e = repo.Retry(ctx, lease, policy, OutboxRetryPublishFailed)
				} else {
					r, e = repo.Complete(ctx, lease)
				}
				results <- r
				errs <- e
			}(retry)
		}
		close(start)
		wg.Wait()
		close(errs)
		close(results)
		success, lost := 0, 0
		for e := range errs {
			if e == nil {
				success++
			} else if errors.Is(e, ErrOutboxLeaseLost) {
				lost++
			} else {
				t.Fatal(e)
			}
		}
		if success != 1 || lost != 1 {
			t.Fatal("same lease settled twice")
		}
		for r := range results {
			if r.EventID != "" {
				check(t, lease, r.Status)
			}
		}
		if !reflect.DeepEqual(before, immutable(t, lease.EventID)) {
			t.Fatal("race changed event/attempt")
		}
	})
	t.Run("canceled_lock_wait_preserves_owned_record", func(t *testing.T) {
		lease := seed(t, time.Minute)
		before := immutable(t, lease.EventID)
		tx := lock(t, lease.EventID)
		call, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		result, err := repo.Complete(call, lease)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || result != (OutboxSettlement{}) {
			t.Fatal("canceled settlement reported success", err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, immutable(t, lease.EventID)) {
			t.Fatal("cancellation changed event")
		}
		if _, err := repo.Complete(ctx, lease); err != nil {
			t.Fatal("pool/lease not usable after cancellation", err)
		}
		check(t, lease, "completed")
	})
	for _, table := range []string{"message", "message_command", "conversation", "message_delivery_cursor", "message_quarantine"} {
		var n int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"`").Scan(&n); err != nil || n != 0 {
			t.Fatal("settlement wrote another table", table, err)
		}
	}
	var n int
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_outbox").Scan(&n); err != nil || n != 5 {
		t.Fatal("outbox set changed", n, err)
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal("schema fingerprint changed", err)
	}
}
