package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"

	"iChat/messaging"
	"iChat/messaging/rabbitmq"
)

type quarantineResolveTestState struct {
	quarantineReplayTestState
	command, message []driver.Value
}
type quarantineResolveTestConnector struct{ state *quarantineResolveTestState }
type quarantineResolveTestConn struct {
	*quarantineReplayTestConn
	s *quarantineResolveTestState
}

func (quarantineResolveTestConnector) Driver() driver.Driver { return nil }
func (c quarantineResolveTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &quarantineResolveTestConn{&quarantineReplayTestConn{&c.state.quarantineReplayTestState}, c.state}, nil
}
func (c *quarantineResolveTestConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	s := c.s
	var stage string
	var row []driver.Value
	var count int
	switch query {
	case strings.TrimSuffix(readQuarantineAuditSQL, " FOR SHARE") + " FOR UPDATE":
		stage, row, count = "audit", s.row, 13
	case selectStatusCommandSQL + " FOR SHARE":
		stage, row, count = "command", s.command, 10
		if len(args) != 2 || args[0].Value != int64(12) {
			return nil, errors.New("unexpected terminal selection")
		}
	case selectStatusMessageSQL:
		stage, row, count = "message", s.message, 10
	default:
		return nil, errors.New("unexpected resolution query")
	}
	s.events = append(s.events, stage)
	if s.stage == stage {
		return nil, errors.New("secret SQL error")
	}
	return &statusTestRows{count: count, values: row}, nil
}
func (c *quarantineResolveTestConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	s := c.s
	s.events, s.args = append(s.events, "update"), args
	if query != updateQuarantineResolvedSQL || len(args) != 4 || args[1].Value != s.row[0] || args[2].Value != s.row[9] {
		return nil, errors.New("unexpected resolution update")
	}
	if s.stage == "update" {
		return nil, errors.New("secret SQL update")
	}
	if s.stage == "zero" {
		return driver.RowsAffected(0), nil
	}
	return driver.RowsAffected(1), nil
}

func TestQuarantineResolveRepository(t *testing.T) {
	for _, scenario := range []string{"persisted", "rejected", "pending_attempt", "already_resolved", "begin", "audit", "missing_audit", "bad_audit", "unidentified", "stale_attempt", "command", "pending_terminal", "hash_conflict", "identity_conflict", "claim", "message", "missing_message", "bad_message", "update", "zero", "commit", "rollback"} {
		t.Run(scenario, func(t *testing.T) {
			row := quarantineQueryTestRow(t, scenario != "unidentified")
			input, _ := rabbitmq.DecodeQuarantineAudit(row[6].([]byte))
			source, _ := input.Envelope().Source()
			wire, _ := messaging.DecodeEnvelope(source.Body)
			command, _ := messaging.ValidateEnvelope(wire)
			terminal := CommandPersisted
			if scenario == "rejected" || scenario == "claim" {
				terminal = CommandRejected
			}
			cr, mr := statusTestCommandRow(command, terminal), statusTestMessageRow(command)
			cr[0], mr[2], mr[3] = int64(12), int64(12), int64(34)
			s := &quarantineResolveTestState{quarantineReplayTestState: quarantineReplayTestState{row: row, stage: scenario}, command: cr, message: mr}
			selection := QuarantineResolveSelection{TransportID: row[0].(string)}
			previous := "171c6c89-bfa6-4bd1-a738-cfa7d7a63164"
			switch scenario {
			case "pending_attempt", "stale_attempt":
				row[9], row[10] = "replay_pending", previous
				if scenario == "pending_attempt" {
					selection.PreviousAttempt = previous
				}
			case "already_resolved", "rollback":
				row[9], row[12] = "resolved", "2026-10-22 05:06:07.000000"
			case "missing_audit":
				s.row = nil
			case "bad_audit":
				row[5] = []byte("corrupt")
			case "pending_terminal":
				s.command = nil
			case "hash_conflict":
				cr[3] = strings.Repeat("0", 64)
			case "identity_conflict":
				cr[2] = strings.Repeat("0", 64)
			case "claim":
				cr[5] = "__claim:uncommitted"
			case "missing_message":
				s.message = nil
			case "bad_message":
				mr[6] = "changed content"
			}
			pool := sql.OpenDB(quarantineResolveTestConnector{s})
			defer pool.Close()
			repo, _ := NewQuarantineRepository(pool)
			result, err := repo.Resolve(context.Background(), selection)
			success := scenario == "persisted" || scenario == "rejected" || scenario == "pending_attempt" || scenario == "already_resolved"
			if (err == nil) != success || (err != nil && strings.Contains(err.Error(), "secret")) {
				t.Fatal("wrong resolution or leaked error", s.events, err)
			}
			if success {
				if result.TerminalState != terminal || result.CommandID != command.CommandID || result.ResolvedAt == nil || result.ResolvedAt.Location().String() != "UTC" {
					t.Fatal("terminal proof or timestamp missing", result)
				}
				if scenario == "already_resolved" {
					if result.Outcome != "already_resolved" || result.ResolvedAt.Format(mysqlMicrosecondLayout) != row[12] || !reflect.DeepEqual(s.events, []string{"begin", "audit", "command", "message", "rollback"}) {
						t.Fatal("existing resolution overwritten", s.events)
					}
				} else {
					want := []string{"begin", "audit", "command", "message", "update", "commit"}
					if terminal == CommandRejected {
						want = []string{"begin", "audit", "command", "update", "commit"}
					}
					if result.Outcome != "resolved" || !reflect.DeepEqual(s.events, want) {
						t.Fatal("resolution did not follow terminal proof", s.events)
					}
				}
				if scenario == "pending_attempt" && (result.ReplayAttemptID == nil || *result.ReplayAttemptID != previous || s.args[3].Value != previous) {
					t.Fatal("recorded replay attempt changed")
				}
			} else {
				if result.TransportID != "" && (result.Outcome != "unconfirmed" || result.ResolvedAt != nil) {
					t.Fatal("failure claimed successful resolution", result)
				}
				if scenario == "commit" && !errors.Is(err, ErrQuarantineCommitUnknown) {
					t.Fatal(err)
				}
				if scenario == "rollback" && !errors.Is(err, ErrQuarantineRollbackUnknown) {
					t.Fatal(err)
				}
				if scenario == "pending_terminal" && !errors.Is(err, ErrQuarantineTerminalPending) {
					t.Fatal(err)
				}
				if (scenario == "hash_conflict" || scenario == "identity_conflict") && !errors.Is(err, ErrQuarantineTerminalConflict) {
					t.Fatal(err)
				}
				if (scenario == "stale_attempt" || scenario == "zero") && !errors.Is(err, ErrQuarantineResolveChanged) {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestQuarantineResolveValidation(t *testing.T) {
	s := &quarantineResolveTestState{}
	pool := sql.OpenDB(quarantineResolveTestConnector{s})
	defer pool.Close()
	repo, _ := NewQuarantineRepository(pool)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, callCtx := range []context.Context{nil, ctx, context.Background()} {
		if _, err := repo.Resolve(callCtx, QuarantineResolveSelection{TransportID: "invalid"}); err == nil {
			t.Fatal("invalid resolution accepted")
		}
	}
	if len(s.events) != 0 {
		t.Fatal("invalid/canceled request opened a transaction")
	}
}
