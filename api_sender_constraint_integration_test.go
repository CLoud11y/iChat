//go:build integration

package main

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync/atomic"

	mysql "github.com/go-sql-driver/mysql"
)

// Like testenv's retrySQLConn, this issues a real auxiliary statement inside
// the production transaction after its actual message INSERT succeeded. The
// existing seq>0 CHECK rejects the UPDATE; the real MySQL error is returned
// unchanged. Neither schema nor the original command/INSERT is rewritten.
type senderConstraintFault struct {
	enabled                                     atomic.Bool
	updates, faults, begins, commits, rollbacks atomic.Uint64
}

type senderConstraintConnector struct {
	base  driver.Connector
	fault *senderConstraintFault
}

func (c senderConstraintConnector) Driver() driver.Driver { return c.base.Driver() }
func (c senderConstraintConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return senderConstraintConn{Conn: conn, fault: c.fault}, nil
}

type senderConstraintConn struct {
	driver.Conn
	fault *senderConstraintFault
}

func (c senderConstraintConn) Ping(ctx context.Context) error {
	return c.Conn.(driver.Pinger).Ping(ctx)
}
func (c senderConstraintConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c senderConstraintConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	executor := c.Conn.(driver.ExecerContext)
	result, err := executor.ExecContext(ctx, query, args)
	if err != nil || !c.fault.enabled.Load() || !strings.HasPrefix(query, "INSERT INTO message\n") || len(args) != 11 {
		return result, err
	}
	c.fault.updates.Add(1)
	_, err = executor.ExecContext(ctx, "UPDATE message SET seq=0 WHERE id=?", []driver.NamedValue{{Ordinal: 1, Value: args[0].Value}})
	var server *mysql.MySQLError
	if errors.As(err, &server) && server.Number == 3819 {
		c.fault.faults.Add(1)
	}
	if err != nil {
		return nil, err
	}
	return result, nil // A missing real constraint is caught by the fixture's counts.
}

func (c senderConstraintConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	c.fault.begins.Add(1)
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return senderConstraintTx{Tx: tx, fault: c.fault}, nil
}

type senderConstraintTx struct {
	driver.Tx
	fault *senderConstraintFault
}

func (t senderConstraintTx) Commit() error   { t.fault.commits.Add(1); return t.Tx.Commit() }
func (t senderConstraintTx) Rollback() error { t.fault.rollbacks.Add(1); return t.Tx.Rollback() }
