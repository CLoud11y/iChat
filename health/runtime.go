package health

import "reflect"

// ReadinessState is deliberately closed to arbitrary diagnostic strings. HTTP
// adapters expose only these reason codes, never URLs or dependency errors.
type ReadinessState uint8

const (
	NotConfigured ReadinessState = iota
	Starting
	Stopping
	ServiceUnavailable
	AuthenticationUnavailable
	PublisherUnavailable
	ConsumerUnavailable
	Ready
	OutboxUnavailable
)

func (s ReadinessState) ReasonCode() string {
	switch s {
	case NotConfigured:
		return "MQ_ACCEPTANCE_NOT_CONFIGURED"
	case Starting:
		return "RUNTIME_STARTING"
	case Stopping:
		return "RUNTIME_STOPPING"
	case ServiceUnavailable:
		return "RUNTIME_SERVICE_UNAVAILABLE"
	case AuthenticationUnavailable:
		return "AUTHENTICATION_UNAVAILABLE"
	case PublisherUnavailable:
		return "MQ_PUBLISHER_UNAVAILABLE"
	case ConsumerUnavailable:
		return "MQ_CONSUMER_UNAVAILABLE"
	case Ready:
		return "READY"
	case OutboxUnavailable:
		return "OUTBOX_UNAVAILABLE"
	default:
		return "READINESS_UNAVAILABLE"
	}
}

type RuntimeStatus struct {
	Acceptance ReadinessState
	Consumers  ReadinessState
	Outbox     ReadinessState
}

// RuntimeHealth is an observation capability, with no setters or lifecycle
// ownership. Implementations must use cached state only: no I/O, waits or RPCs.
type RuntimeHealth interface {
	RuntimeReadiness() RuntimeStatus
}

// ObserveRuntime treats nil, including a typed nil capability, as unconfigured.
func ObserveRuntime(runtime RuntimeHealth) RuntimeStatus {
	if runtime == nil {
		return RuntimeStatus{}
	}
	v := reflect.ValueOf(runtime)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if v.IsNil() {
			return RuntimeStatus{}
		}
	}
	return runtime.RuntimeReadiness()
}
