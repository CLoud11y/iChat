package database

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Exercise transaction boundaries without a server, including commit failures
// that cannot be deterministically produced by closing a real connection.
type groupTransactionPool struct {
	gorm.ConnPool
	stage   string
	cause   error
	events  []string
	inside  bool
	writes  int
	queryDB *sql.DB
}

type groupTransaction struct {
	gorm.ConnPool
	pool *groupTransactionPool
}

type groupWriteResult struct{}

func (groupWriteResult) LastInsertId() (int64, error) { return 1, nil }
func (groupWriteResult) RowsAffected() (int64, error) { return 1, nil }

func (p *groupTransactionPool) BeginTx(context.Context, *sql.TxOptions) (gorm.ConnPool, error) {
	p.events = append(p.events, "begin")
	if p.stage == "begin" {
		return nil, p.cause
	}
	p.inside = true
	return &groupTransaction{pool: p}, nil
}
func (p *groupTransactionPool) ExecContext(context.Context, string, ...interface{}) (sql.Result, error) {
	if !p.inside {
		return nil, errors.New("write outside transaction")
	}
	p.writes++
	p.events = append(p.events, "write")
	if p.writes == 2 {
		if p.stage == "write" {
			return nil, p.cause
		}
		if p.stage == "panic" {
			panic(p.cause)
		}
	}
	return groupWriteResult{}, nil
}
func (p *groupTransactionPool) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	if !p.inside {
		return nil, errors.New("lock outside transaction")
	}
	p.events = append(p.events, "lock_user")
	if p.stage == "lock" {
		return nil, p.cause
	}
	return p.queryDB.QueryContext(ctx, query, args...)
}
func (tx *groupTransaction) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return tx.pool.QueryContext(ctx, query, args...)
}
func (tx *groupTransaction) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return tx.pool.ExecContext(ctx, query, args...)
}
func (tx *groupTransaction) Commit() error {
	p := tx.pool
	p.events = append(p.events, "commit")
	if p.stage == "commit" {
		return p.cause
	}
	return nil
}
func (tx *groupTransaction) Rollback() error {
	p := tx.pool
	p.events = append(p.events, "rollback")
	return nil
}

func TestCreateGroupTransactionFailures(t *testing.T) {
	for _, tc := range []struct {
		stage  string
		events []string
	}{
		{"success", []string{"begin", "lock_user", "write", "write", "commit"}},
		{"begin", []string{"begin"}},
		{"lock", []string{"begin", "lock_user", "rollback"}},
		{"write", []string{"begin", "lock_user", "write", "write", "rollback"}},
		{"commit", []string{"begin", "lock_user", "write", "write", "commit", "rollback"}},
		{"panic", []string{"begin", "lock_user", "write", "write", "rollback"}},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			pool := &groupTransactionPool{stage: tc.stage, cause: errors.New("injected transaction failure")}
			pool.queryDB = permissionTestDB(t, &permissionTestState{active: map[string]bool{"user:1": true}})
			db, err := gorm.Open(mysql.New(mysql.Config{Conn: pool, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatal(err)
			}
			var recovered interface{}
			func() {
				defer func() { recovered = recover() }()
				err = NewGroupManager(db).CreateGroup(context.Background(), "group", 1, "desc")
			}()
			if tc.stage == "panic" {
				if recovered != pool.cause {
					t.Fatalf("panic was swallowed: %v", recovered)
				}
			} else if recovered != nil {
				t.Fatalf("unexpected panic: %v", recovered)
			} else if tc.stage == "success" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, pool.cause) || !errors.Is(err, ErrStorageUnavailable) {
				t.Fatalf("transaction error=%v", err)
			}
			if !reflect.DeepEqual(pool.events, tc.events) {
				t.Fatalf("events=%v want=%v", pool.events, tc.events)
			}
		})
	}
}
