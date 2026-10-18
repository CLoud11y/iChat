//go:build integration

package testenv

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"iChat/database"
	"iChat/messaging/rabbitmq"
	"iChat/workers"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

// Test-only hooks execute real locking SQL after the real message INSERT. The
// deadlock and lock-timeout errors below come from MySQL, not error injection.
type persistenceRetrySQL struct {
	mode          string
	everyAttempt  bool
	rollbackError error
	commitError   error
	entered       chan struct{}
	proceed       chan struct{}
	faults        chan error
	begins        atomic.Int64
	rollbacks     atomic.Int64
	commits       atomic.Int64
	hooks         atomic.Int64
}

func newPersistenceRetrySQL(mode string) *persistenceRetrySQL {
	return &persistenceRetrySQL{mode: mode, entered: make(chan struct{}, 5), proceed: make(chan struct{}), faults: make(chan error, 5)}
}

func (f *persistenceLifecycleFixture) retryPool(t *testing.T, scenario *persistenceRetrySQL) *sql.DB {
	t.Helper()
	conf := f.dsn.Clone()
	conf.InterpolateParams = true // Preserve ExecContext hooks for parameterized SQL.
	base, err := mysql.NewConnector(conf)
	if err != nil {
		t.Fatal(err)
	}
	pool := sql.OpenDB(retrySQLConnector{base: base, scenario: scenario})
	t.Cleanup(func() { pool.Close() })
	return pool
}

type retryInvocation struct {
	stream    *rabbitmq.ConsumerChannel
	done      chan struct{}
	execution database.CommandExecution
	err       error
}

func (f *persistenceLifecycleFixture) invokeRetry(t *testing.T, pool *sql.DB, options workers.PersistenceRetryOptions) *retryInvocation {
	t.Helper()
	stream, err := f.session(t, f.broker).OpenConsumer(f.ctx, rabbitmq.PersistQueue, 1)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := stream.Next(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := database.NewMessageRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := workers.NewPersistenceHandlerWithRetry(repository, options)
	if err != nil {
		t.Fatal(err)
	}
	operation, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	call := &retryInvocation{stream: stream, done: make(chan struct{})}
	go func() {
		call.execution, call.err = handler.Handle(operation, delivery)
		close(call.done)
	}()
	t.Cleanup(func() {
		cancel()
		lifecycleAwait(t, call.done) // Retain pool/channel until SQL actually returns.
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := stream.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	return call
}

func (f *persistenceLifecycleFixture) retryBlocker(t *testing.T) *sql.Tx {
	t.Helper()
	tx, err := f.pool.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	// Make the blocker heavier than the small message transaction. InnoDB
	// should choose the message transaction as the real deadlock victim.
	if _, err := tx.ExecContext(f.ctx, "UPDATE persistence_retry_lock SET v=v+1 WHERE id>=2"); err != nil {
		t.Fatal(err)
	}
	return tx
}

func requireRetrySQL(t *testing.T, scenario *persistenceRetrySQL, number uint16) {
	t.Helper()
	select {
	case err := <-scenario.faults:
		var server *mysql.MySQLError
		if !errors.As(err, &server) || server.Number != number {
			t.Fatalf("expected real MySQL %d, got %v", number, err)
		}
	default:
		t.Fatal("expected SQL fault was not observed")
	}
}

func TestPersistenceRetryIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	f := newPersistenceLifecycleFixture(t, ctx)
	if _, err := f.pool.ExecContext(ctx, "CREATE TABLE persistence_retry_lock (id INT PRIMARY KEY,v INT NOT NULL) ENGINE=InnoDB"); err != nil {
		t.Fatal(err)
	}
	for id := 1; id <= 128; id++ {
		if _, err := f.pool.ExecContext(ctx, "INSERT INTO persistence_retry_lock VALUES (?,0)", id); err != nil {
			t.Fatal(err)
		}
	}
	run := func(name string, test func(*testing.T)) {
		t.Helper()
		if !t.Run(name, test) {
			t.FailNow()
		}
	}
	run("real_deadlock_rolls_back_then_retries_same_delivery", func(t *testing.T) {
		command, _ := f.publish(t)
		scenario := newPersistenceRetrySQL("deadlock")
		var release sync.Once
		defer release.Do(func() { close(scenario.proceed) })
		pool := f.retryPool(t, scenario)
		blocker := f.retryBlocker(t)
		defer blocker.Rollback()
		call := f.invokeRetry(t, pool, workers.DefaultPersistenceRetryOptions())
		select {
		case <-scenario.entered:
		case <-call.done:
			t.Fatal("message transaction did not reach locking hook", call.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if f.count(t, command) != 0 {
			t.Fatal("uncommitted first attempt became visible")
		}
		blocked := make(chan error, 1)
		go func() {
			_, err := blocker.ExecContext(ctx, "UPDATE persistence_retry_lock SET v=v+1 WHERE id=1")
			if err == nil {
				err = blocker.Commit()
			}
			blocked <- err
		}()
		release.Do(func() { close(scenario.proceed) })
		lifecycleAwait(t, call.done)
		if err := <-blocked; err != nil {
			t.Fatal("blocker unexpectedly became the deadlock victim", err)
		}
		requireRetrySQL(t, scenario, 1213)
		if call.err != nil || call.execution.Disposition != database.CommandApplied || scenario.begins.Load() != 2 || scenario.rollbacks.Load() != 1 || scenario.commits.Load() != 1 {
			t.Fatal("real deadlock did not recover in one local retry", call.err)
		}
		if err := call.stream.Close(ctx); err != nil {
			t.Fatal(err)
		}
		f.checkDurableSet(t, command)
		f.checkEmpty(t)
	})
	run("real_lock_wait_timeout_retries_full_transaction", func(t *testing.T) {
		command, _ := f.publish(t)
		scenario := newPersistenceRetrySQL("lock_wait")
		blocker := f.retryBlocker(t)
		defer blocker.Rollback()
		call := f.invokeRetry(t, f.retryPool(t, scenario), workers.DefaultPersistenceRetryOptions())
		lifecycleAwait(t, call.done)
		requireRetrySQL(t, scenario, 1205)
		if call.err != nil || scenario.begins.Load() != 2 || scenario.rollbacks.Load() != 1 || scenario.commits.Load() != 1 {
			t.Fatal("lock timeout did not roll back the first transaction before retry", call.err)
		}
		if err := call.stream.Close(ctx); err != nil {
			t.Fatal(err)
		}
		f.checkDurableSet(t, command)
		f.checkEmpty(t)
	})
	run("exhaustion_preserves_original_delivery", func(t *testing.T) {
		command, receipt := f.publish(t)
		scenario := newPersistenceRetrySQL("lock_wait")
		scenario.everyAttempt = true
		blocker := f.retryBlocker(t)
		defer blocker.Rollback()
		call := f.invokeRetry(t, f.retryPool(t, scenario), workers.DefaultPersistenceRetryOptions())
		lifecycleAwait(t, call.done)
		requireRetrySQL(t, scenario, 1205)
		var failure *workers.PersistenceError
		if !errors.Is(call.err, workers.ErrPersistenceRetriesExhausted) || !errors.As(call.err, &failure) || failure.Attempts != 3 ||
			workers.ClassifyPersistenceFailure(call.err) != workers.FailureTransactionRetryable ||
			scenario.begins.Load() != 3 || scenario.rollbacks.Load() != 3 || scenario.commits.Load() != 0 || f.count(t, command) != 0 {
			t.Fatal("exhaustion was unbounded, committed partial work or changed failure class", call.err)
		}
		if _, err := call.stream.Next(ctx); !errors.Is(err, rabbitmq.ErrConsumerBusy) {
			t.Fatal("exhausted delivery was disposed", err)
		}
		if err := blocker.Rollback(); err != nil {
			t.Fatal(err)
		}
		if err := call.stream.Close(ctx); err != nil {
			t.Fatal(err)
		}
		f.recover(t, receipt, database.CommandApplied)
		f.checkDurableSet(t, command)
		f.checkEmpty(t)
	})
	run("rollback_response_failure_overrides_lock_timeout", func(t *testing.T) {
		command, receipt := f.publish(t)
		scenario := newPersistenceRetrySQL("lock_wait")
		scenario.rollbackError = io.ErrUnexpectedEOF // Actual Rollback succeeds first.
		blocker := f.retryBlocker(t)
		defer blocker.Rollback()
		call := f.invokeRetry(t, f.retryPool(t, scenario), workers.DefaultPersistenceRetryOptions())
		lifecycleAwait(t, call.done)
		requireRetrySQL(t, scenario, 1205)
		if !errors.Is(call.err, database.ErrCommandRollbackUnknown) || workers.ClassifyPersistenceFailure(call.err) != workers.FailureRollbackUnknown ||
			scenario.begins.Load() != 1 || scenario.rollbacks.Load() != 1 || f.count(t, command) != 0 {
			t.Fatal("unsafe local retry followed a failed rollback response", call.err)
		}
		blocker.Rollback()
		if err := call.stream.Close(ctx); err != nil {
			t.Fatal(err)
		}
		f.recover(t, receipt, database.CommandApplied)
		f.checkDurableSet(t, command)
		f.checkEmpty(t)
	})
	run("commit_unknown_overrides_retryable_server_code", func(t *testing.T) {
		command, receipt := f.publish(t)
		scenario := newPersistenceRetrySQL("")
		scenario.commitError = &mysql.MySQLError{Number: 1213, Message: "injected after actual commit success"}
		call := f.invokeRetry(t, f.retryPool(t, scenario), workers.DefaultPersistenceRetryOptions())
		lifecycleAwait(t, call.done)
		if !errors.Is(call.err, database.ErrCommandCommitUnknown) || workers.ClassifyPersistenceFailure(call.err) != workers.FailureCommitUnknown ||
			scenario.begins.Load() != 1 || scenario.commits.Load() != 1 || f.count(t, command) != 1 {
			t.Fatal("commit uncertainty was hidden by its server error code", call.err)
		}
		if err := call.stream.Close(ctx); err != nil {
			t.Fatal(err)
		}
		f.recover(t, receipt, database.CommandReplayed)
		f.checkDurableSet(t, command)
		f.checkEmpty(t)
	})
	run("real_constraint_error_is_not_retried_or_disposed", func(t *testing.T) {
		command, receipt := f.publish(t)
		var nextSeq uint64
		if err := f.pool.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0)+1 FROM message").Scan(&nextSeq); err != nil {
			t.Fatal(err)
		}
		// The numeric literal comes only from this owned test schema's query.
		if _, err := f.pool.ExecContext(ctx, fmt.Sprintf("ALTER TABLE message ADD CONSTRAINT chk_retry_fixture CHECK (seq <> %d)", nextSeq)); err != nil {
			t.Fatal(err)
		}
		var dropped bool
		t.Cleanup(func() {
			if !dropped {
				f.pool.ExecContext(context.Background(), "ALTER TABLE message DROP CHECK chk_retry_fixture")
			}
		})
		scenario := newPersistenceRetrySQL("")
		call := f.invokeRetry(t, f.retryPool(t, scenario), workers.DefaultPersistenceRetryOptions())
		lifecycleAwait(t, call.done)
		var server *mysql.MySQLError
		if !errors.As(call.err, &server) || server.Number != 3819 || workers.ClassifyPersistenceFailure(call.err) != workers.FailureTechnical ||
			scenario.begins.Load() != 1 || scenario.rollbacks.Load() != 1 || f.count(t, command) != 0 {
			t.Fatal("constraint failure was retried, lost or committed partial data", call.err)
		}
		if _, err := f.pool.ExecContext(ctx, "ALTER TABLE message DROP CHECK chk_retry_fixture"); err != nil {
			t.Fatal(err)
		}
		dropped = true
		if err := call.stream.Close(ctx); err != nil {
			t.Fatal(err)
		}
		f.recover(t, receipt, database.CommandApplied)
		f.checkDurableSet(t, command)
		f.checkEmpty(t)
	})
	for _, force := range []bool{false, true} {
		name := "service_drain_keeps_admitted_retry_and_ack"
		if force {
			name = "service_close_cancels_backoff_without_ack"
		}
		run(name, func(t *testing.T) {
			command, receipt := f.publish(t)
			scenario := newPersistenceRetrySQL("lock_wait")
			blocker := f.retryBlocker(t)
			defer blocker.Rollback()
			pool := f.retryPool(t, scenario)
			o := workers.DefaultPersistenceOptions()
			o.ProcessingTimeout = 10 * time.Second
			o.Retry = workers.PersistenceRetryOptions{MaxAttempts: 3, BaseDelay: 2 * time.Second, MaxDelay: 2 * time.Second}
			s, err := workers.NewPersistenceService(pool, []workers.PersistenceConsumer{{Session: f.session(t, f.broker), Prefetch: 1}}, o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				if err := s.Close(cleanup); errors.Is(err, workers.ErrPersistenceServicePending) {
					t.Error(err)
				}
			})
			setup, stop := context.WithTimeout(ctx, 10*time.Second)
			err = s.Start(setup, ctx)
			stop()
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-scenario.faults:
				var server *mysql.MySQLError
				if !errors.As(err, &server) || server.Number != 1205 {
					t.Fatal("expected real lock timeout before drain", err)
				}
			case <-s.Done():
				t.Fatal("service stopped before local retry", s.Err())
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			s.StopAdmission()
			if force {
				err := s.Close(ctx)
				if !errors.Is(err, context.Canceled) || workers.ClassifyPersistenceFailure(err) != workers.FailureCancelled ||
					scenario.begins.Load() != 1 || scenario.rollbacks.Load() != 1 || scenario.commits.Load() != 0 || f.count(t, command) != 0 {
					t.Fatal("forced close retried or ACKed the cancelled backoff", err)
				}
				f.recover(t, receipt, database.CommandApplied)
				f.checkDurableSet(t, command)
				f.checkEmpty(t)
				return
			}
			short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
			err = s.Drain(short)
			cancel()
			if !errors.Is(err, workers.ErrPersistenceServicePending) || s.ConsumerReady() {
				t.Fatal("graceful drain did not retain the admitted backoff", err)
			}
			if err := s.Drain(ctx); err != nil {
				t.Fatal("admitted local retry failed during drain", err)
			}
			if err := s.Close(ctx); err != nil || scenario.begins.Load() != 2 || scenario.commits.Load() != 1 {
				t.Fatal("service retry configuration was not honored", err)
			}
			f.checkDurableSet(t, command)
			f.checkEmpty(t)
		})
	}
	var gaps int
	if err := f.pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation c WHERE
		c.last_seq<>(SELECT COUNT(*) FROM message m WHERE m.conversation_id=c.id) OR
		c.last_seq<>(SELECT COALESCE(MAX(m.seq),0) FROM message m WHERE m.conversation_id=c.id)`).Scan(&gaps); err != nil || gaps != 0 {
		t.Fatal("transaction retries left a sequence hole", err)
	}
}

type retrySQLConnector struct {
	base     driver.Connector
	scenario *persistenceRetrySQL
}

func (c retrySQLConnector) Driver() driver.Driver { return c.base.Driver() }
func (c retrySQLConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.(driver.ExecerContext).ExecContext(ctx, "SET SESSION innodb_lock_wait_timeout=1", nil); err != nil {
		conn.Close()
		return nil, err
	}
	return retrySQLConn{Conn: conn, scenario: c.scenario}, nil
}

type retrySQLConn struct {
	driver.Conn
	scenario *persistenceRetrySQL
}

func (c retrySQLConn) Ping(ctx context.Context) error { return c.Conn.(driver.Pinger).Ping(ctx) }
func (c retrySQLConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}
func (c retrySQLConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	executor := c.Conn.(driver.ExecerContext)
	result, err := executor.ExecContext(ctx, query, args)
	if err != nil || !strings.HasPrefix(query, "INSERT INTO message\n") || c.scenario.mode == "" {
		return result, err
	}
	s := c.scenario
	if s.hooks.Add(1) != 1 && !s.everyAttempt {
		return result, nil
	}
	if s.mode == "deadlock" {
		if _, err := executor.ExecContext(ctx, "UPDATE persistence_retry_lock SET v=v+1 WHERE id=1", nil); err != nil {
			return nil, err
		}
	}
	s.entered <- struct{}{}
	if s.mode == "deadlock" {
		select {
		case <-s.proceed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	_, err = executor.ExecContext(ctx, "UPDATE persistence_retry_lock SET v=v+1 WHERE id=2", nil)
	if err != nil {
		s.faults <- err
		return nil, err
	}
	return result, nil
}
func (c retrySQLConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	c.scenario.begins.Add(1)
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return retrySQLTx{Tx: tx, scenario: c.scenario}, nil
}

type retrySQLTx struct {
	driver.Tx
	scenario *persistenceRetrySQL
}

func (t retrySQLTx) Commit() error {
	t.scenario.commits.Add(1)
	if err := t.Tx.Commit(); err != nil {
		return err
	}
	return t.scenario.commitError
}
func (t retrySQLTx) Rollback() error {
	t.scenario.rollbacks.Add(1)
	if err := t.Tx.Rollback(); err != nil {
		return err
	}
	return t.scenario.rollbackError
}
