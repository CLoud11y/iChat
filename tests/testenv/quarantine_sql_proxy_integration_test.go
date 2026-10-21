//go:build integration

package testenv

import (
	"bytes"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Dedicated fixture-only MySQL classic-protocol proxy. TLS/compression/query
// attributes are not negotiated by the fixed fixture driver. It never logs
// packets: handshake/authentication and query payloads may contain credentials.
type quarantineSQLProxy struct {
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

func startQuarantineSQLProxy(t *testing.T, backend string) *quarantineSQLProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &quarantineSQLProxy{address: listener.Addr().String(), backend: backend, listener: listener}
	p.wg.Add(1)
	go p.serve()
	t.Cleanup(p.Close)
	return p
}

func (p *quarantineSQLProxy) serve() {
	defer p.wg.Done()
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			client.Close()
			return
		}
		p.connections = append(p.connections, client)
		p.wg.Add(1)
		p.mu.Unlock()
		go func() {
			defer p.wg.Done()
			defer client.Close()
			backend, err := net.DialTimeout("tcp", p.backend, 2*time.Second)
			if err != nil {
				return
			}
			defer backend.Close()
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				return
			}
			p.connections = append(p.connections, backend)
			p.mu.Unlock()
			var waiting atomic.Bool
			upstream := make(chan struct{})
			go func() {
				defer close(upstream)
				defer backend.Close()
				defer client.Close()
				for {
					packet, err := quarantineSQLPacket(client)
					if err != nil {
						return
					}
					// COM_QUERY=0x03, sequence=0; the fixed driver sends COMMIT
					// with no query attributes. Only one armed response is lost.
					if packet[3] == 0 && len(packet) == 11 && packet[4] == 0x03 && bytes.Equal(packet[5:], []byte("COMMIT")) && p.armed.CompareAndSwap(true, false) {
						waiting.Store(true)
					}
					if _, err := io.Copy(backend, bytes.NewReader(packet)); err != nil {
						return
					}
				}
			}()
			for {
				packet, err := quarantineSQLPacket(backend)
				if err != nil {
					break
				}
				if waiting.Load() && packet[3] == 1 && len(packet) >= 11 && packet[4] == 0x00 {
					// The complete real server OK is consumed but never reaches
					// the client. Closing produces a genuine unknown SQL outcome.
					p.dropped.Add(1)
					break
				}
				if _, err := io.Copy(client, bytes.NewReader(packet)); err != nil {
					break
				}
			}
			_ = client.Close()
			_ = backend.Close()
			<-upstream
		}()
	}
}

func quarantineSQLPacket(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	size := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	packet := make([]byte, size+4)
	copy(packet, header[:])
	_, err := io.ReadFull(r, packet[4:])
	return packet, err
}

func (p *quarantineSQLProxy) Close() {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		_ = p.listener.Close()
		for _, conn := range p.connections {
			_ = conn.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
}
