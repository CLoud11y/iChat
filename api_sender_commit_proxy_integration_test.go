//go:build integration

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type senderCommitOK struct {
	connection uint64
	packet     []byte
}

// Test-only, plaintext MySQL packet relay for the pinned driver without query
// attributes. Only the persistence pool uses it; API and Outbox stay direct.
// A real COMMIT OK is withheld at a barrier, then discarded and the socket
// closed. No driver result, SQL success packet or transaction is fabricated.
type senderCommitLossProxy struct {
	listener                              net.Listener
	upstream                              string
	mu                                    sync.Mutex
	closed                                bool
	connections                           map[net.Conn]bool
	wg                                    sync.WaitGroup
	once, discardOnce                     sync.Once
	stop                                  chan struct{}
	discard                               chan struct{}
	captured                              chan senderCommitOK
	armed, invalid                        atomic.Bool
	accepted, commits, rollbacks, dropped atomic.Uint64
}

func newSenderCommitLossProxy(t *testing.T, upstream string) *senderCommitLossProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &senderCommitLossProxy{listener: l, upstream: upstream, connections: make(map[net.Conn]bool),
		stop: make(chan struct{}), discard: make(chan struct{}), captured: make(chan senderCommitOK, 1)}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			client, err := l.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				client.Close()
				return
			}
			p.connections[client] = true
			index := p.accepted.Add(1)
			p.wg.Add(1)
			p.mu.Unlock()
			go p.bridge(client, index)
		}
	}()
	t.Cleanup(p.Close)
	return p
}

func senderSQLPacket(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	size := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	if size == 0 || size > 1<<20 {
		return nil, fmt.Errorf("invalid test SQL packet size")
	}
	packet := make([]byte, size+4)
	copy(packet, header[:])
	_, err := io.ReadFull(r, packet[4:])
	return packet, err
}

func (p *senderCommitLossProxy) bridge(client net.Conn, index uint64) {
	defer p.wg.Done()
	defer func() { client.Close(); p.mu.Lock(); delete(p.connections, client); p.mu.Unlock() }()
	backend, err := net.DialTimeout("tcp", p.upstream, 2*time.Second)
	if err != nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		backend.Close()
		return
	}
	p.connections[backend] = true
	p.mu.Unlock()
	defer func() { backend.Close(); p.mu.Lock(); delete(p.connections, backend); p.mu.Unlock() }()
	var waiting atomic.Bool
	upstreamDone := make(chan struct{})
	go func() {
		defer close(upstreamDone)
		defer backend.Close()
		defer client.Close()
		for {
			packet, err := senderSQLPacket(client)
			if err != nil {
				return
			}
			if packet[3] == 0 && len(packet) >= 5 && packet[4] == 0x03 {
				switch {
				case bytes.Equal(packet[5:], []byte("COMMIT")):
					p.commits.Add(1)
					if p.armed.CompareAndSwap(true, false) {
						waiting.Store(true)
					}
				case bytes.Equal(packet[5:], []byte("ROLLBACK")):
					p.rollbacks.Add(1)
				}
			}
			if err := senderConfirmWrite(backend, packet); err != nil {
				return
			}
		}
	}()
	for {
		packet, err := senderSQLPacket(backend)
		if err != nil {
			break
		}
		if waiting.Load() {
			// COMMIT has no result rows: seq=1, OK, zero affected rows and
			// last insert id, and SERVER_STATUS_IN_TRANS cleared. The status
			// check is in addition to independent committed SQL visibility.
			if len(packet) < 11 || packet[3] != 1 || packet[4] != 0 || packet[5] != 0 || packet[6] != 0 || binary.LittleEndian.Uint16(packet[7:9])&1 != 0 {
				p.invalid.Store(true)
				break
			}
			p.captured <- senderCommitOK{connection: index, packet: packet}
			select {
			case <-p.discard:
				p.dropped.Add(1)
			case <-p.stop:
			}
			break
		}
		if err := senderConfirmWrite(client, packet); err != nil {
			break
		}
	}
	client.Close()
	backend.Close()
	<-upstreamDone
}

func (p *senderCommitLossProxy) discardResponse() {
	p.discardOnce.Do(func() { close(p.discard) })
}

func (p *senderCommitLossProxy) Close() {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		close(p.stop)
		p.listener.Close()
		for conn := range p.connections {
			conn.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
}
