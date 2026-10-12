package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"iChat/messaging"
	"io"
	"reflect"
	"strings"
	"testing"
)

type statusTestState struct {
	stage            string
	cause            error
	command, message []driver.Value
	quarantined      bool
	events           []string
	queries          []string
	arguments        [][]driver.NamedValue
}
type statusTestConnector struct{ state *statusTestState }
type statusTestConn struct{ state *statusTestState }
type statusTestTx struct{ state *statusTestState }
type statusTestRows struct {
	count  int
	values []driver.Value
	done   bool
}

func (c statusTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &statusTestConn{state: c.state}, nil
}
func (statusTestConnector) Driver() driver.Driver { return commandTestDriver{} }
func (*statusTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("status must not prepare/write")
}
func (*statusTestConn) Close() error { return nil }
func (*statusTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("status requires explicit snapshot options")
}
func (c *statusTestConn) BeginTx(_ context.Context, options driver.TxOptions) (driver.Tx, error) {
	c.state.events = append(c.state.events, "begin")
	if options.Isolation != driver.IsolationLevel(sql.LevelRepeatableRead) || !options.ReadOnly {
		return nil, fmt.Errorf("unsafe status transaction options: %+v", options)
	}
	if c.state.stage == "begin" {
		return nil, c.state.cause
	}
	return &statusTestTx{state: c.state}, nil
}
func (*statusTestTx) Commit() error { return errors.New("read-only status must release with rollback") }
func (tx *statusTestTx) Rollback() error {
	tx.state.events = append(tx.state.events, "rollback")
	if tx.state.stage == "rollback" {
		return tx.state.cause
	}
	return nil
}
func (c *statusTestConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.state.queries = append(c.state.queries, query)
	c.state.arguments = append(c.state.arguments, append([]driver.NamedValue(nil), args...))
	if strings.Contains(query, "FOR UPDATE") || strings.Contains(query, "FOR SHARE") {
		return nil, errors.New("status must not use locking reads")
	}
	var stage string
	var rows *statusTestRows
	switch query {
	case selectStatusCommandSQL:
		stage, rows = "command", &statusTestRows{count: 10, values: c.state.command}
	case selectStatusMessageSQL:
		stage, rows = "message", &statusTestRows{count: 10, values: c.state.message}
	case selectStatusQuarantineSQL:
		stage, rows = "quarantine", &statusTestRows{count: 1}
		if c.state.quarantined {
			rows.values = []driver.Value{int64(1)}
		}
	default:
		return nil, fmt.Errorf("unexpected status query: %s", query)
	}
	c.state.events = append(c.state.events, stage)
	if c.state.stage == stage {
		return nil, c.state.cause
	}
	return rows, nil
}
func (r *statusTestRows) Columns() []string { return make([]string, r.count) }
func (*statusTestRows) Close() error        { return nil }
func (r *statusTestRows) Next(values []driver.Value) error {
	if r.done || r.values == nil {
		return io.EOF
	}
	r.done = true
	copy(values, r.values)
	return nil
}

func statusTestRepository(t *testing.T, state *statusTestState) *MessageRepository {
	t.Helper()
	pool := sql.OpenDB(statusTestConnector{state: state})
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	repo, err := NewMessageRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func statusTestCommandRow(command messaging.Command, state CommandState) []driver.Value {
	row := []driver.Value{int64(1), command.ClientMessageID, command.CommandID, command.PayloadHash, string(state), nil, nil, nil,
		"2026-10-11 00:00:00.123456", "2026-10-11 00:00:01.123456"}
	if state == CommandPersisted {
		row[6], row[7] = command.CommandID, command.ConversationID
	} else {
		row[5] = ReasonNotFriends
	}
	return row
}
func statusTestMessageRow(command messaging.Command) []driver.Value {
	return []driver.Value{command.CommandID, command.ClientMessageID, int64(1), int64(2), string(command.TargetType), string(command.Media), command.Content, command.ConversationID, int64(42), "2026-10-11 00:00:00.654321"}
}

func TestCommandStatusDecisionsAndReadBoundaries(t *testing.T) {
	command := testCommand(t)
	for _, tc := range []struct {
		name   string
		state  *statusTestState
		hash   string
		want   messaging.StatusState
		events []string
	}{
		{"persisted", &statusTestState{command: statusTestCommandRow(command, CommandPersisted), message: statusTestMessageRow(command), quarantined: true}, command.PayloadHash, messaging.StatusPersisted, []string{"begin", "command", "message", "rollback"}},
		{"rejected", &statusTestState{command: statusTestCommandRow(command, CommandRejected), quarantined: true}, command.PayloadHash, messaging.StatusRejected, []string{"begin", "command", "rollback"}},
		{"conflict", &statusTestState{command: statusTestCommandRow(command, CommandPersisted), quarantined: true}, strings.Repeat("0", 64), messaging.StatusConflict, []string{"begin", "command", "rollback"}},
		{"quarantined", &statusTestState{quarantined: true}, command.PayloadHash, messaging.StatusQuarantined, []string{"begin", "command", "quarantine", "rollback"}},
		{"unknown", &statusTestState{}, command.PayloadHash, messaging.StatusPendingOrUnknown, []string{"begin", "command", "quarantine", "rollback"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, err := statusTestRepository(t, tc.state).GetCommandStatus(context.Background(), 1, strings.ToUpper(command.ClientMessageID), tc.hash)
			if err != nil || status.State != tc.want || status.ClientMessageID != command.ClientMessageID || status.PayloadHash != tc.hash || !reflect.DeepEqual(tc.state.events, tc.events) {
				t.Fatalf("status=%+v events=%v err=%v", status, tc.state.events, err)
			}
			if tc.want == messaging.StatusPersisted {
				if status.Message == nil || status.Message.Seq != 42 || status.Message.ServerCreatedAt != "2026-10-11T00:00:00.654321Z" {
					t.Fatalf("persisted DTO=%+v", status.Message)
				}
			} else if status.Message != nil {
				t.Fatal("non-success exposed canonical message")
			}
			if tc.want == messaging.StatusRejected {
				if status.ReasonCode != ReasonNotFriends {
					t.Fatal("lost rejection reason")
				}
			} else if status.ReasonCode != "" {
				t.Fatal("leaked canonical reason")
			}
			wantPoll := 0
			if tc.want == messaging.StatusQuarantined || tc.want == messaging.StatusPendingOrUnknown {
				wantPoll = messaging.DefaultStatusPollAfterMs
			}
			if status.PollAfterMs != wantPoll {
				t.Fatalf("poll interval=%d want=%d", status.PollAfterMs, wantPoll)
			}
			if tc.state.arguments[0][0].Value != int64(1) || tc.state.arguments[0][1].Value != command.ClientMessageID {
				t.Fatal("command read lost authenticated scope")
			}
		})
	}
}

func TestCommandStatusFailuresNeverReturnPending(t *testing.T) {
	command := testCommand(t)
	for _, stage := range []string{"begin", "command", "message", "quarantine", "rollback"} {
		t.Run(stage, func(t *testing.T) {
			state := &statusTestState{stage: stage, cause: io.ErrUnexpectedEOF}
			if stage == "message" {
				state.command = statusTestCommandRow(command, CommandPersisted)
			}
			status, err := statusTestRepository(t, state).GetCommandStatus(context.Background(), 1, command.ClientMessageID, command.PayloadHash)
			if !errors.Is(err, io.ErrUnexpectedEOF) || !reflect.DeepEqual(status, messaging.CommandStatus{}) {
				t.Fatalf("failed %s became a status: %+v err=%v", stage, status, err)
			}
		})
	}
}

func TestCommandStatusCorruptRecordsFailClosed(t *testing.T) {
	command := testCommand(t)
	for _, stage := range []string{"claim", "identity", "timestamp", "missing_message", "changed_content", "changed_sender", "sequence_zero", "message_timestamp"} {
		t.Run(stage, func(t *testing.T) {
			state := &statusTestState{command: statusTestCommandRow(command, CommandPersisted), message: statusTestMessageRow(command)}
			switch stage {
			case "claim":
				state.command = statusTestCommandRow(command, CommandRejected)
				state.command[5] = commandClaimPrefix + "corrupt"
			case "identity":
				state.command[2] = strings.Repeat("a", 64)
			case "timestamp":
				state.command[8] = "bad time"
			case "missing_message":
				state.message = nil
			case "changed_content":
				state.message[6] = "tampered"
			case "changed_sender":
				state.message[2] = int64(2)
			case "sequence_zero":
				state.message[8] = int64(0)
			case "message_timestamp":
				state.message[9] = "bad time"
			}
			status, err := statusTestRepository(t, state).GetCommandStatus(context.Background(), 1, command.ClientMessageID, command.PayloadHash)
			if !errors.Is(err, ErrCommandInvariant) || !reflect.DeepEqual(status, messaging.CommandStatus{}) {
				t.Fatalf("corrupt %s returned %+v err=%v", stage, status, err)
			}
		})
	}
}

func TestCommandStatusValidationBeforeIO(t *testing.T) {
	command := testCommand(t)
	var unavailable *MessageRepository
	if _, err := unavailable.GetCommandStatus(context.Background(), 1, command.ClientMessageID, command.PayloadHash); !errors.Is(err, ErrCommandRepositoryUnavailable) {
		t.Fatal(err)
	}
	state := &statusTestState{}
	repo := statusTestRepository(t, state)
	for _, values := range [][2]string{{"bad", command.PayloadHash}, {command.ClientMessageID, "BAD"}} {
		if _, err := repo.GetCommandStatus(context.Background(), 1, values[0], values[1]); !errors.Is(err, messaging.ErrInvalidInput) {
			t.Fatal(err)
		}
	}
	if _, err := repo.GetCommandStatus(context.Background(), 0, command.ClientMessageID, command.PayloadHash); !errors.Is(err, messaging.ErrInvalidInput) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.GetCommandStatus(ctx, 1, command.ClientMessageID, command.PayloadHash); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(state.events) != 0 {
		t.Fatalf("invalid/canceled query touched database: %v", state.events)
	}
}
