package rabbitmq

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
)

const unitVHost = "ichat_test_0123456789abcdef"

type topologyPeer struct {
	responses map[string]any
	writes    []string
	err       error
}

func validPeer() *topologyPeer {
	p := &topologyPeer{responses: map[string]any{
		"/api/overview":                       map[string]any{"rabbitmq_version": BrokerVersion},
		"/api/feature-flags":                  []any{map[string]string{"name": "quorum_queue", "state": "enabled"}, map[string]string{"name": "stream_queue", "state": "enabled"}},
		"/api/vhosts/" + unitVHost:            map[string]string{"name": unitVHost},
		"/api/policies/" + unitVHost:          []any{},
		"/api/operator-policies/" + unitVHost: []any{},
	}}
	for _, q := range localQueues() {
		p.responses["/api/exchanges/"+unitVHost+"/"+q.exchange] = map[string]any{"name": q.exchange, "vhost": unitVHost, "type": "direct", "durable": true, "auto_delete": false, "internal": false, "arguments": map[string]any{}}
		p.responses["/api/queues/"+unitVHost+"/"+q.name] = map[string]any{"name": q.name, "vhost": unitVHost, "type": "quorum", "durable": true, "auto_delete": false, "exclusive": false, "arguments": q.arguments, "state": "running", "members": []string{"rabbit@local"}, "online": []string{"rabbit@local"}, "effective_policy_definition": map[string]any{}}
		p.responses["/api/exchanges/"+unitVHost+"/"+q.exchange+"/bindings/source"] = []any{map[string]any{"source": q.exchange, "vhost": unitVHost, "destination": q.name, "destination_type": "queue", "routing_key": q.key, "arguments": map[string]any{}}}
	}
	return p
}

func (p *topologyPeer) management(t *testing.T) *Management {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "operator" || password != "admin-secret" || r.Method != http.MethodGet {
			t.Error("invalid operator request")
			w.WriteHeader(403)
			return
		}
		value, ok := p.responses[r.URL.EscapedPath()]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	m, err := NewManagement(server.URL, "operator", "admin-secret")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (p *topologyPeer) ExchangeDeclare(name, kind string, durable, autoDelete, internal, noWait bool, args amqp.Table) error {
	p.writes = append(p.writes, "exchange:"+name)
	if kind != "direct" || !durable || autoDelete || internal || noWait || len(args) != 0 {
		return errors.New("invalid exchange declaration")
	}
	if p.err != nil {
		return p.err
	}
	p.responses["/api/exchanges/"+unitVHost+"/"+name] = validPeer().responses["/api/exchanges/"+unitVHost+"/"+name]
	return nil
}
func (p *topologyPeer) QueueDeclare(name string, durable, autoDelete, exclusive, noWait bool, args amqp.Table) (amqp.Queue, error) {
	p.writes = append(p.writes, "queue:"+name)
	if !durable || autoDelete || exclusive || noWait {
		return amqp.Queue{}, errors.New("invalid queue declaration")
	}
	expected := validPeer().responses["/api/queues/"+unitVHost+"/"+name].(map[string]any)
	if !sameJSON(args, expected["arguments"]) {
		return amqp.Queue{}, errors.New("invalid queue arguments")
	}
	p.responses["/api/queues/"+unitVHost+"/"+name] = expected
	return amqp.Queue{Name: name}, nil
}
func (p *topologyPeer) QueueBind(name, key, exchange string, noWait bool, args amqp.Table) error {
	p.writes = append(p.writes, "binding:"+name)
	if noWait || len(args) != 0 {
		return errors.New("invalid binding")
	}
	for _, q := range localQueues() {
		if q.name == name && q.exchange == exchange && q.key == key {
			path := "/api/exchanges/" + unitVHost + "/" + exchange + "/bindings/source"
			p.responses[path] = validPeer().responses[path]
			return nil
		}
	}
	return errors.New("unknown binding")
}

func TestLocalTopologyRefusesDriftBeforeWrites(t *testing.T) {
	queue := "/api/queues/" + unitVHost + "/" + PersistQueue
	exchange := "/api/exchanges/" + unitVHost + "/" + CommandsExchange
	cases := []struct {
		name   string
		mutate func(*topologyPeer)
	}{
		{"broker_version", func(p *topologyPeer) { p.responses["/api/overview"] = map[string]string{"rabbitmq_version": "4.3.5"} }},
		{"missing_flag", func(p *topologyPeer) { p.responses["/api/feature-flags"] = []any{} }},
		{"disabled_stream_flag", func(p *topologyPeer) {
			p.responses["/api/feature-flags"].([]any)[1].(map[string]string)["state"] = "disabled"
		}},
		{"classic_queue", func(p *topologyPeer) { p.responses[queue].(map[string]any)["type"] = "classic" }},
		{"non_durable", func(p *topologyPeer) { p.responses[queue].(map[string]any)["durable"] = false }},
		{"auto_delete", func(p *topologyPeer) { p.responses[queue].(map[string]any)["auto_delete"] = true }},
		{"exclusive", func(p *topologyPeer) { p.responses[queue].(map[string]any)["exclusive"] = true }},
		{"three_members", func(p *topologyPeer) { p.responses[queue].(map[string]any)["members"] = []string{"a", "b", "c"} }},
		{"offline_member", func(p *topologyPeer) { p.responses[queue].(map[string]any)["online"] = []string{} }},
		{"wrong_online_member", func(p *topologyPeer) { p.responses[queue].(map[string]any)["online"] = []string{"foreign"} }},
		{"not_running", func(p *topologyPeer) { p.responses[queue].(map[string]any)["state"] = "down" }},
		{"ttl", func(p *topologyPeer) {
			p.responses[queue].(map[string]any)["arguments"].(amqp.Table)["x-message-ttl"] = 1
		}},
		{"drop_head", func(p *topologyPeer) {
			p.responses[queue].(map[string]any)["arguments"].(amqp.Table)["x-overflow"] = "drop-head"
		}},
		{"unreliable_dlx", func(p *topologyPeer) {
			p.responses[queue].(map[string]any)["arguments"].(amqp.Table)["x-dead-letter-strategy"] = "at-most-once"
		}},
		{"wrong_capacity", func(p *topologyPeer) {
			p.responses[queue].(map[string]any)["arguments"].(amqp.Table)["x-max-length-bytes"] = 1
		}},
		{"missing_delivery_limit", func(p *topologyPeer) {
			delete(p.responses[queue].(map[string]any)["arguments"].(amqp.Table), "x-delivery-limit")
		}},
		{"quarantine_delivery_limit", func(p *topologyPeer) {
			p.responses["/api/queues/"+unitVHost+"/"+QuarantineQueue].(map[string]any)["arguments"].(amqp.Table)["x-delivery-limit"] = 20
		}},
		{"user_policy", func(p *topologyPeer) {
			p.responses["/api/policies/"+unitVHost] = []any{map[string]any{"definition": map[string]any{"overflow": "drop-head"}}}
		}},
		{"operator_policy", func(p *topologyPeer) {
			p.responses["/api/operator-policies/"+unitVHost] = []any{map[string]any{"definition": map[string]any{"message-ttl": 1}}}
		}},
		{"effective_policy", func(p *topologyPeer) {
			p.responses[queue].(map[string]any)["effective_policy_definition"] = map[string]any{"overflow": "drop-head"}
		}},
		{"fanout_exchange", func(p *topologyPeer) { p.responses[exchange].(map[string]any)["type"] = "fanout" }},
		{"internal_exchange", func(p *topologyPeer) { p.responses[exchange].(map[string]any)["internal"] = true }},
		{"extra_exchange_arguments", func(p *topologyPeer) {
			p.responses[exchange].(map[string]any)["arguments"] = map[string]string{"alternate-exchange": "foreign"}
		}},
		{"extra_binding", func(p *topologyPeer) {
			path := exchange + "/bindings/source"
			p.responses[path] = append(p.responses[path].([]any), map[string]string{"destination": "foreign"})
		}},
		{"wrong_binding", func(p *topologyPeer) {
			p.responses[exchange+"/bindings/source"].([]any)[0].(map[string]any)["routing_key"] = "wrong"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validPeer()
			tc.mutate(p)
			m := p.management(t)
			if err := EnsureLocal(context.Background(), p, m, unitVHost); err == nil {
				t.Fatal("drift accepted")
			}
			if len(p.writes) != 0 {
				t.Fatal("modified broker despite preflight failure", p.writes)
			}
		})
	}
}

func TestLocalTopologyCreatesQuarantineFirstAndChecksReadOnly(t *testing.T) {
	p := validPeer()
	for path := range p.responses {
		if strings.HasPrefix(path, "/api/queues/") || strings.HasPrefix(path, "/api/exchanges/") {
			delete(p.responses, path)
		}
	}
	m := p.management(t)
	if err := VerifyLocal(context.Background(), m, unitVHost); err == nil {
		t.Fatal("missing topology passed check")
	}
	if err := EnsureLocal(context.Background(), p, m, unitVHost); err != nil {
		t.Fatal(err)
	}
	expected := []string{"exchange:" + QuarantineExchange, "queue:" + QuarantineQueue, "binding:" + QuarantineQueue, "exchange:" + CommandsExchange, "queue:" + PersistQueue, "binding:" + PersistQueue}
	if !reflect.DeepEqual(p.writes, expected) {
		t.Fatal("unsafe creation order", p.writes)
	}
	if err := VerifyLocal(context.Background(), m, unitVHost); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.writes, expected) {
		t.Fatal("check mutated topology")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := EnsureLocal(ctx, p, m, unitVHost); !errors.Is(err, context.Canceled) {
		t.Fatal("ignored cancellation", err)
	}
	if len(p.writes) != 6 {
		t.Fatal("canceled check mutated topology")
	}
}

func TestManagementBoundsAndSecrets(t *testing.T) {
	for _, address := range []string{"http://example.com:15672", "http://admin:secret@localhost:15672", "ftp://localhost", "http://localhost/api", "http://localhost?token=secret"} {
		if _, err := NewManagement(address, "admin", "secret"); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid endpoint allowed or leaked")
		}
	}
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://example.com/secret", 302) }},
		{"large", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat("x", (1<<20)+1))) }},
		{"trailing_json", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{} {}`)) }},
		{"server_error", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500); w.Write([]byte("secret")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			m, err := NewManagement(server.URL, "admin", "secret")
			if err != nil {
				t.Fatal(err)
			}
			_, err = m.get(context.Background(), "/api/overview", new(any))
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatal("invalid response accepted or leaked")
			}
		})
	}
	if url.PathEscape(LocalVHost) != "%2Fichat" {
		t.Fatal("vhost must be escaped as a single segment")
	}
}
