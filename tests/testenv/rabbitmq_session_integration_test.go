//go:build integration

package testenv

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/messaging"
	"iChat/messaging/rabbitmq"
)

func TestRabbitMQSessionIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	env, conf := startRabbitFixture(t, ctx)
	m, err := rabbitmq.NewManagement("http://"+conf.ManagementAddress, rabbitAdminUser, env.password)
	if err != nil {
		t.Fatal(err)
	}
	setup := openPublisherConnection(t, ctx, conf)
	ch, err := setup.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := rabbitmq.EnsureLocal(ctx, ch, m, conf.VHost); err != nil {
		t.Fatal(err)
	}
	c, err := messaging.Normalize(12, messaging.SendRequest{ClientMessageID: "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", TargetType: messaging.Private, TargetID: "34", Media: messaging.Text, Content: "session:" + env.RunID() + ":中🙂\x00"})
	if err != nil {
		t.Fatal(err)
	}
	options := rabbitmq.DefaultSessionOptions()
	options.Publisher.Slots = 2
	options.OperationTimeout = 2 * time.Second
	options.WriteTimeout = 500 * time.Millisecond
	options.CloseTimeout = 300 * time.Millisecond
	dial := func(t *testing.T, address string, o rabbitmq.SessionOptions) *rabbitmq.PublisherSession {
		t.Helper()
		s, err := rabbitmq.DialPublisher(ctx, address, o)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close(context.Background()) })
		return s
	}
	read := func(t *testing.T, r rabbitmq.PublishResult) {
		t.Helper()
		d := getTopologyDelivery(t, ctx, ch, rabbitmq.PersistQueue)
		e, err := messaging.DecodeEnvelope(d.Body)
		if err != nil || e.CommandID != c.CommandID || e.Content != c.Content || e.PayloadHash != c.PayloadHash || e.PublishAttemptID != r.PublishAttemptID || d.MessageId != r.CommandID || d.CorrelationId != r.PublishAttemptID {
			t.Fatal("confirmed/unknown attempt body or identity changed", err)
		}
		if err := d.Ack(false); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("healthy_session_and_graceful_close", func(t *testing.T) {
		s := dial(t, conf.AMQPURL(), options)
		for i := 0; i < 3; i++ {
			r, err := s.Publish(ctx, c, time.Now())
			if err != nil || r.Outcome != rabbitmq.PublishAccepted {
				t.Fatal("healthy managed publication failed", r, err)
			}
			read(t, r)
		}
		if err := s.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if r, err := s.Publish(ctx, c, time.Now()); !errors.Is(err, rabbitmq.ErrPublisherUnavailable) || r.Outcome != rabbitmq.PublishNotStarted {
			t.Fatal("closed session admitted work", r, err)
		}
	})
	t.Run("confirm_blackhole_cancel_marks_all_pending_unknown", func(t *testing.T) {
		proxy := startSessionProxy(t, conf.Address)
		proxied := conf
		proxied.Address = proxy.address
		s := dial(t, proxied.AMQPURL(), options)
		proxy.mode.Store(1)
		first, firstCancel := context.WithCancel(ctx)
		defer firstCancel()
		results := make(chan rabbitmq.PublishResult, 2)
		failures := make(chan error, 2)
		for _, requestCtx := range []context.Context{first, ctx} {
			go func(requestCtx context.Context) {
				r, err := s.Publish(requestCtx, c, time.Now())
				results <- r
				failures <- err
			}(requestCtx)
		}
		// Observe two actual broker confirms before canceling. The proxy drops
		// all downstream frames, including heartbeats, after session setup.
		for i := 0; i < 2; i++ {
			select {
			case <-proxy.dropped:
			case <-time.After(time.Second):
				t.Fatal("two actual broker ACKs were not intercepted")
			}
		}
		if r, err := s.Publish(ctx, c, time.Now()); !errors.Is(err, rabbitmq.ErrPublisherBusy) || r.Outcome != rabbitmq.PublishNotStarted {
			t.Fatal("full managed pool wrote", r, err)
		}
		started := time.Now()
		firstCancel()
		expected := map[string]rabbitmq.PublishResult{}
		for i := 0; i < 2; i++ {
			select {
			case r := <-results:
				if err := <-failures; err == nil || r.Outcome != rabbitmq.PublishUnknown {
					t.Fatal("blackholed confirm became accepted/definite", r, err)
				}
				expected[r.PublishAttemptID] = r
			case <-time.After(time.Second):
				t.Fatal("connection-wide abort left a request blocked")
			}
		}
		if time.Since(started) > time.Second {
			t.Fatal("cancellation was not bounded")
		}
		for i := 0; i < 2; i++ {
			d := getTopologyDelivery(t, ctx, ch, rabbitmq.PersistQueue)
			r, ok := expected[d.CorrelationId]
			if !ok {
				t.Fatal("unexpected unknown attempt in MQ")
			}
			delete(expected, d.CorrelationId)
			e, err := messaging.DecodeEnvelope(d.Body)
			if err != nil || e.PublishAttemptID != r.PublishAttemptID || e.CommandID != c.CommandID || e.PayloadHash != c.PayloadHash || e.Content != c.Content {
				t.Fatal("unknown attempt was lost or changed", err)
			}
			if err := d.Ack(false); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Close(ctx); err != nil {
			t.Fatal("aborted connection teardown failed", err)
		}
		proxy.Close()
	})
	t.Run("confirm_blackhole_operation_deadline", func(t *testing.T) {
		proxy := startSessionProxy(t, conf.Address)
		proxied := conf
		proxied.Address = proxy.address
		o := options
		o.Publisher.Slots = 1
		o.OperationTimeout = 250 * time.Millisecond
		s := dial(t, proxied.AMQPURL(), o)
		proxy.mode.Store(1)
		started := time.Now()
		r, err := s.Publish(ctx, c, time.Now())
		if err == nil || r.Outcome != rabbitmq.PublishUnknown {
			t.Fatal("confirm timeout became accepted/definite", r, err)
		}
		if time.Since(started) > time.Second {
			t.Fatal("operation timeout did not interrupt cleanup")
		}
		select {
		case <-proxy.dropped:
		default:
			t.Fatal("broker never produced the dropped ACK")
		}
		read(t, r)
		if err := s.Close(ctx); err != nil {
			t.Fatal(err)
		}
		proxy.Close()
	})
	t.Run("return_with_channel_close_response_blackholed", func(t *testing.T) {
		proxy := startSessionProxy(t, conf.Address)
		proxied := conf
		proxied.Address = proxy.address
		o := options
		o.Publisher.Slots = 1
		s := dial(t, proxied.AMQPURL(), o)
		proxy.mode.Store(2)
		if err := ch.QueueUnbind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, nil); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		r, err := s.Publish(ctx, c, time.Now())
		if !errors.Is(err, rabbitmq.ErrPublisherReturn) || r.Outcome != rabbitmq.PublishUnroutable {
			t.Fatal("known Return lost its classification", r, err)
		}
		if time.Since(started) > time.Second {
			t.Fatal("retirement Close RPC waited forever")
		}
		select {
		case <-proxy.dropped:
		default:
			t.Fatal("channel close response was not blackholed")
		}
		if err := ch.QueueBind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, false, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(ctx); err != nil {
			t.Fatal(err)
		}
		proxy.Close()
	})
	t.Run("connection_close_response_blackholed", func(t *testing.T) {
		proxy := startSessionProxy(t, conf.Address)
		proxied := conf
		proxied.Address = proxy.address
		s := dial(t, proxied.AMQPURL(), options)
		proxy.mode.Store(3)
		started := time.Now()
		if err := s.Close(ctx); err == nil {
			t.Fatal("silent close reported graceful completion")
		}
		if time.Since(started) > time.Second {
			t.Fatal("session close exceeded its bound")
		}
		select {
		case <-proxy.dropped:
		default:
			t.Fatal("connection close response was not blackholed")
		}
		proxy.Close()
	})
	if err := rabbitmq.VerifyLocal(ctx, m, conf.VHost); err != nil {
		t.Fatal(err)
	}
}

// One disposable TCP proxy per case. AMQP frames only flow between this run's
// broker and the owned publisher session; neither development traffic nor
// credentials are logged. Shutdown joins the acceptor and both copying tasks.
type sessionProxy struct {
	address             string
	listener            net.Listener
	upstreamAddress     string
	mu                  sync.Mutex
	stopped             bool
	connections         []net.Conn
	wg                  sync.WaitGroup
	mode                atomic.Uint32
	onlyFirst           atomic.Bool
	accepted            atomic.Uint64
	framesDropped       atomic.Uint64
	dropConsumerAcks    atomic.Bool // Enable before dialing the ACK-loss test connection.
	consumerAcksDropped atomic.Uint64
	dropped             chan struct{}
	closeOnce           sync.Once
}

func startSessionProxy(t *testing.T, upstream string) *sessionProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &sessionProxy{address: listener.Addr().String(), listener: listener, upstreamAddress: upstream, dropped: make(chan struct{}, 8)}
	p.wg.Add(1)
	go p.serve()
	t.Cleanup(p.Close)
	return p
}

func (p *sessionProxy) serve() {
	defer p.wg.Done()
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		index := p.accepted.Add(1)
		broker, err := net.DialTimeout("tcp", p.upstreamAddress, 3*time.Second)
		if err != nil {
			client.Close()
			continue
		}
		p.mu.Lock()
		if p.stopped {
			p.mu.Unlock()
			client.Close()
			broker.Close()
			return
		}
		p.connections = append(p.connections, client, broker)
		p.wg.Add(2)
		p.mu.Unlock()
		go func() {
			defer p.wg.Done()
			defer client.Close()
			defer broker.Close()
			if p.dropConsumerAcks.Load() {
				p.upstreamWithoutConsumerAcks(client, broker)
			} else {
				io.Copy(broker, client)
			}
		}()
		go func() {
			defer p.wg.Done()
			defer client.Close()
			defer broker.Close()
			p.downstream(client, broker, index)
		}()
	}
}

func (p *sessionProxy) downstream(client, broker net.Conn, index uint64) {
	for {
		header := make([]byte, 7)
		if _, err := io.ReadFull(broker, header); err != nil {
			return
		}
		size := binary.BigEndian.Uint32(header[3:])
		if size > 1<<20 {
			return
		}
		payload := make([]byte, int(size)+1)
		if _, err := io.ReadFull(broker, payload); err != nil {
			return
		}
		mode := p.mode.Load()
		if p.onlyFirst.Load() && index != 1 {
			mode = 0
		}
		drop := mode == 1
		signal := false
		if header[0] == 1 && size >= 4 {
			class, method := binary.BigEndian.Uint16(payload), binary.BigEndian.Uint16(payload[2:])
			if mode == 1 && class == 60 && method == 80 {
				signal = true
			}
			if mode == 2 && class == 20 && method == 41 {
				drop = true
				signal = true
			}
			if mode == 3 && class == 10 && method == 51 {
				drop = true
				signal = true
			}
		}
		if signal {
			select {
			case p.dropped <- struct{}{}:
			default:
			}
		}
		if drop {
			p.framesDropped.Add(1)
			continue
		}
		frame := append(header, payload...)
		for len(frame) > 0 {
			n, err := client.Write(frame)
			if err != nil {
				return
			}
			frame = frame[n:]
		}
	}
}

func (p *sessionProxy) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.stopped = true
		p.listener.Close()
		for _, conn := range p.connections {
			conn.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
}
