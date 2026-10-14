package config

import (
	"fmt"

	"iChat/messaging/rabbitmq"
)

// This is a configuration admission bound, not a measured production capacity.
const MaxConsumerWorkers = 32

// ConsumerConfig is a validated input to explicit connection construction.
// Workers counts persistence sessions; the audit role uses one separate session.
type ConsumerConfig struct {
	URL        string
	VHost      string
	Management *rabbitmq.Management
	Options    rabbitmq.ConnectionOptions
	Workers    int
	Prefetch   int
}

func (ConsumerConfig) String() string {
	return "RabbitMQ consumer configuration (credentials redacted)"
}
func (c ConsumerConfig) GoString() string { return c.String() }

func (c RabbitMQConfig) Consumer() (ConsumerConfig, error) {
	if c.URL == "" && c.ManagementURL == "" && c.ManagementUsername == "" && c.ManagementPassword == "" {
		return ConsumerConfig{}, ErrRabbitMQNotConfigured
	}
	if c.ConsumerWorkers < 1 || c.ConsumerWorkers > MaxConsumerWorkers {
		return ConsumerConfig{}, fmt.Errorf("%w: RABBITMQ.consumerWorkers", ErrRabbitMQConfig)
	}
	if c.ConsumerPrefetch < 1 || c.ConsumerPrefetch > rabbitmq.MaxConsumerPrefetch {
		return ConsumerConfig{}, fmt.Errorf("%w: RABBITMQ.consumerPrefetch", ErrRabbitMQConfig)
	}
	o := rabbitmq.DefaultConnectionOptions()
	o.ConnectTimeout, o.OperationTimeout = c.ConnectTimeout, c.ConsumerOperationTimeout
	o.WriteTimeout, o.CloseTimeout, o.Heartbeat = c.WriteTimeout, c.CloseTimeout, c.Heartbeat
	if err := rabbitmq.ValidateConsumerOptions(c.URL, c.VHost, o); err != nil {
		return ConsumerConfig{}, ErrRabbitMQConfig
	}
	m, err := rabbitmq.NewManagement(c.ManagementURL, c.ManagementUsername, c.ManagementPassword)
	if err != nil {
		return ConsumerConfig{}, ErrRabbitMQConfig
	}
	return ConsumerConfig{URL: c.URL, VHost: c.VHost, Management: m, Options: o, Workers: c.ConsumerWorkers, Prefetch: c.ConsumerPrefetch}, nil
}

type ConsumerGroup struct {
	Queue    string
	Settings ConsumerConfig
}

// MQRuntimePlan contains only MQ dependency inputs. It neither implements the
// Outbox/API roles nor establishes readiness, subscriptions or connections.
type MQRuntimePlan struct {
	Mode      RuntimeMode
	Publisher *PublisherConfig
	Consumers []ConsumerGroup
}

func (MQRuntimePlan) String() string {
	return "RabbitMQ runtime dependency plan (credentials redacted)"
}
func (c MQRuntimePlan) GoString() string { return c.String() }

// MQRuntime builds just the inputs selected by the validated runtime roles.
// The caller must explicitly construct and own every resulting connection.
func (c Config) MQRuntime() (MQRuntimePlan, error) {
	roles, err := c.RuntimePlan()
	if err != nil {
		return MQRuntimePlan{}, err
	}
	plan := MQRuntimePlan{Mode: roles.Mode}
	if roles.Publisher {
		settings, err := c.RABBITMQ.Publisher()
		if err != nil {
			return MQRuntimePlan{}, fmt.Errorf("configure command publisher: %w", err)
		}
		plan.Publisher = &settings
	}
	if roles.PersistenceConsumer || roles.QuarantineConsumer {
		settings, err := c.RABBITMQ.Consumer()
		if err != nil {
			return MQRuntimePlan{}, fmt.Errorf("configure consumer connections: %w", err)
		}
		if roles.PersistenceConsumer {
			plan.Consumers = append(plan.Consumers, ConsumerGroup{Queue: rabbitmq.PersistQueue, Settings: settings})
		}
		if roles.QuarantineConsumer {
			settings.Workers = 1
			plan.Consumers = append(plan.Consumers, ConsumerGroup{Queue: rabbitmq.QuarantineQueue, Settings: settings})
		}
	}
	return plan, nil
}
