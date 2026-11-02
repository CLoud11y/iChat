//go:build integration

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type senderBrokerAck struct {
	connection uint64
	channel    uint16
	tag        uint64
	multiple   bool
}

// A bounded AMQP 0-9-1 fixture, following the existing testenv session proxy's
// frame layout. Client publishes and server confirms are observed separately.
// It drops one complete, real Basic.Ack; all other frame bytes are unchanged.
type senderConfirmProxy struct {
	listener                net.Listener
	upstream                string
	mu                      sync.Mutex
	stopped                 bool
	connections             map[net.Conn]bool
	wg                      sync.WaitGroup
	once                    sync.Once
	armed, didDrop, invalid atomic.Bool
	accepted, pubs, acks    atomic.Uint64
	dropped                 chan senderBrokerAck
}

func newSenderConfirmProxy(t *testing.T, upstream string) *senderConfirmProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &senderConfirmProxy{listener: l, upstream: upstream, connections: make(map[net.Conn]bool), dropped: make(chan senderBrokerAck, 1)}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			client, err := l.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			if p.stopped {
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

func senderConfirmWrite(dst net.Conn, body []byte) error {
	for len(body) > 0 {
		n, err := dst.Write(body)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		body = body[n:]
	}
	return nil
}

func (p *senderConfirmProxy) frame(src net.Conn) ([]byte, []byte, error) {
	header := make([]byte, 7)
	if _, err := io.ReadFull(src, header); err != nil {
		return nil, nil, err
	}
	size := binary.BigEndian.Uint32(header[3:])
	if size > 1<<20 {
		p.invalid.Store(true)
		return nil, nil, errors.New("invalid confirm fixture frame size")
	}
	payload := make([]byte, int(size)+1)
	if _, err := io.ReadFull(src, payload); err != nil {
		return nil, nil, err
	}
	if payload[len(payload)-1] != 0xce {
		p.invalid.Store(true)
		return nil, nil, errors.New("invalid confirm fixture frame end")
	}
	return header, payload, nil
}

func (p *senderConfirmProxy) bridge(client net.Conn, index uint64) {
	defer p.wg.Done()
	defer client.Close()
	defer func() { p.mu.Lock(); delete(p.connections, client); p.mu.Unlock() }()
	broker, err := net.DialTimeout("tcp", p.upstream, 3*time.Second)
	if err != nil {
		return
	}
	defer broker.Close()
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.connections[broker] = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.connections, broker); p.mu.Unlock() }()
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		defer client.Close()
		defer broker.Close()
		protocol := make([]byte, 8)
		if _, err := io.ReadFull(client, protocol); err != nil {
			return
		}
		if !bytes.Equal(protocol, []byte{'A', 'M', 'Q', 'P', 0, 0, 9, 1}) {
			p.invalid.Store(true)
			return
		}
		if senderConfirmWrite(broker, protocol) != nil {
			return
		}
		for {
			header, payload, err := p.frame(client)
			if err != nil {
				return
			}
			if header[0] == 1 && len(payload) >= 5 && binary.BigEndian.Uint16(payload) == 60 && binary.BigEndian.Uint16(payload[2:]) == 40 {
				p.pubs.Add(1) // Client -> broker Basic.Publish, not a consumer ACK.
			}
			if senderConfirmWrite(broker, append(header, payload...)) != nil {
				return
			}
		}
	}()
	for {
		header, payload, err := p.frame(broker)
		if err != nil {
			break
		}
		if header[0] == 1 && len(payload) == 14 && binary.BigEndian.Uint16(payload) == 60 && binary.BigEndian.Uint16(payload[2:]) == 80 {
			p.acks.Add(1) // Broker -> publisher Basic.Ack, including its frame end.
			if p.armed.Load() && p.didDrop.CompareAndSwap(false, true) {
				p.dropped <- senderBrokerAck{connection: index, channel: binary.BigEndian.Uint16(header[1:]),
					tag: binary.BigEndian.Uint64(payload[4:12]), multiple: payload[12]&1 != 0}
				continue
			}
		}
		if senderConfirmWrite(client, append(header, payload...)) != nil {
			break
		}
	}
	client.Close()
	broker.Close()
	<-joined
}

func (p *senderConfirmProxy) Close() {
	p.once.Do(func() {
		p.mu.Lock()
		p.stopped = true
		p.listener.Close()
		for conn := range p.connections {
			conn.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
}
