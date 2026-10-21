package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"

	"iChat/messaging/rabbitmq"
)

type quarantineReplayTestState struct {
	row    []driver.Value
	stage  string
	events []string
	args   []driver.NamedValue
}
type quarantineReplayTestConnector struct{ state *quarantineReplayTestState }
type quarantineReplayTestConn struct{ state *quarantineReplayTestState }
type quarantineReplayTestTx struct{ state *quarantineReplayTestState }

func (quarantineReplayTestConnector) Driver() driver.Driver { return nil }
func (c quarantineReplayTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &quarantineReplayTestConn{c.state}, nil
}
func (*quarantineReplayTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (*quarantineReplayTestConn) Close() error { return nil }
func (c *quarantineReplayTestConn) Begin() (driver.Tx, error) {
	c.state.events = append(c.state.events, "begin")
	if c.state.stage == "begin" {
		return nil, errors.New("secret SQL begin")
	}
	return &quarantineReplayTestTx{c.state}, nil
}
func (c *quarantineReplayTestConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.state.events = append(c.state.events, "read")
	if !strings.HasSuffix(query, " FOR UPDATE") || len(args) != 1 || args[0].Value != c.state.row[0] {
		return nil, errors.New("unexpected read")
	}
	if c.state.stage == "read" {
		return nil, errors.New("secret SQL read")
	}
	if c.state.stage == "missing" {
		return &quarantineTestRows{}, nil
	}
	return &quarantineTestRows{row: c.state.row}, nil
}
func (c *quarantineReplayTestConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.state.events = append(c.state.events, "update")
	c.state.args = args
	if query != updateQuarantineReplaySQL {
		return nil, errors.New("unexpected update")
	}
	if c.state.stage == "update" || c.state.stage == "rollback" {
		return nil, errors.New("secret SQL update")
	}
	if c.state.stage == "zero" {
		return driver.RowsAffected(0), nil
	}
	return driver.RowsAffected(1), nil
}
func (tx *quarantineReplayTestTx) Commit() error {
	tx.state.events = append(tx.state.events, "commit")
	if tx.state.stage == "commit" {
		return errors.New("secret SQL commit")
	}
	return nil
}
func (tx *quarantineReplayTestTx) Rollback() error {
	tx.state.events = append(tx.state.events, "rollback")
	if tx.state.stage == "rollback" {
		return errors.New("secret SQL rollback")
	}
	return nil
}

func TestQuarantineReplayRepository(t *testing.T) {
	row := quarantineQueryTestRow(t, true)
	input, err := rabbitmq.DecodeQuarantineAudit(row[6].([]byte))
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := input.Identity()
	expected := QuarantineDetail{Summary: QuarantineSummary{TransportID: row[0].(string), State: "open"}, Input: input}
	receipt := rabbitmq.PublishResult{Outcome: rabbitmq.PublishAccepted, CommandID: identity.CommandID, PublishAttemptID: "171c6c89-bfa6-4bd1-a738-cfa7d7a63164", ConnectionEpoch: 1, ChannelEpoch: 1, DeliveryTag: 1}
	for _, stage := range []string{"success", "begin", "read", "missing", "update", "zero", "commit", "rollback", "changed", "corrupt", "resolved"} {
		t.Run(stage, func(t *testing.T) {
			s := &quarantineReplayTestState{row: append([]driver.Value(nil), row...), stage: stage}
			if stage == "changed" {
				s.row[9], s.row[10] = "replay_pending", "a4e3044c-5f44-4d1b-9d61-a35ab19ec01d"
			}
			if stage == "corrupt" {
				s.row[5] = []byte("changed")
			}
			if stage == "resolved" {
				s.row[9], s.row[12] = "resolved", "2026-10-22 02:03:04.000000"
			}
			pool := sql.OpenDB(quarantineReplayTestConnector{s})
			defer pool.Close()
			repo, _ := NewQuarantineRepository(pool)
			err := repo.RecordReplay(context.Background(), expected, receipt)
			if stage == "success" {
				if err != nil || !reflect.DeepEqual(s.events, []string{"begin", "read", "update", "commit"}) || s.args[0].Value != receipt.PublishAttemptID || s.args[3].Value != nil {
					t.Fatal("confirmed replay transition failed", s.events, err)
				}
			} else if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatal("failure accepted or leaked", stage, err)
			}
			if stage == "commit" && !errors.Is(err, ErrQuarantineCommitUnknown) {
				t.Fatal("lost commit uncertainty", err)
			}
			if stage == "rollback" && !errors.Is(err, ErrQuarantineRollbackUnknown) {
				t.Fatal("lost rollback uncertainty", err)
			}
			if (stage == "changed" || stage == "resolved" || stage == "zero") && !errors.Is(err, ErrQuarantineReplayChanged) {
				t.Fatal("stale snapshot accepted", err)
			}
			if stage == "corrupt" && !errors.Is(err, ErrQuarantineInvariant) {
				t.Fatal("corrupt source accepted", err)
			}
			if stage == "missing" && !errors.Is(err, ErrQuarantineNotFound) {
				t.Fatal(err)
			}
		})
	}
	s := &quarantineReplayTestState{row: row}
	pool := sql.OpenDB(quarantineReplayTestConnector{s})
	defer pool.Close()
	repo, _ := NewQuarantineRepository(pool)
	for _, change := range []func(*rabbitmq.PublishResult){
		func(r *rabbitmq.PublishResult) { r.Outcome = rabbitmq.PublishUnknown },
		func(r *rabbitmq.PublishResult) { r.CommandID = "different" },
		func(r *rabbitmq.PublishResult) { r.PublishAttemptID = "bad" },
		func(r *rabbitmq.PublishResult) { r.DeliveryTag = 0 },
	} {
		bad := receipt
		change(&bad)
		if err := repo.RecordReplay(context.Background(), expected, bad); !errors.Is(err, ErrQuarantineQuery) || len(s.events) != 0 {
			t.Fatal("invalid receipt reached SQL", err)
		}
	}
}
