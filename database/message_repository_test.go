package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"iChat/messaging"
	"io"
	"reflect"
	"strings"
	"testing"
)

// A private connector exercises database/sql's real transaction lifecycle
// without networking. MySQL uniqueness/locking is tested separately.
type commandTestConnector struct{ state *commandTestState }
type commandTestDriver struct{}
type commandTestConn struct{ state *commandTestState }
type commandTestTx struct{ state *commandTestState }
type commandTestRows struct {
	values []driver.Value
	done   bool
}
type commandTestResult struct {
	count int64
	err   error
}

type commandTestState struct {
	stage    string
	cause    error
	events   []string
	row      []driver.Value
	existing bool
	finished bool
}

func (c commandTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &commandTestConn{state: c.state}, nil
}
func (commandTestConnector) Driver() driver.Driver { return commandTestDriver{} }
func (commandTestDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use the private connector")
}
func (*commandTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (*commandTestConn) Close() error { return nil }
func (c *commandTestConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *commandTestConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.state.events = append(c.state.events, "begin")
	if c.state.stage == "begin" {
		return nil, c.state.cause
	}
	return commandTestTx{state: c.state}, nil
}
func (c *commandTestConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	p := c.state
	if strings.HasPrefix(query, "INSERT") {
		p.events = append(p.events, "claim")
		if p.stage == "claim" {
			return nil, p.cause
		}
		if !p.existing {
			p.row = []driver.Value{args[0].Value, args[1].Value, args[2].Value, args[3].Value,
				"rejected", args[4].Value, nil, nil, "2026-10-11 01:02:03.123456", "2026-10-11 01:02:03.123456"}
		}
		return commandTestResult{count: 1}, nil
	}
	p.events = append(p.events, "finish")
	if p.stage == "finish" {
		return nil, p.cause
	}
	p.finished = true
	if p.stage == "changed_claim" {
		return commandTestResult{count: 0}, nil
	}
	if p.stage == "affected_rows" {
		return commandTestResult{err: p.cause}, nil
	}
	p.row[4], p.row[5], p.row[6], p.row[7] = args[0].Value, args[1].Value, args[2].Value, args[3].Value
	return commandTestResult{count: 1}, nil
}
func (c *commandTestConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	p := c.state
	p.events = append(p.events, "read")
	if p.stage == "read" || (p.stage == "final_read" && p.finished) {
		return nil, p.cause
	}
	if p.stage == "other_unique_key" {
		return &commandTestRows{}, nil
	}
	return &commandTestRows{values: p.row}, nil
}
func (tx commandTestTx) Commit() error {
	tx.state.events = append(tx.state.events, "commit")
	if tx.state.stage == "commit" {
		return tx.state.cause
	}
	return nil
}
func (tx commandTestTx) Rollback() error {
	tx.state.events = append(tx.state.events, "rollback")
	if tx.state.stage == "rollback" {
		return tx.state.cause
	}
	return nil
}
func (r *commandTestRows) Columns() []string {
	return []string{"sender_id", "client_message_id", "command_id", "payload_hash", "state", "reason_code", "message_id", "conversation_id", "created_at", "completed_at"}
}
func (*commandTestRows) Close() error { return nil }
func (r *commandTestRows) Next(values []driver.Value) error {
	if r.done || r.values == nil {
		return io.EOF
	}
	r.done = true
	copy(values, r.values)
	return nil
}
func (commandTestResult) LastInsertId() (int64, error)   { return 0, nil }
func (r commandTestResult) RowsAffected() (int64, error) { return r.count, r.err }

func newCommandTestRepository(t *testing.T, state *commandTestState) *MessageRepository {
	t.Helper()
	db := sql.OpenDB(commandTestConnector{state: state})
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	repo, err := NewMessageRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func testCommand(t *testing.T) messaging.Command {
	t.Helper()
	command, err := messaging.Normalize(1, messaging.SendRequest{
		ClientMessageID: "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", TargetType: messaging.Private,
		TargetID: "2", Media: messaging.Text, Content: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	return command
}

func TestCommandRepositoryValidation(t *testing.T) {
	if _, err := NewMessageRepository(nil); !errors.Is(err, ErrCommandRepositoryUnavailable) {
		t.Fatal(err)
	}
	var missing *MessageRepository
	if _, err := missing.ExecuteCommand(context.Background(), messaging.Command{}, nil); !errors.Is(err, ErrCommandRepositoryUnavailable) {
		t.Fatal(err)
	}
	state := &commandTestState{}
	repo := newCommandTestRepository(t, state)
	command := testCommand(t)
	tampered := command
	tampered.PayloadHash = strings.Repeat("a", 64)
	handler := func(context.Context, CommandTransaction, messaging.Command) (CommandDecision, error) {
		t.Fatal("invalid inputs invoked business handler")
		return CommandDecision{}, nil
	}
	if _, err := repo.ExecuteCommand(context.Background(), tampered, handler); !errors.Is(err, messaging.ErrIdentityMismatch) {
		t.Fatal(err)
	}
	if _, err := repo.ExecuteCommand(context.Background(), command, nil); !errors.Is(err, ErrInvalidCommandDecision) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.ExecuteCommand(ctx, command, handler); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(state.events) != 0 {
		t.Fatalf("invalid inputs touched DB: %v", state.events)
	}
}

func TestCommandTransactionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		stage  string
		events []string
	}{
		{"success", []string{"begin", "claim", "read", "handler", "finish", "read", "commit"}},
		{"begin", []string{"begin"}},
		{"claim", []string{"begin", "claim", "rollback"}},
		{"read", []string{"begin", "claim", "read", "rollback"}},
		{"handler", []string{"begin", "claim", "read", "handler", "rollback"}},
		{"panic", []string{"begin", "claim", "read", "handler", "rollback"}},
		{"invalid_decision", []string{"begin", "claim", "read", "handler", "rollback"}},
		{"finish", []string{"begin", "claim", "read", "handler", "finish", "rollback"}},
		{"affected_rows", []string{"begin", "claim", "read", "handler", "finish", "rollback"}},
		{"changed_claim", []string{"begin", "claim", "read", "handler", "finish", "rollback"}},
		{"final_read", []string{"begin", "claim", "read", "handler", "finish", "read", "rollback"}},
		{"commit", []string{"begin", "claim", "read", "handler", "finish", "read", "commit"}},
		{"other_unique_key", []string{"begin", "claim", "read", "rollback"}},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			state := &commandTestState{stage: tc.stage, cause: errors.New("injected failure")}
			repo := newCommandTestRepository(t, state)
			var result CommandExecution
			var err error
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				result, err = repo.ExecuteCommand(context.Background(), testCommand(t), func(_ context.Context, tx CommandTransaction, _ messaging.Command) (CommandDecision, error) {
					state.events = append(state.events, "handler")
					if _, exposesCommit := tx.(interface{ Commit() error }); exposesCommit {
						t.Fatal("handler can commit the provisional claim")
					}
					if state.stage == "handler" {
						return CommandDecision{}, state.cause
					}
					if state.stage == "panic" {
						panic(state.cause)
					}
					if state.stage == "invalid_decision" {
						return CommandDecision{State: CommandRejected, ReasonCode: commandClaimPrefix + "fake"}, nil
					}
					return CommandDecision{State: CommandPersisted}, nil
				})
			}()
			if tc.stage == "success" {
				if err != nil || result.Disposition != CommandApplied || result.Result == nil || result.Result.State != CommandPersisted {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				if result.Result.CreatedAt.Location().String() != "UTC" || result.Result.CreatedAt.Nanosecond() != 123456000 {
					t.Fatalf("wrong UTC/microsecond timestamps: %+v", result.Result)
				}
			} else if tc.stage == "panic" {
				if recovered != state.cause {
					t.Fatalf("panic=%v", recovered)
				}
			} else {
				if err == nil || result.Result != nil || result.Disposition != "" {
					t.Fatalf("failed transaction returned a terminal result: %+v err=%v", result, err)
				}
				if tc.stage == "commit" && (!errors.Is(err, ErrCommandCommitUnknown) || !errors.Is(err, state.cause)) {
					t.Fatalf("commit failure lost uncertainty/cause: %v", err)
				}
				if tc.stage == "other_unique_key" || tc.stage == "changed_claim" {
					if !errors.Is(err, ErrCommandInvariant) {
						t.Fatal(err)
					}
				}
			}
			if !reflect.DeepEqual(state.events, tc.events) {
				t.Fatalf("events=%v want=%v", state.events, tc.events)
			}
		})
	}
}

func TestCommandReplayRollbackFailure(t *testing.T) {
	command := testCommand(t)
	state := &commandTestState{stage: "rollback", cause: io.ErrUnexpectedEOF, existing: true,
		row: []driver.Value{int64(1), command.ClientMessageID, command.CommandID, command.PayloadHash,
			"rejected", "NOT_FRIENDS", nil, nil, "2026-10-11 01:02:03.123456", "2026-10-11 01:02:03.123456"}}
	repo := newCommandTestRepository(t, state)
	result, err := repo.ExecuteCommand(context.Background(), command, func(context.Context, CommandTransaction, messaging.Command) (CommandDecision, error) {
		t.Fatal("replay invoked handler")
		return CommandDecision{}, nil
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) || result.Result != nil || result.Disposition != "" {
		t.Fatalf("rollback failure returned success: %+v err=%v", result, err)
	}
}
