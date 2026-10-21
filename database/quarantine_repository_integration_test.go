//go:build integration

package database

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/migrations"
	"iChat/tests/testenv"

	mysql "github.com/go-sql-driver/mysql"
)

func TestQuarantineRepositoryIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, err := testenv.Start(ctx, filepath.Join("..", "docker-compose.test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("quarantine audit fixture run=%s", env.RunID())
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
	options := mysql.NewConfig()
	options.User, options.Passwd, options.DBName = conf.MYSQL.User, conf.MYSQL.Password, conf.MYSQL.DbName
	options.Net, options.Addr = "tcp", net.JoinHostPort(conf.MYSQL.Host, conf.MYSQL.Port)
	options.ParseTime, options.InterpolateParams = true, true
	options.Timeout, options.ReadTimeout, options.WriteTimeout = 5*time.Second, 10*time.Second, 10*time.Second
	options.Loc = time.FixedZone("audit-driver-offset", 13*60*60)
	options.Params = map[string]string{"charset": "utf8mb4", "time_zone": "'+05:30'", "transaction_isolation": "'READ-COMMITTED'"}
	connector, err := mysql.NewConnector(options)
	if err != nil {
		t.Fatal(err)
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
		t.Fatal("refusing writes outside owned schema", err)
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var initialName, initialDDL string
	if err := pool.QueryRowContext(ctx, "SHOW CREATE TABLE message_quarantine").Scan(&initialName, &initialDDL); err != nil {
		t.Fatal(err)
	}
	repo, err := NewQuarantineRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(t *testing.T, query string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	count := func(t *testing.T, id string, want int) {
		t.Helper()
		var n int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_quarantine WHERE transport_id=?", id).Scan(&n); err != nil || n != want {
			t.Fatal("unexpected audit count", n, want, err)
		}
	}
	document := func(t *testing.T, id string) []byte {
		t.Helper()
		var raw []byte
		if err := pool.QueryRowContext(ctx, "SELECT raw_headers FROM message_quarantine WHERE transport_id=?", id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	save := func(t *testing.T, input rabbitmq.QuarantineInput, want QuarantineAuditDisposition) {
		t.Helper()
		got, err := repo.Save(ctx, input)
		if err != nil || got.Disposition != want || got.TransportID != input.Envelope().TransportID() {
			var sqlErr *mysql.MySQLError
			if errors.As(err, &sqlErr) {
				t.Logf("fixture MySQL error code=%d state=%s", sqlErr.Number, sqlErr.SQLState)
			}
			t.Fatal(got, err)
		}
		count(t, got.TransportID, 1)
	}
	var canonical rabbitmq.QuarantineInput
	t.Run("first_complete_snapshot", func(t *testing.T) {
		canonical = quarantineRepositoryInput(t, quarantineRepositorySource(t, "canonical source", true), false)
		save(t, canonical, QuarantineInserted)
		stored, err := rabbitmq.DecodeQuarantineAudit(document(t, canonical.Envelope().TransportID()))
		if err != nil {
			t.Fatal(err)
		}
		a, _ := canonical.EncodeAudit()
		b, _ := stored.EncodeAudit()
		if !bytes.Equal(a, b) {
			t.Fatal("SQL JSON normalized away input content")
		}
	})
	t.Run("formats_null_identity_and_status", func(t *testing.T) {
		for _, raw := range []bool{false, true} {
			for _, valid := range []bool{false, true} {
				input := quarantineRepositoryInput(t, quarantineRepositorySource(t, fmt.Sprintf("format-%t-%t", raw, valid), valid), raw)
				save(t, input, QuarantineInserted)
				stored, err := rabbitmq.DecodeQuarantineAudit(document(t, input.Envelope().TransportID()))
				if err != nil {
					t.Fatal(err)
				}
				a, _ := input.EncodeAudit()
				b, _ := stored.EncodeAudit()
				if !bytes.Equal(a, b) {
					t.Fatal("lost full observation")
				}
				var sender, client, command, hash sql.NullString
				if err := pool.QueryRowContext(ctx, "SELECT sender_id,client_message_id,command_id,payload_hash FROM message_quarantine WHERE transport_id=?", input.Envelope().TransportID()).Scan(&sender, &client, &command, &hash); err != nil {
					t.Fatal(err)
				}
				if sender.Valid != valid || client.Valid != valid || command.Valid != valid || hash.Valid != valid {
					t.Fatal("partial/unverified query identity stored")
				}
				if valid {
					identity, _ := input.Identity()
					reader, _ := NewMessageRepository(pool)
					status, err := reader.GetCommandStatus(ctx, 12, identity.ClientMessageID, identity.PayloadHash)
					if err != nil || status.State != messaging.StatusQuarantined {
						t.Fatal(status, err)
					}
				}
				save(t, input, QuarantineExisting)
			}
		}
	})
	t.Run("concurrent_same_transport_and_wrapper_dlx_equivalence", func(t *testing.T) {
		source := quarantineRepositorySource(t, "concurrent source", true)
		wrapper := quarantineRepositoryInput(t, source, false)
		raw := quarantineRepositoryInput(t, source, true)
		start := make(chan struct{})
		type response struct {
			result QuarantineAuditResult
			err    error
		}
		done := make(chan response, 12)
		for i := 0; i < 12; i++ {
			input := wrapper
			if i%2 == 1 {
				input = raw
			}
			go func() { <-start; r, err := repo.Save(ctx, input); done <- response{r, err} }()
		}
		close(start)
		inserted, existing := 0, 0
		for i := 0; i < 12; i++ {
			select {
			case got := <-done:
				if got.err != nil {
					t.Error(got.err)
				} else if got.result.Disposition == QuarantineInserted {
					inserted++
				} else if got.result.Disposition == QuarantineExisting {
					existing++
				} else {
					t.Error("missing disposition")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		if inserted != 1 || existing != 11 {
			t.Fatal("concurrent audit did not converge", inserted, existing)
		}
		count(t, wrapper.Envelope().TransportID(), 1)
	})
	t.Run("replay_and_resolved_state_are_preserved", func(t *testing.T) {
		id := canonical.Envelope().TransportID()
		first := document(t, id)
		for _, state := range []string{"replay_pending", "resolved"} {
			var resolved any
			if state == "resolved" {
				resolved = "2026-10-21 02:03:04.000000"
			}
			exec(t, "UPDATE message_quarantine SET state=?,replay_attempt_id=?,resolved_at=? WHERE transport_id=?", state, "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", resolved, id)
			save(t, canonical, QuarantineExisting)
			var got, attempt string
			var stamp sql.NullString
			if err := pool.QueryRowContext(ctx, "SELECT state,replay_attempt_id,DATE_FORMAT(resolved_at,'%Y-%m-%d %H:%i:%s.%f') FROM message_quarantine WHERE transport_id=?", id).Scan(&got, &attempt, &stamp); err != nil || got != state || attempt != "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1" || stamp.Valid != (state == "resolved") {
				t.Fatal("replay state overwritten", err)
			}
			if !bytes.Equal(first, document(t, id)) {
				t.Fatal("first observation overwritten")
			}
		}
	})
	t.Run("corrupt_and_conflicting_audit_is_retained", func(t *testing.T) {
		input := quarantineRepositoryInput(t, quarantineRepositorySource(t, "integrity source", false), false)
		save(t, input, QuarantineInserted)
		id := input.Envelope().TransportID()
		original := document(t, id)
		for _, query := range []string{"UPDATE message_quarantine SET raw_body=_binary'changed' WHERE transport_id=?", "UPDATE message_quarantine SET raw_headers=JSON_OBJECT('audit_version',2) WHERE transport_id=?", "UPDATE message_quarantine SET sender_id=999 WHERE transport_id=?"} {
			exec(t, query, id)
			got, err := repo.Save(ctx, input)
			if !errors.Is(err, ErrQuarantineInvariant) || got != (QuarantineAuditResult{}) {
				t.Fatal(got, err)
			}
			source, _ := input.Envelope().Source()
			exec(t, "UPDATE message_quarantine SET raw_body=?,raw_headers=?,sender_id=NULL WHERE transport_id=?", source.Body, string(original), id)
		}
		other := quarantineRepositoryInput(t, quarantineRepositorySource(t, "wrong key content", false), false)
		otherDoc, _ := other.EncodeAudit()
		otherSource, _ := other.Envelope().Source()
		exec(t, "UPDATE message_quarantine SET raw_body=?,raw_headers=? WHERE transport_id=?", otherSource.Body, string(otherDoc), id)
		before := document(t, id)
		got, err := repo.Save(ctx, input)
		if !errors.Is(err, ErrQuarantineConflict) || got != (QuarantineAuditResult{}) || !bytes.Equal(before, document(t, id)) {
			t.Fatal("content conflict overwrote audit", got, err)
		}
	})
	t.Run("large_wrapper_keeps_full_metadata", func(t *testing.T) {
		source := quarantineRepositorySource(t, "large source", false)
		source.Body = bytes.Repeat([]byte{0xff, 0}, 1024*1024)
		input := quarantineRepositoryInput(t, source, false)
		delivery, _ := input.Delivery()
		delivery.Headers["large-metadata"] = bytes.Repeat([]byte{0, 255}, 768*1024)
		input, err = rabbitmq.ParseQuarantineInput(delivery, input.ReceivedAt())
		if err != nil {
			t.Fatal(err)
		}
		save(t, input, QuarantineInserted)
		stored, err := rabbitmq.DecodeQuarantineAudit(document(t, input.Envelope().TransportID()))
		if err != nil {
			t.Fatal(err)
		}
		a, _ := stored.EncodeAudit()
		b, _ := input.EncodeAudit()
		if !bytes.Equal(a, b) {
			t.Fatal("large input truncated")
		}
		t.Logf("large fixture source=%d document=%d metadata=%d", len(source.Body), len(a), len(delivery.Headers["large-metadata"].([]byte)))
	})
	for _, stage := range []string{"constraint_error", "insert_error_after", "commit_error_before", "commit_error_after", "rollback_error_after", "cancel_after_insert"} {
		t.Run(stage, func(t *testing.T) {
			input := quarantineRepositoryInput(t, quarantineRepositorySource(t, stage, false), false)
			fault := &quarantineFaultConnector{inner: connector, stage: stage, entered: make(chan struct{})}
			faultPool := sql.OpenDB(fault)
			faultRepo, _ := NewQuarantineRepository(faultPool)
			t.Cleanup(func() {
				if err := faultPool.Close(); err != nil {
					t.Error(err)
				}
			})
			if stage == "rollback_error_after" {
				save(t, input, QuarantineInserted)
			}
			var result QuarantineAuditResult
			var callErr error
			if stage == "cancel_after_insert" {
				callCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				done := make(chan struct{})
				go func() { result, callErr = faultRepo.Save(callCtx, input); close(done) }()
				select {
				case <-fault.entered:
				case <-done:
					t.Fatal("SQL barrier did not enter", callErr)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				count(t, input.Envelope().TransportID(), 0)
				cancel()
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if !errors.Is(callErr, context.Canceled) {
					t.Fatal(callErr)
				}
			} else {
				result, callErr = faultRepo.Save(ctx, input)
			}
			if callErr == nil || result != (QuarantineAuditResult{}) || !fault.triggered.Load() {
				t.Fatal("injected uncertainty accepted", result, callErr)
			}
			if stage == "commit_error_before" || stage == "commit_error_after" {
				if !errors.Is(callErr, ErrQuarantineCommitUnknown) {
					t.Fatal(callErr)
				}
			}
			if stage == "rollback_error_after" && !errors.Is(callErr, ErrQuarantineRollbackUnknown) {
				t.Fatal(callErr)
			}
			if stage == "constraint_error" {
				var sqlErr *mysql.MySQLError
				if !errors.As(callErr, &sqlErr) || sqlErr.Number != 3819 {
					t.Fatal("CHECK failure was not observed", callErr)
				}
			}
			if stage == "commit_error_after" || stage == "rollback_error_after" {
				count(t, input.Envelope().TransportID(), 1)
				save(t, input, QuarantineExisting)
			} else {
				count(t, input.Envelope().TransportID(), 0)
				save(t, input, QuarantineInserted)
			}
		})
	}
	for _, table := range []string{"conversation", "message", "message_command", "message_outbox", "message_delivery_cursor"} {
		var n int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"`").Scan(&n); err != nil || n != 0 {
			t.Fatal("audit wrote business state", table, n, err)
		}
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		var name, ddl string
		if readErr := pool.QueryRowContext(ctx, "SHOW CREATE TABLE message_quarantine").Scan(&name, &ddl); readErr == nil && ddl != initialDDL {
			t.Logf("fixture DDL before:\n%s\nafter:\n%s", initialDDL, ddl)
		}
		t.Fatal("schema fingerprint changed", err)
	}
}

// Faults wrap a real MySQL transaction. COMMIT response errors are injected
// after actual commit/rollback; they do not claim SQL TCP response loss.
type quarantineFaultConnector struct {
	inner     driver.Connector
	stage     string
	entered   chan struct{}
	once      sync.Once
	triggered atomic.Bool
}

func (c *quarantineFaultConnector) Driver() driver.Driver { return c.inner.Driver() }
func (c *quarantineFaultConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &quarantineFaultConn{Conn: conn, fault: c}, nil
}

type quarantineFaultConn struct {
	driver.Conn
	fault *quarantineFaultConnector
}

func (c *quarantineFaultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &quarantineFaultTx{Tx: tx, fault: c.fault}, nil
}
func (c *quarantineFaultConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}
func (c *quarantineFaultConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if c.fault.stage == "constraint_error" && q == insertQuarantineAuditSQL {
		args = append([]driver.NamedValue(nil), args...)
		args[0].Value = "" // Trigger the existing identity CHECK, without ALTER.
		c.fault.triggered.Store(true)
	}
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
	if err != nil || q != insertQuarantineAuditSQL {
		return result, err
	}
	if c.fault.stage == "insert_error_after" {
		c.fault.triggered.Store(true)
		return nil, io.ErrUnexpectedEOF
	}
	if c.fault.stage == "cancel_after_insert" {
		c.fault.triggered.Store(true)
		c.fault.once.Do(func() { close(c.fault.entered) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return result, nil
}

type quarantineFaultTx struct {
	driver.Tx
	fault *quarantineFaultConnector
}

func (tx *quarantineFaultTx) Commit() error {
	if tx.fault.stage == "commit_error_before" {
		if err := tx.Tx.Rollback(); err != nil {
			return err
		}
		tx.fault.triggered.Store(true)
		return io.ErrUnexpectedEOF
	}
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	if tx.fault.stage == "commit_error_after" {
		tx.fault.triggered.Store(true)
		return io.ErrUnexpectedEOF
	}
	return nil
}
func (tx *quarantineFaultTx) Rollback() error {
	if err := tx.Tx.Rollback(); err != nil {
		return err
	}
	if tx.fault.stage == "rollback_error_after" {
		tx.fault.triggered.Store(true)
		return io.ErrUnexpectedEOF
	}
	return nil
}
