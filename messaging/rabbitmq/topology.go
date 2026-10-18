// Package rabbitmq implements the versioned messaging topology. No connection
// is opened at import time. The modern API verifies it without repairing drift.
package rabbitmq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	BrokerVersion        = "4.3.6"
	LocalVHost           = "/ichat"
	LocalAppUser         = "ichat_app"
	ResourcePattern      = `^ichat\.`
	CommandsExchange     = "ichat.commands.v1"
	PersistQueue         = "ichat.message.persist.v1"
	SubmitRoutingKey     = "message.submit.v1"
	QuarantineExchange   = "ichat.quarantine.v1"
	QuarantineQueue      = "ichat.message.quarantine.v1"
	QuarantineRoutingKey = "message.quarantine.v1"
	DeliveryLimit        = 50
	// A local starting bound per queue, not a measured production capacity.
	LocalMaxLengthBytes = 64 * 1024 * 1024
)

// TopologyChannel is satisfied by *amqp.Channel. Its caller owns the channel,
// connection and transport deadlines. Recovery must be disabled by the caller.
type TopologyChannel interface {
	ExchangeDeclare(string, string, bool, bool, bool, bool, amqp.Table) error
	QueueDeclare(string, bool, bool, bool, bool, amqp.Table) (amqp.Queue, error)
	QueueBind(string, string, string, bool, amqp.Table) error
}

type queueSpec struct {
	name, exchange, key string
	arguments           amqp.Table
}

func localQueues() []queueSpec {
	// Always build a fresh map: callers cannot mutate the shared contract.
	return []queueSpec{
		{QuarantineQueue, QuarantineExchange, QuarantineRoutingKey, amqp.Table{"x-queue-type": "quorum", "x-quorum-initial-group-size": int32(1), "x-overflow": "reject-publish", "x-max-length-bytes": int64(LocalMaxLengthBytes), "x-delivery-limit": int32(-1)}},
		{PersistQueue, CommandsExchange, SubmitRoutingKey, amqp.Table{"x-queue-type": "quorum", "x-quorum-initial-group-size": int32(1), "x-overflow": "reject-publish", "x-max-length-bytes": int64(LocalMaxLengthBytes), "x-delivery-limit": int32(DeliveryLimit), "x-dead-letter-strategy": "at-least-once", "x-dead-letter-exchange": QuarantineExchange, "x-dead-letter-routing-key": QuarantineRoutingKey}},
	}
}

// PreflightLocal checks the broker and every existing object before any writes.
// Missing objects are permitted, incompatible objects and all user/operator
// policies in this dedicated local vhost are rejected. It never repairs by
// deleting queues, changing policies or enabling feature flags.
func PreflightLocal(ctx context.Context, m *Management, vhost string) error {
	return inspectLocal(ctx, m, vhost, true)
}

// VerifyLocal is a read-only, fail-closed check, including bindings and actual
// online quorum members. Success is a point-in-time topology check, not API
// readiness or a publisher confirm.
func VerifyLocal(ctx context.Context, m *Management, vhost string) error {
	return inspectLocal(ctx, m, vhost, false)
}

func EnsureLocal(ctx context.Context, ch TopologyChannel, m *Management, vhost string) error {
	if ch == nil {
		return errors.New("RabbitMQ topology channel is required")
	}
	if err := PreflightLocal(ctx, m, vhost); err != nil {
		return err
	}
	for _, q := range localQueues() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ch.ExchangeDeclare(q.exchange, "direct", true, false, false, false, nil); err != nil {
			return fmt.Errorf("declare %s: %w", q.exchange, err)
		}
		if _, err := ch.QueueDeclare(q.name, true, false, false, false, q.arguments); err != nil {
			return fmt.Errorf("declare %s: %w", q.name, err)
		}
		if err := ch.QueueBind(q.name, q.key, q.exchange, false, nil); err != nil {
			return fmt.Errorf("bind %s: %w", q.name, err)
		}
	}
	return VerifyLocal(ctx, m, vhost)
}

func inspectLocal(ctx context.Context, m *Management, vhost string, allowMissing bool) error {
	if vhost == "" {
		return errors.New("RabbitMQ vhost is required")
	}
	var overview struct {
		Version string `json:"rabbitmq_version"`
	}
	if _, err := m.get(ctx, "/api/overview", &overview); err != nil {
		return err
	}
	if overview.Version != BrokerVersion {
		return fmt.Errorf("RabbitMQ broker version must be %s", BrokerVersion)
	}
	var flags []struct {
		Name  string `json:"name"`
		State string `json:"state"`
	}
	if _, err := m.get(ctx, "/api/feature-flags", &flags); err != nil {
		return err
	}
	for _, required := range []string{"quorum_queue", "stream_queue"} {
		enabled := false
		for _, flag := range flags {
			if flag.Name == required && flag.State == "enabled" {
				enabled = true
			}
		}
		if !enabled {
			return fmt.Errorf("RabbitMQ required feature flag %s is not enabled", required)
		}
	}
	v := url.PathEscape(vhost)
	if _, err := m.get(ctx, "/api/vhosts/"+v, new(any)); err != nil {
		return err
	}
	for _, kind := range []string{"policies", "operator-policies"} {
		var policies []json.RawMessage
		if _, err := m.get(ctx, "/api/"+kind+"/"+v, &policies); err != nil {
			return err
		}
		if len(policies) != 0 {
			return fmt.Errorf("RabbitMQ local vhost must have no %s; refusing possible topology overrides", kind)
		}
	}
	for _, q := range localQueues() {
		var exchange struct {
			Name       string         `json:"name"`
			VHost      string         `json:"vhost"`
			Type       string         `json:"type"`
			Durable    bool           `json:"durable"`
			AutoDelete bool           `json:"auto_delete"`
			Internal   bool           `json:"internal"`
			Arguments  map[string]any `json:"arguments"`
		}
		status, err := m.request(ctx, http.MethodGet, "/api/exchanges/"+v+"/"+q.exchange, nil, &exchange)
		if err != nil {
			return err
		}
		if status == http.StatusNotFound {
			if !allowMissing {
				return fmt.Errorf("RabbitMQ exchange %s is missing", q.exchange)
			}
		} else if exchange.Name != q.exchange || exchange.VHost != vhost || exchange.Type != "direct" || !exchange.Durable || exchange.AutoDelete || exchange.Internal || exchange.Arguments == nil || len(exchange.Arguments) != 0 {
			return fmt.Errorf("RabbitMQ exchange %s does not match the local contract", q.exchange)
		}
		var queue struct {
			Name           string         `json:"name"`
			VHost          string         `json:"vhost"`
			Type           string         `json:"type"`
			Durable        bool           `json:"durable"`
			AutoDelete     bool           `json:"auto_delete"`
			Exclusive      bool           `json:"exclusive"`
			Arguments      map[string]any `json:"arguments"`
			Members        []string       `json:"members"`
			Online         []string       `json:"online"`
			State          string         `json:"state"`
			Policy         string         `json:"policy"`
			OperatorPolicy string         `json:"operator_policy"`
			Effective      map[string]any `json:"effective_policy_definition"`
		}
		status, err = m.request(ctx, http.MethodGet, "/api/queues/"+v+"/"+q.name, nil, &queue)
		if err != nil {
			return err
		}
		if status == http.StatusNotFound {
			if !allowMissing {
				return fmt.Errorf("RabbitMQ queue %s is missing", q.name)
			}
		} else {
			if queue.Name != q.name || queue.VHost != vhost || queue.Type != "quorum" || !queue.Durable || queue.AutoDelete || queue.Exclusive || !sameJSON(queue.Arguments, q.arguments) || queue.Policy != "" || queue.OperatorPolicy != "" || len(queue.Effective) != 0 {
				return fmt.Errorf("RabbitMQ queue %s does not match the local contract", q.name)
			}
			if queue.State != "running" || len(queue.Members) != 1 || len(queue.Online) != 1 || queue.Members[0] == "" || queue.Online[0] != queue.Members[0] {
				return fmt.Errorf("RabbitMQ queue %s must have one online quorum member", q.name)
			}
		}
		var bindings []struct {
			Source          string         `json:"source"`
			VHost           string         `json:"vhost"`
			Destination     string         `json:"destination"`
			DestinationType string         `json:"destination_type"`
			RoutingKey      string         `json:"routing_key"`
			Arguments       map[string]any `json:"arguments"`
		}
		status, err = m.request(ctx, http.MethodGet, "/api/exchanges/"+v+"/"+q.exchange+"/bindings/source", nil, &bindings)
		if err != nil {
			return err
		}
		if len(bindings) == 0 && allowMissing {
			continue
		}
		if status == http.StatusNotFound || len(bindings) != 1 {
			return fmt.Errorf("RabbitMQ exchange %s has missing or extra bindings", q.exchange)
		}
		b := bindings[0]
		if b.Source != q.exchange || b.VHost != vhost || b.Destination != q.name || b.DestinationType != "queue" || b.RoutingKey != q.key || b.Arguments == nil || len(b.Arguments) != 0 {
			return fmt.Errorf("RabbitMQ binding for %s does not match the local contract", q.name)
		}
	}
	return nil
}

func sameJSON(a, b any) bool {
	x, err := json.Marshal(a)
	if err != nil {
		return false
	}
	y, err := json.Marshal(b)
	return err == nil && string(x) == string(y)
}
