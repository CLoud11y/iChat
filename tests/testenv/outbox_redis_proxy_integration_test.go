//go:build integration

package testenv

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Fixture-only RESP2 request/reply proxy, never used for subscriptions. Consume
// a real Redis integer PUBLISH reply before losing it; never log wire data.
type outboxRedisProxy struct {
	address, backend string
	listener         net.Listener
	mu               sync.Mutex
	closed           bool
	connections      []net.Conn
	wg               sync.WaitGroup
	once             sync.Once
	armed            atomic.Bool
	dropped          atomic.Int64
}

func startOutboxRedisProxy(t *testing.T, backend string) *outboxRedisProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &outboxRedisProxy{address: l.Addr().String(), backend: backend, listener: l}
	p.wg.Add(1)
	go p.serve()
	t.Cleanup(p.Close)
	return p
}
func (p *outboxRedisProxy) serve() {
	defer p.wg.Done()
	for {
		c, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			c.Close()
			return
		}
		p.connections = append(p.connections, c)
		p.wg.Add(1)
		p.mu.Unlock()
		go func() {
			defer p.wg.Done()
			defer c.Close()
			b, err := net.DialTimeout("tcp", p.backend, 2*time.Second)
			if err != nil {
				return
			}
			defer b.Close()
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				return
			}
			p.connections = append(p.connections, b)
			p.mu.Unlock()
			cr, br := bufio.NewReader(c), bufio.NewReader(b)
			for {
				packet, err := outboxRESP(cr, 0)
				if err != nil {
					return
				}
				if _, err := io.Copy(b, bytes.NewReader(packet)); err != nil {
					return
				}
				reply, err := outboxRESP(br, 0)
				if err != nil {
					return
				}
				if outboxRESPPublish(packet) && len(reply) > 2 && reply[0] == ':' && p.armed.CompareAndSwap(true, false) {
					p.dropped.Add(1)
					return
				}
				if _, err := io.Copy(c, bytes.NewReader(reply)); err != nil {
					return
				}
			}
		}()
	}
}
func outboxRESP(r *bufio.Reader, depth int) ([]byte, error) {
	if depth > 8 {
		return nil, io.ErrUnexpectedEOF
	}
	line, err := r.ReadSlice('\n')
	if err != nil || len(line) < 3 || line[len(line)-2] != '\r' {
		return nil, io.ErrUnexpectedEOF
	}
	packet := append([]byte(nil), line...)
	switch line[0] {
	case '+', '-', ':':
		return packet, nil
	case '$', '*':
		n, err := strconv.Atoi(string(line[1 : len(line)-2]))
		if err != nil || n < -1 || n > 1024*1024 {
			return nil, io.ErrUnexpectedEOF
		}
		if n == -1 {
			return packet, nil
		}
		if line[0] == '$' {
			body := make([]byte, n+2)
			if _, err := io.ReadFull(r, body); err != nil || !bytes.HasSuffix(body, []byte("\r\n")) {
				return nil, io.ErrUnexpectedEOF
			}
			return append(packet, body...), nil
		}
		if n > 128 {
			return nil, io.ErrUnexpectedEOF
		}
		for range n {
			part, err := outboxRESP(r, depth+1)
			if err != nil {
				return nil, err
			}
			packet = append(packet, part...)
			if len(packet) > 1024*1024 {
				return nil, io.ErrUnexpectedEOF
			}
		}
		return packet, nil
	default:
		return nil, io.ErrUnexpectedEOF
	}
}
func outboxRESPPublish(packet []byte) bool {
	r := bufio.NewReader(bytes.NewReader(packet))
	line, err := r.ReadString('\n')
	if err != nil || line != "*3\r\n" {
		return false
	}
	line, err = r.ReadString('\n')
	if err != nil || line != "$7\r\n" {
		return false
	}
	line, err = r.ReadString('\n')
	return err == nil && strings.EqualFold(line, "publish\r\n")
}
func (p *outboxRedisProxy) Close() {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.listener.Close()
		for _, c := range p.connections {
			c.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
}
