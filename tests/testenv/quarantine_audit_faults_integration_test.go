//go:build integration

package testenv

import (
	"context"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	mysql "github.com/go-sql-driver/mysql"
)

// Only audit INSERT transactions are intercepted. All SQL is sent to the real
// fixture MySQL. The pool remains borrowed until every callback actually joins.
type quarantineAuditFaultConnector struct {
	mu      sync.Mutex
	base    driver.Connector
	stage   string
	armed   atomic.Bool
	entered chan context.Context
	release <-chan struct{}
}

func (c *quarantineAuditFaultConnector) Driver() driver.Driver { return &mysql.MySQLDriver{} }
func (c *quarantineAuditFaultConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c.mu.Lock()
	base := c.base
	c.mu.Unlock()
	conn, err := base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &quarantineAuditFaultConn{Conn: conn, fault: c}, nil
}

type quarantineAuditFaultConn struct {
	driver.Conn
	fault *quarantineAuditFaultConnector
	audit bool
}

func (c *quarantineAuditFaultConn) Ping(ctx context.Context) error {
	return c.Conn.(driver.Pinger).Ping(ctx)
}
func (c *quarantineAuditFaultConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	c.audit = false
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return &quarantineAuditFaultTx{Tx: tx, conn: c, ctx: ctx}, nil
}
func (c *quarantineAuditFaultConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
	if err == nil && strings.HasPrefix(q, "INSERT INTO message_quarantine ") {
		c.audit = true
	}
	return result, err
}
func (c *quarantineAuditFaultConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}

type quarantineAuditFaultTx struct {
	driver.Tx
	conn *quarantineAuditFaultConn
	ctx  context.Context
}

func (tx *quarantineAuditFaultTx) Commit() error {
	f := tx.conn.fault
	fault := tx.conn.audit && f.armed.CompareAndSwap(true, false)
	if fault && f.stage == "before_commit" {
		f.entered <- tx.ctx
		<-f.release
	}
	err := tx.Tx.Commit()
	if fault && err == nil && (f.stage == "after_commit" || f.stage == "commit_error") {
		f.entered <- tx.ctx
		if f.stage == "commit_error" {
			return io.EOF
		}
		<-f.release
	}
	return err
}
