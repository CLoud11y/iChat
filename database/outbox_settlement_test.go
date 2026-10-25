package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type outboxSettlementTestState struct {
	outboxTestState
	lease OutboxLease
	row   []driver.Value
	query string
	args  []driver.NamedValue
}
type outboxSettlementTestConnector struct{ s *outboxSettlementTestState }
type outboxSettlementTestConn struct {
	*outboxTestConn
	s *outboxSettlementTestState
}

func (outboxSettlementTestConnector) Driver() driver.Driver { return nil }
func (c outboxSettlementTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &outboxSettlementTestConn{&outboxTestConn{&c.s.outboxTestState}, c.s}, nil
}
func (c *outboxSettlementTestConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	s := c.s
	if q == selectOutboxSettlementSQL {
		s.events = append(s.events, "lock")
		if s.stage == "read" {
			return nil, errors.New("secret SQL read")
		}
		return &statusTestRows{count: 6, values: s.row}, nil
	}
	if q == "SELECT DATE_FORMAT(UTC_TIMESTAMP(6),'%Y-%m-%d %H:%i:%s.%f')" {
		s.events = append(s.events, "clock")
		if s.stage == "clock" {
			return nil, errors.New("secret SQL clock")
		}
		clock := "2026-10-25 01:02:03.000000"
		if s.stage == "expired" {
			clock = s.lease.Until.UTC().Format(mysqlMicrosecondLayout)
		}
		return &statusTestRows{count: 1, values: []driver.Value{clock}}, nil
	}
	return nil, errors.New("unexpected settlement query")
}
func (c *outboxSettlementTestConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	s := c.s
	s.events = append(s.events, "update")
	s.query, s.args = q, args
	if q != completeOutboxSQL && q != retryOutboxSQL {
		return nil, errors.New("unexpected settlement update")
	}
	if s.stage == "update" || s.stage == "rollback" {
		return nil, errors.New("secret SQL update")
	}
	if s.stage == "zero" {
		return driver.RowsAffected(0), nil
	}
	return driver.RowsAffected(1), nil
}
func settlementTestLease() OutboxLease {
	return OutboxLease{EventID: strings.Repeat("a", 64), Token: "171c6c89-bfa6-4bd1-a738-cfa7d7a63164", Attempts: 3, Until: time.Date(2026, 10, 25, 1, 3, 0, 0, time.UTC)}
}

func TestOutboxSettlementTransaction(t *testing.T) {
	for _, retry := range []bool{false, true} {
		for _, stage := range []string{"success", "begin", "read", "clock", "missing", "stale", "expired", "attempt", "deadline", "completed", "update", "zero", "commit", "rollback"} {
			t.Run(stage+map[bool]string{false: "_complete", true: "_retry"}[retry], func(t *testing.T) {
				lease := settlementTestLease()
				s := &outboxSettlementTestState{outboxTestState: outboxTestState{stage: stage}, lease: lease, row: []driver.Value{lease.EventID, "leased", lease.Token, lease.Until.Format(mysqlMicrosecondLayout), int64(lease.Attempts), nil}}
				switch stage {
				case "missing":
					s.row = nil
				case "stale":
					s.row[2] = "a4e3044c-5f44-4d1b-9d61-a35ab19ec01d"
				case "attempt":
					s.row[4] = int64(2)
				case "deadline":
					s.row[3] = "2026-10-25 02:00:00.000000"
				case "completed":
					s.row[1], s.row[5] = "completed", "2026-10-25 00:00:00.000000"
				}
				pool := sql.OpenDB(outboxSettlementTestConnector{s})
				defer pool.Close()
				repo, _ := NewOutboxRepository(pool)
				var result OutboxSettlement
				var err error
				if retry {
					result, err = repo.Retry(context.Background(), lease, OutboxRetryOptions{Base: time.Second, Max: time.Minute}, OutboxRetryPublishUnknown)
				} else {
					result, err = repo.Complete(context.Background(), lease)
				}
				if stage == "success" {
					if err != nil || result.EventID != lease.EventID || !reflect.DeepEqual(s.events, []string{"begin", "lock", "clock", "update", "commit"}) {
						t.Fatal("settlement not committed", s.events, err)
					}
					if retry {
						if result.Status != "pending" || result.AvailableAt == nil || result.CompletedAt != nil || s.args[1].Value != string(OutboxRetryPublishUnknown) {
							t.Fatal("retry metadata lost")
						}
					} else {
						if result.Status != "completed" || result.CompletedAt == nil || result.AvailableAt != nil {
							t.Fatal("completion metadata lost")
						}
					}
					if !strings.Contains(s.query, "lease_until>UTC_TIMESTAMP(6)") || !strings.Contains(s.query, "attempts=?") {
						t.Fatal("deadline/attempt fence missing")
					}
				} else {
					if err == nil || result != (OutboxSettlement{}) || strings.Contains(err.Error(), "secret") {
						t.Fatal("failure returned success or leaked", err)
					}
					if stage == "commit" && !errors.Is(err, ErrOutboxCommitUnknown) {
						t.Fatal(err)
					}
					if stage == "rollback" && !errors.Is(err, ErrOutboxRollbackUnknown) {
						t.Fatal(err)
					}
					if stage == "missing" || stage == "stale" || stage == "expired" || stage == "attempt" || stage == "deadline" || stage == "completed" || stage == "zero" {
						if !errors.Is(err, ErrOutboxLeaseLost) {
							t.Fatal("lost owner not classified", err)
						}
					}
				}
			})
		}
	}
}

func TestOutboxRetryDelay(t *testing.T) {
	o := OutboxRetryOptions{Base: time.Millisecond, Max: 100 * time.Millisecond}
	for attempt, want := range map[uint32]time.Duration{1: time.Millisecond, 2: 2 * time.Millisecond, 7: 64 * time.Millisecond, 8: 100 * time.Millisecond, ^uint32(0): 100 * time.Millisecond} {
		cap, err := o.ceiling(attempt)
		if err != nil || cap != want {
			t.Fatal("exponential ceiling wrong", attempt, cap, err)
		}
		for i := 0; i < 30; i++ {
			delay, err := o.Delay(attempt)
			if err != nil || delay < cap/2 || delay > cap || delay%time.Microsecond != 0 {
				t.Fatal("jitter escaped bounds", delay, err)
			}
		}
	}
	for _, o := range []OutboxRetryOptions{{}, {Base: time.Nanosecond, Max: time.Second}, {Base: time.Second, Max: time.Millisecond}, {Base: time.Second, Max: 2 * time.Hour}} {
		if _, err := o.Delay(1); !errors.Is(err, ErrOutboxSettlementInput) {
			t.Fatal("invalid retry policy accepted")
		}
	}
	if _, err := o.Delay(0); !errors.Is(err, ErrOutboxSettlementInput) {
		t.Fatal("zero attempt accepted")
	}
}

func TestOutboxSettlementValidation(t *testing.T) {
	s := &outboxSettlementTestState{}
	pool := sql.OpenDB(outboxSettlementTestConnector{s})
	defer pool.Close()
	repo, _ := NewOutboxRepository(pool)
	for _, mutate := range []func(*OutboxLease){func(l *OutboxLease) { l.EventID = "bad" }, func(l *OutboxLease) { l.Token = "bad" }, func(l *OutboxLease) { l.Attempts = 0 }, func(l *OutboxLease) { l.Until = l.Until.Add(time.Nanosecond) }} {
		lease := settlementTestLease()
		mutate(&lease)
		if _, err := repo.Complete(context.Background(), lease); !errors.Is(err, ErrOutboxSettlementInput) {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.Complete(ctx, settlementTestLease()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := repo.Complete(nil, settlementTestLease()); !errors.Is(err, ErrOutboxSettlementInput) {
		t.Fatal(err)
	}
	if _, err := repo.Retry(context.Background(), settlementTestLease(), OutboxRetryOptions{Base: time.Second, Max: time.Minute}, OutboxRetryReason("private secret error")); !errors.Is(err, ErrOutboxSettlementInput) {
		t.Fatal("arbitrary diagnostics persisted")
	}
	if len(s.events) != 0 {
		t.Fatal("invalid request opened SQL")
	}
}
