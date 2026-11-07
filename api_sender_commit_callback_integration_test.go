//go:build integration

package main

import (
	"context"
	"database/sql/driver"
	"sync"
	"sync/atomic"
)

// This follows testenv's lifecycleCommitConnector: a real MySQL Commit must
// succeed before the callback is held. Cancellation is deliberately ignored
// only at this fixture barrier; the original driver result is never changed.
// It models a slow driver callback, not loss of the SQL wire response.
type senderCommitCallbackGate struct {
	entered                      chan context.Context
	release                      chan struct{}
	finished                     chan struct{}
	once                         sync.Once
	armed                        atomic.Bool
	commits, rollbacks, returned atomic.Uint64
}

func newSenderCommitCallbackGate() *senderCommitCallbackGate {
	return &senderCommitCallbackGate{entered: make(chan context.Context, 1), release: make(chan struct{}), finished: make(chan struct{})}
}

func (g *senderCommitCallbackGate) unblock() {
	g.once.Do(func() { close(g.release) })
}

type senderCommitCallbackConnector struct {
	base driver.Connector
	gate *senderCommitCallbackGate
}

func (c senderCommitCallbackConnector) Driver() driver.Driver { return c.base.Driver() }
func (c senderCommitCallbackConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return senderCommitCallbackConn{Conn: conn, gate: c.gate}, nil
}

type senderCommitCallbackConn struct {
	driver.Conn
	gate *senderCommitCallbackGate
}

func (c senderCommitCallbackConn) Ping(ctx context.Context) error {
	return c.Conn.(driver.Pinger).Ping(ctx)
}

func (c senderCommitCallbackConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return senderCommitCallbackTx{Tx: tx, ctx: ctx, gate: c.gate}, nil
}

type senderCommitCallbackTx struct {
	driver.Tx
	ctx  context.Context
	gate *senderCommitCallbackGate
}

func (t senderCommitCallbackTx) Commit() error {
	t.gate.commits.Add(1)
	err := t.Tx.Commit()
	if err == nil && t.gate.armed.CompareAndSwap(true, false) {
		t.gate.entered <- t.ctx
		<-t.gate.release
		t.gate.returned.Add(1)
		close(t.gate.finished)
	}
	return err
}

func (t senderCommitCallbackTx) Rollback() error {
	t.gate.rollbacks.Add(1)
	return t.Tx.Rollback()
}
