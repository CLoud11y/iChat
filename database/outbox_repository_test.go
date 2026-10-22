package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"iChat/messaging"
)

type outboxTestState struct {
	rows   [][]driver.Value
	stage  string
	events []string
}
type outboxTestConnector struct{ s *outboxTestState }
type outboxTestConn struct{ s *outboxTestState }
type outboxTestTx struct{ s *outboxTestState }

func (outboxTestConnector) Driver() driver.Driver { return nil }
func (c outboxTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &outboxTestConn{c.s}, nil
}
func (*outboxTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (*outboxTestConn) Close() error { return nil }
func (*outboxTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("explicit transaction required")
}
func (c *outboxTestConn) BeginTx(_ context.Context, o driver.TxOptions) (driver.Tx, error) {
	c.s.events = append(c.s.events, "begin")
	if o.Isolation != driver.IsolationLevel(sql.LevelReadCommitted) || o.ReadOnly {
		return nil, errors.New("wrong isolation")
	}
	if c.s.stage == "begin" {
		return nil, errors.New("secret begin")
	}
	return &outboxTestTx{c.s}, nil
}
func (c *outboxTestConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "UTC_TIMESTAMP") {
		c.s.events = append(c.s.events, "clock")
		return &statusTestRows{count: 1, values: []driver.Value{"2026-10-23 01:02:03.000000"}}, nil
	}
	c.s.events = append(c.s.events, "select")
	if q != selectOutboxClaimSQL || len(args) != 3 {
		return nil, errors.New("unsafe claim query")
	}
	if c.s.stage == "select" {
		return nil, errors.New("secret select")
	}
	return &outboxTestRows{rows: c.s.rows}, nil
}

type outboxTestRows struct {
	rows [][]driver.Value
	n    int
}

func (*outboxTestRows) Columns() []string { return make([]string, 13) }
func (*outboxTestRows) Close() error      { return nil }
func (r *outboxTestRows) Next(dst []driver.Value) error {
	if r.n == len(r.rows) {
		return io.EOF
	}
	copy(dst, r.rows[r.n])
	r.n++
	return nil
}
func (c *outboxTestConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.s.events = append(c.s.events, "update")
	if q != updateOutboxClaimSQL || len(args) != 6 || !messaging.ValidPublishAttemptID(args[0].Value.(string)) {
		return nil, errors.New("unsafe lease update")
	}
	if c.s.stage == "update" || c.s.stage == "rollback" {
		return nil, errors.New("secret update")
	}
	if c.s.stage == "zero" {
		return driver.RowsAffected(0), nil
	}
	return driver.RowsAffected(1), nil
}
func (tx *outboxTestTx) Commit() error {
	tx.s.events = append(tx.s.events, "commit")
	if tx.s.stage == "commit" {
		return errors.New("secret commit")
	}
	return nil
}
func (tx *outboxTestTx) Rollback() error {
	tx.s.events = append(tx.s.events, "rollback")
	if tx.s.stage == "rollback" {
		return errors.New("secret rollback")
	}
	return nil
}
func outboxTestRow() []driver.Value {
	command := strings.Repeat("a", 64)
	id, _ := messaging.OutboxEventID(command, messaging.EventCommandResult)
	return []driver.Value{id, "command.result", command, nil, nil, []byte(`{"private":"中文\u0000"}`), "pending", "2026-10-23 01:02:02.000000", nil, nil, int64(0), "2026-10-23 00:00:00.000000", nil}
}
func TestOutboxClaimTransaction(t *testing.T) {
	for _, stage := range []string{"success", "empty", "begin", "select", "update", "zero", "commit", "rollback", "invalid_later_row"} {
		t.Run(stage, func(t *testing.T) {
			s := &outboxTestState{stage: stage, rows: [][]driver.Value{outboxTestRow()}}
			if stage == "empty" {
				s.rows = nil
			}
			if stage == "invalid_later_row" {
				bad := outboxTestRow()
				bad[0] = "corrupt"
				s.rows = append(s.rows, bad)
			}
			pool := sql.OpenDB(outboxTestConnector{s})
			defer pool.Close()
			repo, _ := NewOutboxRepository(pool)
			leases, err := repo.Claim(context.Background(), OutboxClaimOptions{Limit: 2, Lease: 10 * time.Second})
			if stage == "success" {
				if err != nil || len(leases) != 1 || leases[0].Attempts != 1 || leases[0].Until.Format(mysqlMicrosecondLayout) != "2026-10-23 01:02:13.000000" || !reflect.DeepEqual(s.events, []string{"begin", "clock", "select", "update", "commit"}) {
					t.Fatal("claim not committed", err, s.events)
				}
				if strings.Contains(leases[0].GoString(), "private") {
					t.Fatal("lease log leaked payload")
				}
			} else if stage == "empty" {
				if err != nil || leases == nil || len(leases) != 0 || s.events[len(s.events)-1] != "rollback" {
					t.Fatal("empty claim failed", err)
				}
			} else {
				if err == nil || leases != nil || strings.Contains(err.Error(), "secret") {
					t.Fatal("uncertain claim returned leases or leaked", err)
				}
				if stage == "commit" && !errors.Is(err, ErrOutboxCommitUnknown) {
					t.Fatal(err)
				}
				if stage == "rollback" && !errors.Is(err, ErrOutboxRollbackUnknown) {
					t.Fatal(err)
				}
				if stage == "invalid_later_row" && !reflect.DeepEqual(s.events, []string{"begin", "clock", "select", "update", "rollback"}) {
					t.Fatal("partial batch did not roll back", s.events)
				}
			}
		})
	}
}
func TestOutboxClaimValidation(t *testing.T) {
	now := time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)
	valid := outboxClaimRow{id: outboxTestRow()[0].(string), kind: "command.result", command: strings.Repeat("a", 64), payload: []byte(`{}`), status: "leased", available: "2026-10-23 01:02:02.000000", created: "2026-10-23 00:00:00.000000", token: sql.NullString{String: "171c6c89-bfa6-4bd1-a738-cfa7d7a63164", Valid: true}, until: sql.NullString{String: "2026-10-23 01:02:03.000000", Valid: true}, attempts: 4}
	if err := validateOutboxClaim(valid, now); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*outboxClaimRow){func(r *outboxClaimRow) { r.attempts = 1<<32 - 1 }, func(r *outboxClaimRow) { r.attempts = 0 }, func(r *outboxClaimRow) { r.until.String = "2026-10-23 01:02:04.000000" }, func(r *outboxClaimRow) { r.token.String = "bad" }, func(r *outboxClaimRow) { r.payload = []byte(`bad`) }, func(r *outboxClaimRow) { r.status = "completed" }, func(r *outboxClaimRow) { r.id = "bad" }} {
		bad := valid
		mutate(&bad)
		if !errors.Is(validateOutboxClaim(bad, now), ErrOutboxInvariant) {
			t.Fatal("bad row accepted")
		}
	}
	for _, o := range []OutboxClaimOptions{{}, {Limit: 101, Lease: time.Second}, {Limit: 1, Lease: time.Millisecond}, {Limit: 1, Lease: 6 * time.Minute}} {
		if o.Validate() == nil {
			t.Fatal("bad options accepted")
		}
	}
	pool := sql.OpenDB(outboxTestConnector{&outboxTestState{}})
	defer pool.Close()
	repo, _ := NewOutboxRepository(pool)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.Claim(ctx, OutboxClaimOptions{1, time.Second}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := repo.Claim(nil, OutboxClaimOptions{1, time.Second}); !errors.Is(err, ErrOutboxClaimInput) {
		t.Fatal(err)
	}
}
