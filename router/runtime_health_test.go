package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"iChat/config"
	"iChat/health"
	"iChat/service"
)

type runtimeState struct{ status health.RuntimeStatus }

func (r *runtimeState) RuntimeReadiness() health.RuntimeStatus { return r.status }

func TestRuntimeHealthHTTPUsesCurrentSeparateObservations(t *testing.T) {
	state := &runtimeState{}
	r := gin.New()
	RegisterHealth(r, storageState(false), state)
	for _, tc := range []struct {
		state health.RuntimeStatus
		path  string
		code  int
		body  string
	}{
		{health.RuntimeStatus{}, "/health/ready/accept", 503, "MQ_ACCEPTANCE_NOT_CONFIGURED"},
		{health.RuntimeStatus{Acceptance: health.Starting}, "/health/ready/accept", 503, "RUNTIME_STARTING"},
		{health.RuntimeStatus{Acceptance: health.Ready}, "/health/ready/accept", 200, `"status":"ready"`},
		{health.RuntimeStatus{Acceptance: health.AuthenticationUnavailable}, "/health/ready/accept", 503, "AUTHENTICATION_UNAVAILABLE"},
		{health.RuntimeStatus{Acceptance: health.PublisherUnavailable}, "/health/ready/accept", 503, "MQ_PUBLISHER_UNAVAILABLE"},
		{health.RuntimeStatus{Acceptance: health.Ready, Consumers: health.ConsumerUnavailable}, "/health/ready/accept", 200, `"status":"ready"`},
		{health.RuntimeStatus{Acceptance: health.Ready, Consumers: health.ConsumerUnavailable}, "/health/ready/consumers", 503, "MQ_CONSUMER_UNAVAILABLE"},
		{health.RuntimeStatus{Acceptance: health.PublisherUnavailable, Consumers: health.Ready}, "/health/ready/consumers", 200, `"status":"ready"`},
		{health.RuntimeStatus{Acceptance: health.Stopping, Consumers: health.Stopping}, "/health/ready/accept", 503, "RUNTIME_STOPPING"},
		{health.RuntimeStatus{Acceptance: health.Stopping, Consumers: health.Stopping}, "/health/ready/consumers", 503, "RUNTIME_STOPPING"},
		{health.RuntimeStatus{Acceptance: health.ReadinessState(255)}, "/health/ready/accept", 503, "READINESS_UNAVAILABLE"},
	} {
		state.status = tc.state
		response := httptest.NewRecorder()
		r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if response.Code != tc.code || !strings.Contains(response.Body.String(), tc.body) || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("incorrect readiness response", tc.path, response.Code, response.Body.String())
		}
	}
	// Acceptance can remain ready during MySQL/consumer outages. The separate
	// storage endpoint retains its own unavailable result, and liveness stays up.
	state.status = health.RuntimeStatus{Acceptance: health.Ready}
	for _, tc := range []struct {
		path string
		code int
	}{{"/health/live", 200}, {"/health/ready/storage", 503}} {
		response := httptest.NewRecorder()
		r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if response.Code != tc.code {
			t.Fatal("runtime health changed liveness/storage semantics", tc.path, response.Code)
		}
	}
}

func TestRuntimeHealthHTTPNilAndLegacyFailClosed(t *testing.T) {
	var typedNil *runtimeState
	for _, observed := range []health.RuntimeHealth{nil, typedNil} {
		r := gin.New()
		RegisterHealth(r, storageState(true), observed)
		response := httptest.NewRecorder()
		r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/ready/accept", nil))
		if response.Code != 503 || !strings.Contains(response.Body.String(), "MQ_ACCEPTANCE_NOT_CONFIGURED") {
			t.Fatal("nil capability became ready", response.Code, response.Body.String())
		}
	}
	// Even a modern mode in a Config cannot turn legacy handlers into a modern
	// API. Only the explicit modern RegisterHealth entry accepts a capability.
	r := Router(config.Config{RUNTIME: config.RuntimeConfig{Mode: config.ModeAPI}}, nil, nil, service.ContactQueries{}, service.ContactWrites{}, nil, nil, storageState(true))
	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/ready/accept", nil))
	if response.Code != 503 || !strings.Contains(response.Body.String(), "MQ_ACCEPTANCE_NOT_CONFIGURED") {
		t.Fatal("legacy routes falsely advertised confirmed acceptance")
	}
}
