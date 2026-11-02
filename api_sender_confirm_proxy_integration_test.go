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

type senderObservedDelivery struct {
	connection  uint64
	channel     uint16
	tag         uint64
	redelivered bool
	body        []byte
}

type senderDeliveryCapture struct {
	value  senderObservedDelivery
	size   uint64
	header bool
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
	consumerMode            bool
	consumerAcks, delivered atomic.Uint64
	deliveries              chan senderObservedDelivery
	forwardedConsumerAcks   chan senderBrokerAck
}

func newSenderConfirmProxy(t *testing.T, upstream string) *senderConfirmProxy {
	t.Helper()
	return newSenderAMQPProxy(t, upstream, false)
}

func newSenderConsumerAckProxy(t *testing.T, upstream string) *senderConfirmProxy {
	t.Helper()
	return newSenderAMQPProxy(t, upstream, true)
}

func newSenderAMQPProxy(t *testing.T, upstream string, consumer bool) *senderConfirmProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &senderConfirmProxy{listener: l, upstream: upstream, connections: make(map[net.Conn]bool), dropped: make(chan senderBrokerAck, 1),
		consumerMode: consumer, deliveries: make(chan senderObservedDelivery, 16), forwardedConsumerAcks: make(chan senderBrokerAck, 16)}
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
			if p.consumerMode && header[0] == 1 && len(payload) == 14 && binary.BigEndian.Uint16(payload) == 60 && binary.BigEndian.Uint16(payload[2:]) == 80 {
				p.consumerAcks.Add(1)
				if p.armed.Load() && p.didDrop.CompareAndSwap(false, true) {
					p.dropped <- senderBrokerAck{connection: index, channel: binary.BigEndian.Uint16(header[1:]),
						tag: binary.BigEndian.Uint64(payload[4:12]), multiple: payload[12]&1 != 0}
					continue // Consumer -> broker ACK is lost after the real SQL commit.
				}
			}
			if senderConfirmWrite(broker, append(header, payload...)) != nil {
				return
			}
			if p.consumerMode && header[0] == 1 && len(payload) == 14 && binary.BigEndian.Uint16(payload) == 60 && binary.BigEndian.Uint16(payload[2:]) == 80 {
				select {
				case p.forwardedConsumerAcks <- senderBrokerAck{connection: index, channel: binary.BigEndian.Uint16(header[1:]),
					tag: binary.BigEndian.Uint64(payload[4:12]), multiple: payload[12]&1 != 0}:
				default:
					p.invalid.Store(true)
					return
				}
			}
		}
	}()
	pending := make(map[uint16]*senderDeliveryCapture)
	for {
		header, payload, err := p.frame(broker)
		if err != nil {
			break
		}
		if !p.consumerMode && header[0] == 1 && len(payload) == 14 && binary.BigEndian.Uint16(payload) == 60 && binary.BigEndian.Uint16(payload[2:]) == 80 {
			p.acks.Add(1) // Broker -> publisher Basic.Ack, including its frame end.
			if p.armed.Load() && p.didDrop.CompareAndSwap(false, true) {
				p.dropped <- senderBrokerAck{connection: index, channel: binary.BigEndian.Uint16(header[1:]),
					tag: binary.BigEndian.Uint64(payload[4:12]), multiple: payload[12]&1 != 0}
				continue
			}
		}
		if p.consumerMode && !p.observeDelivery(header, payload, index, pending) {
			break
		}
		if senderConfirmWrite(client, append(header, payload...)) != nil {
			break
		}
	}
	client.Close()
	broker.Close()
	<-joined
}

func (p *senderConfirmProxy) observeDelivery(header, payload []byte, index uint64, pending map[uint16]*senderDeliveryCapture) bool {
	channel := binary.BigEndian.Uint16(header[1:])
	data := payload[:len(payload)-1]
	if header[0] == 1 && len(data) >= 5 && binary.BigEndian.Uint16(data) == 60 && binary.BigEndian.Uint16(data[2:]) == 60 {
		offset := 5 + int(data[4]) // Basic.Deliver: short consumer tag, uint64 tag, redelivered bit.
		if len(data) < offset+9 || pending[channel] != nil {
			p.invalid.Store(true)
			return false
		}
		pending[channel] = &senderDeliveryCapture{value: senderObservedDelivery{connection: index, channel: channel,
			tag: binary.BigEndian.Uint64(data[offset:]), redelivered: data[offset+8]&1 != 0}}
	}
	capture := pending[channel]
	if header[0] == 2 {
		if capture == nil || capture.header || len(data) < 14 || binary.BigEndian.Uint16(data) != 60 {
			p.invalid.Store(true)
			return false
		}
		capture.size, capture.header = binary.BigEndian.Uint64(data[4:12]), true
		if capture.size > 1<<20 {
			p.invalid.Store(true)
			return false
		}
	} else if header[0] == 3 {
		if capture == nil || !capture.header || uint64(len(capture.value.body)+len(data)) > capture.size {
			p.invalid.Store(true)
			return false
		}
		capture.value.body = append(capture.value.body, data...)
	}
	if capture != nil && capture.header && uint64(len(capture.value.body)) == capture.size {
		p.delivered.Add(1)
		select {
		case p.deliveries <- capture.value:
			delete(pending, channel)
		default:
			p.invalid.Store(true)
			return false
		}
	}
	return true
}

func (p *senderConfirmProxy) disconnect() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for conn := range p.connections {
		conn.Close()
	}
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
