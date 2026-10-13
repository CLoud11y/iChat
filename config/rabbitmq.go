package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
	"iChat/messaging/rabbitmq"
)

var (
	ErrRabbitMQConfig        = errors.New("invalid RabbitMQ application configuration")
	ErrRabbitMQNotConfigured = errors.New("RabbitMQ application connection is not configured")
)

// RabbitMQConfig is preparation for explicit publisher assembly. Loading it
// does not enable MQ, start workers, or change the legacy message transport.
type RabbitMQConfig struct {
	URL                string
	VHost              string
	ManagementURL      string
	ManagementUsername string
	ManagementPassword string
	PublisherSlots     int
	PublishTimeout     time.Duration
	ConnectTimeout     time.Duration
	WriteTimeout       time.Duration
	CloseTimeout       time.Duration
	Heartbeat          time.Duration
	RetryMin           time.Duration
	RetryMax           time.Duration
}

func (RabbitMQConfig) String() string {
	return "RabbitMQ application configuration (credentials redacted)"
}
func (c RabbitMQConfig) GoString() string { return c.String() }

// PublisherConfig contains validated inputs, not an active network connection.
type PublisherConfig struct {
	URL        string
	VHost      string
	Management *rabbitmq.Management
	Options    rabbitmq.SupervisorOptions
}

func (PublisherConfig) String() string {
	return "RabbitMQ publisher configuration (credentials redacted)"
}
func (c PublisherConfig) GoString() string { return c.String() }

// Publisher builds the bounded management client and supervisor settings
// without I/O. It also validates explicitly constructed configurations.
func (c RabbitMQConfig) Publisher() (PublisherConfig, error) {
	if c.URL == "" && c.ManagementURL == "" && c.ManagementUsername == "" && c.ManagementPassword == "" {
		return PublisherConfig{}, ErrRabbitMQNotConfigured
	}
	o := rabbitmq.DefaultSupervisorOptions()
	o.Session.Publisher.Slots = c.PublisherSlots
	o.Session.OperationTimeout = c.PublishTimeout
	o.Session.ConnectTimeout = c.ConnectTimeout
	o.Session.WriteTimeout = c.WriteTimeout
	o.Session.CloseTimeout = c.CloseTimeout
	o.Session.Heartbeat = c.Heartbeat
	o.RetryMin, o.RetryMax = c.RetryMin, c.RetryMax
	if err := rabbitmq.ValidateSupervisorOptions(c.URL, c.VHost, o); err != nil {
		return PublisherConfig{}, ErrRabbitMQConfig
	}
	m, err := rabbitmq.NewManagement(c.ManagementURL, c.ManagementUsername, c.ManagementPassword)
	if err != nil {
		return PublisherConfig{}, ErrRabbitMQConfig
	}
	return PublisherConfig{URL: c.URL, VHost: c.VHost, Management: m, Options: o}, nil
}

func loadRabbitMQ(v *viper.Viper) (RabbitMQConfig, error) {
	var c RabbitMQConfig
	known := map[string]bool{
		"url": true, "vhost": true, "managementurl": true, "managementusername": true, "managementpassword": true,
		"publisherslots": true, "publishtimeout": true, "connecttimeout": true, "writetimeout": true,
		"closetimeout": true, "heartbeat": true, "retrymin": true, "retrymax": true,
	}
	var section map[string]interface{}
	if v.IsSet("RABBITMQ") {
		var ok bool
		section, ok = v.Get("RABBITMQ").(map[string]interface{})
		if !ok {
			return RabbitMQConfig{}, ErrRabbitMQConfig
		}
		for key := range section {
			if !known[strings.ToLower(key)] {
				return RabbitMQConfig{}, fmt.Errorf("%w: unknown RABBITMQ field", ErrRabbitMQConfig)
			}
		}
	}
	// Read each known key directly: AutomaticEnv alone does not add omitted
	// YAML keys to Viper's Unmarshal set. LookupEnv also honors explicit empty
	// MQ overrides, so an empty secret cannot silently reuse a file secret.
	read := func(key string, fallback any) any {
		if value, ok := os.LookupEnv("ICHAT_RABBITMQ_" + strings.ToUpper(key)); ok {
			return value
		}
		if value, ok := section[strings.ToLower(key)]; ok {
			return value
		}
		if v.IsSet("RABBITMQ." + key) {
			return v.Get("RABBITMQ." + key)
		}
		return fallback
	}
	invalid := func(key string) error { return fmt.Errorf("%w: RABBITMQ.%s", ErrRabbitMQConfig, key) }
	for _, field := range []struct {
		key, fallback string
		target        *string
	}{
		{"url", "", &c.URL}, {"vhost", rabbitmq.LocalVHost, &c.VHost},
		{"managementURL", "", &c.ManagementURL}, {"managementUsername", "", &c.ManagementUsername},
		{"managementPassword", "", &c.ManagementPassword},
	} {
		value, ok := read(field.key, field.fallback).(string)
		if !ok {
			return RabbitMQConfig{}, invalid(field.key)
		}
		*field.target = value
	}
	defaults := rabbitmq.DefaultSupervisorOptions()
	switch value := read("publisherSlots", defaults.Session.Publisher.Slots).(type) {
	case int:
		c.PublisherSlots = value
	case string:
		n, err := strconv.Atoi(value)
		if err != nil {
			return RabbitMQConfig{}, invalid("publisherSlots")
		}
		c.PublisherSlots = n
	default:
		return RabbitMQConfig{}, invalid("publisherSlots")
	}
	if c.PublisherSlots < 1 || c.PublisherSlots > rabbitmq.MaxPublisherSlots {
		return RabbitMQConfig{}, invalid("publisherSlots")
	}
	for _, field := range []struct {
		key      string
		fallback time.Duration
		target   *time.Duration
	}{
		{"publishTimeout", defaults.Session.OperationTimeout, &c.PublishTimeout},
		{"connectTimeout", defaults.Session.ConnectTimeout, &c.ConnectTimeout},
		{"writeTimeout", defaults.Session.WriteTimeout, &c.WriteTimeout},
		{"closeTimeout", defaults.Session.CloseTimeout, &c.CloseTimeout},
		{"heartbeat", defaults.Session.Heartbeat, &c.Heartbeat},
		{"retryMin", defaults.RetryMin, &c.RetryMin}, {"retryMax", defaults.RetryMax, &c.RetryMax},
	} {
		value, ok := read(field.key, field.fallback.String()).(string)
		if !ok {
			return RabbitMQConfig{}, invalid(field.key)
		}
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 || d > time.Minute {
			return RabbitMQConfig{}, invalid(field.key)
		}
		*field.target = d
	}
	if c.Heartbeat < time.Second || c.Heartbeat%time.Second != 0 {
		return RabbitMQConfig{}, invalid("heartbeat")
	}
	if c.RetryMin < time.Millisecond || c.RetryMax < c.RetryMin {
		return RabbitMQConfig{}, invalid("retryMin/retryMax")
	}
	if c.VHost == "" {
		return RabbitMQConfig{}, invalid("vhost")
	}
	if _, err := c.Publisher(); err != nil && !errors.Is(err, ErrRabbitMQNotConfigured) {
		return RabbitMQConfig{}, err
	}
	return c, nil
}
