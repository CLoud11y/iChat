package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"iChat/messaging"
)

const acceptanceProbeBody = `{"clientMessageId":"8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1","targetType":"private","targetId":"34","media":"text","content":"pending probe"}`

func probeOptions(t *testing.T, base string) options {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pending-request.json")
	if err := os.WriteFile(path, []byte(acceptanceProbeBody), 0600); err != nil {
		t.Fatal(err)
	}
	return options{mode: "acceptance", baseURL: base, senderID: 42, requestFile: path, attempts: 1, requestTimeout: time.Second}
}

func probeResponse(t *testing.T, r *http.Request) messaging.SubmissionResponse {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := messaging.DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	command, err := messaging.Normalize(42, request)
	if err != nil {
		t.Fatal(err)
	}
	e, err := messaging.NewEnvelope(command, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return messaging.SubmissionResponse{Code: 0, Msg: "accepted", Data: messaging.Submission{State: "accepted", CommandID: command.CommandID, ClientMessageID: command.ClientMessageID, PayloadHash: command.PayloadHash, PublishAttemptID: e.PublishAttemptID}}
}

func TestAcceptanceProbeStableRequestAndKnownAcceptedDoesNotRegress(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/sendMessage" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer probe-fixture-token" {
			t.Error("probe used legacy account/WS protocol or lost authorization")
		}
		response := probeResponse(t, r)
		switch calls.Add(1) {
		case 1:
			response.Code, response.Msg, response.Data.State, response.Data.ReasonCode = -1, "unknown", "publish_unknown", "PUBLISH_UNKNOWN"
			w.WriteHeader(503)
		case 2:
			w.WriteHeader(202)
		default:
			// A legacy or lost response after known acceptance cannot undo it.
			w.WriteHeader(200)
		}
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	opt := probeOptions(t, server.URL)
	opt.attempts = 3
	report, err := acceptanceProbe(opt, "probe-fixture-token")
	if err != nil || report.Accepted != 1 || report.Unknown != 2 || report.State != "accepted" || calls.Load() != 3 {
		t.Fatal("incorrect reliable probe counts", report, err)
	}
	retained, err := os.ReadFile(opt.requestFile)
	if err != nil || string(retained) != acceptanceProbeBody || report.ClientMessageID == "" || report.CommandID == "" || report.PayloadHash == "" {
		t.Fatal("probe discarded/rewrote recovery request", err)
	}
}

func TestAcceptanceProbeUnknownAndDefiniteFailureClassification(t *testing.T) {
	for _, scenario := range []string{"lost_response", "bad_json", "large_response", "legacy_200", "wrong_identity", "no_attempt", "busy", "unavailable", "unroutable", "nack", "invalid", "unauthenticated"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				response := probeResponse(t, r)
				status := 202
				switch scenario {
				case "lost_response":
					connection, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						connection.Close()
					}
					return
				case "bad_json":
					w.WriteHeader(202)
					w.Write([]byte("{"))
					return
				case "large_response":
					w.WriteHeader(202)
					w.Write([]byte(strings.Repeat("x", messaging.MaxBodyBytes+1)))
					return
				case "legacy_200":
					status = 200
				case "wrong_identity":
					response.Data.PayloadHash = strings.Repeat("0", 64)
				case "no_attempt":
					response.Data.PublishAttemptID = ""
				case "busy":
					status, response.Code, response.Data.State, response.Data.ReasonCode = 429, -1, "not_published", "OVERLOADED"
				case "unavailable":
					status, response.Code, response.Data.State, response.Data.ReasonCode = 503, -1, "not_published", "DEPENDENCY_UNAVAILABLE"
				case "unroutable":
					status, response.Code, response.Data.State, response.Data.ReasonCode = 503, -1, "unroutable", "PUBLISH_UNROUTABLE"
				case "nack":
					status, response.Code, response.Data.State, response.Data.ReasonCode = 503, -1, "nacked", "DEPENDENCY_UNAVAILABLE"
				case "invalid":
					status, response.Code, response.Data.State, response.Data.ReasonCode = 400, -1, "not_published", "INVALID_REQUEST"
				case "unauthenticated":
					status, response.Code, response.Data.State, response.Data.ReasonCode = 401, -1, "", "UNAUTHENTICATED"
				}
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			opt := probeOptions(t, server.URL)
			report, err := acceptanceProbe(opt, "probe-fixture-token")
			if err != nil || report.Accepted != 0 {
				t.Fatal("non-proof became accepted", report, err)
			}
			switch scenario {
			case "busy", "unavailable":
				if report.NotPublished != 1 {
					t.Fatal(report)
				}
			case "unroutable", "nack", "invalid", "unauthenticated":
				if report.Rejected != 1 {
					t.Fatal(report)
				}
			default:
				if report.Unknown != 1 || report.State != "publish_unknown" {
					t.Fatal(report)
				}
			}
			if retained, err := os.ReadFile(opt.requestFile); err != nil || string(retained) != acceptanceProbeBody {
				t.Fatal("original recovery request lost", err)
			}
		})
	}
}

func TestAcceptanceProbeRefusesRedirectAndInvalidSetupBeforeIO(t *testing.T) {
	var forwarded atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded.Add(1) }))
	defer sink.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	opt := probeOptions(t, redirect.URL)
	if report, err := acceptanceProbe(opt, "probe-fixture-token"); err != nil || report.Unknown != 1 || forwarded.Load() != 0 {
		t.Fatal("probe followed redirect with credentials/payload", report, err)
	}
	for _, scenario := range []string{"sender", "attempts", "timeout", "token", "body", "output_alias"} {
		t.Run(scenario, func(t *testing.T) {
			opt := probeOptions(t, sink.URL)
			token := "probe-fixture-token"
			switch scenario {
			case "sender":
				opt.senderID = 0
			case "attempts":
				opt.attempts = 101
			case "timeout":
				opt.requestTimeout = 0
			case "token":
				token = ""
			case "body":
				if err := os.WriteFile(opt.requestFile, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "output_alias":
				opt.output = opt.requestFile
			}
			if _, err := acceptanceProbe(opt, token); !errors.Is(err, errAcceptanceProbeOptions) || forwarded.Load() != 0 {
				t.Fatal("invalid probe performed I/O", err)
			}
		})
	}
}
