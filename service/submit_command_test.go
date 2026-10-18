package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
)

const submissionBody = `{"clientMessageId":"8FD7C2EF-AE26-4BB5-91A5-32A302BB05F1","targetType":"private","targetId":"00034","media":"text","content":" 中🙂\u0000\r\n","clientSentAt":"2026-10-18T09:00:00+11:00"}`

type submissionPublisher func(context.Context, messaging.Command, time.Time) (rabbitmq.PublishResult, error)

func (f submissionPublisher) Publish(ctx context.Context, c messaging.Command, at time.Time) (rabbitmq.PublishResult, error) {
	return f(ctx, c, at)
}

func submissionReceipt(t *testing.T, command messaging.Command, at time.Time) rabbitmq.PublishResult {
	t.Helper()
	e, err := messaging.NewEnvelope(command, at)
	if err != nil {
		t.Fatal(err)
	}
	return rabbitmq.PublishResult{Outcome: rabbitmq.PublishAccepted, CommandID: command.CommandID, PublishAttemptID: e.PublishAttemptID, ConnectionEpoch: 1, ChannelEpoch: 1, DeliveryTag: 1}
}

func submissionRoute(t *testing.T, publisher CommandPublisher, uid any, timeout time.Duration) *gin.Engine {
	t.Helper()
	handler, err := SubmitCommand(publisher, timeout)
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.POST("/", func(c *gin.Context) {
		if uid != nil {
			c.Set("uid", uid)
		}
		c.Next()
	}, handler)
	return r
}

func submitRequest(t *testing.T, r http.Handler, body string, ctx context.Context) (*httptest.ResponseRecorder, messaging.SubmissionResponse) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	response := httptest.NewRecorder()
	r.ServeHTTP(response, request)
	var decoded messaging.SubmissionResponse
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return response, decoded
}

func TestSubmitCommandCanonicalIdentityAndStableRetry(t *testing.T) {
	var commands []messaging.Command
	var attempts []string
	pub := submissionPublisher(func(ctx context.Context, c messaging.Command, at time.Time) (rabbitmq.PublishResult, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Second || at.IsZero() {
			t.Fatal("unbounded publisher context or missing server timestamp")
		}
		commands = append(commands, c)
		r := submissionReceipt(t, c, at)
		attempts = append(attempts, r.PublishAttemptID)
		if len(commands) == 1 {
			r.Outcome = rabbitmq.PublishUnknown
			return r, context.DeadlineExceeded
		}
		return r, nil
	})
	r := submissionRoute(t, pub, uint(42), time.Second)
	first, unknown := submitRequest(t, r, submissionBody, context.Background())
	second, accepted := submitRequest(t, r, submissionBody, context.Background())
	if first.Code != 503 || unknown.Data.State != "publish_unknown" || unknown.Data.ReasonCode != "PUBLISH_UNKNOWN" || second.Code != 202 || accepted.Code != 0 || accepted.Msg != "accepted" {
		t.Fatal("wrong response outcomes", unknown, accepted)
	}
	if len(commands) != 2 || commands[0] != commands[1] || commands[0].SenderID != "42" || commands[0].TargetID != "34" || commands[0].ClientMessageID != strings.ToLower("8FD7C2EF-AE26-4BB5-91A5-32A302BB05F1") || commands[0].Content != " 中🙂\x00\r\n" || commands[0].ClientSentAt != "2026-10-17T22:00:00Z" {
		t.Fatal("HTTP boundary changed protocol identity or content", commands)
	}
	if !unknown.Data.Matches(commands[0]) || !accepted.Data.Matches(commands[0]) || attempts[0] == attempts[1] || accepted.Data.PublishAttemptID != attempts[1] {
		t.Fatal("retry lost original identity or reused attempt")
	}
	if strings.Contains(second.Body.String(), "seq") || strings.Contains(second.Body.String(), "delivered") || second.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("acceptance falsely claimed terminal state or was cacheable")
	}
	// A changed payload with the same key is still published for the consumer's
	// authoritative conflict decision; this endpoint never queries MySQL.
	_, changed := submitRequest(t, r, strings.Replace(submissionBody, `"content":" 中🙂\u0000\r\n"`, `"content":"changed"`, 1), context.Background())
	if changed.Data.State != "accepted" || changed.Data.CommandID != accepted.Data.CommandID || changed.Data.PayloadHash == accepted.Data.PayloadHash {
		t.Fatal("input conflict was prematurely decided or hash overwritten")
	}
}

func TestSubmitCommandInvalidRequestsNeverPublish(t *testing.T) {
	calls := 0
	pub := submissionPublisher(func(context.Context, messaging.Command, time.Time) (rabbitmq.PublishResult, error) {
		calls++
		return rabbitmq.PublishResult{}, nil
	})
	r := submissionRoute(t, pub, uint(42), time.Second)
	for _, body := range []string{
		``, `null`, `[]`, `{}`, submissionBody + `{}`,
		strings.Replace(submissionBody, `"targetId":"00034"`, `"targetId":34`, 1),
		strings.Replace(submissionBody, `"targetId":"00034"`, `"targetId":"18446744073709551616"`, 1),
		strings.Replace(submissionBody, `"media":"text"`, `"media":"Text"`, 1),
		strings.Replace(submissionBody, `"targetType":"private"`, `"targetType":"unknown"`, 1),
		strings.Replace(submissionBody, `"content":" 中🙂\u0000\r\n"`, `"content":""`, 1),
		strings.Replace(submissionBody, `"content":" 中🙂\u0000\r\n"`, `"content":"\ud800"`, 1),
		strings.Replace(submissionBody, `"content":" 中🙂\u0000\r\n"`, `"content":"`+strings.Repeat("x", 1025)+`"`, 1),
		strings.Replace(submissionBody, `"content":" 中🙂\u0000\r\n"`, "\"content\":\"\xff\"", 1),
		strings.Replace(submissionBody, `{`, `{"senderId":"99",`, 1),
		strings.Replace(submissionBody, `{`, `{"fromUser":{"id":99},`, 1),
		strings.Replace(submissionBody, `{`, `{"content":"duplicate",`, 1),
		strings.Replace(submissionBody, `{`, `{"\u0063ontent":"duplicate",`, 1),
		strings.Repeat(" ", messaging.MaxBodyBytes+1) + submissionBody,
	} {
		response, decoded := submitRequest(t, r, body, context.Background())
		if response.Code != 400 || decoded.Data.ReasonCode != "INVALID_REQUEST" || decoded.Code != -1 {
			t.Fatal("invalid request accepted", response.Code, decoded)
		}
	}
	if calls != 0 {
		t.Fatal("invalid input reached publisher", calls)
	}
}

type countedSubmissionBody struct{ read int }

func (b *countedSubmissionBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	b.read += len(p)
	return len(p), nil
}
func (*countedSubmissionBody) Close() error { return nil }

func TestSubmitCommandBodyAndIdentityBoundaries(t *testing.T) {
	for _, uid := range []any{nil, "42", uint64(42), uint(0), uint(42)} {
		body := &countedSubmissionBody{}
		request := httptest.NewRequest(http.MethodPost, "/", nil)
		request.Body, request.ContentLength = body, -1
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		r := submissionRoute(t, nil, uid, time.Second)
		r.ServeHTTP(response, request)
		if uid != uint(42) {
			if response.Code != 401 || body.read != 0 {
				t.Fatal("untrusted identity caused body read or admission", uid, response.Code, body.read)
			}
		} else if response.Code != 400 || body.read > messaging.MaxBodyBytes+1 {
			t.Fatal("streaming body exceeded byte limit", response.Code, body.read)
		}
	}
	for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded", "bad;"} {
		r := submissionRoute(t, nil, uint(42), time.Second)
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(submissionBody))
		request.Header.Set("Content-Type", contentType)
		response := httptest.NewRecorder()
		r.ServeHTTP(response, request)
		if response.Code != 400 {
			t.Fatal("unsupported content type accepted", contentType)
		}
	}
}

func TestSubmitCommandResultMappingAndContradictoryReceipts(t *testing.T) {
	for _, scenario := range []string{"accepted", "busy", "unavailable", "unroutable", "nacked", "unknown", "unknown_busy", "accepted_error", "wrong_command", "missing_attempt", "private_attempt", "zero_connection", "zero_channel", "zero_tag", "unknown_outcome", "not_started_without_error", "not_started_after_tag"} {
		t.Run(scenario, func(t *testing.T) {
			wantStatus, wantState, wantReason := 503, "publish_unknown", "PUBLISH_UNKNOWN"
			calls := 0
			pub := submissionPublisher(func(ctx context.Context, c messaging.Command, at time.Time) (rabbitmq.PublishResult, error) {
				calls++
				r := submissionReceipt(t, c, at)
				err := errors.New("PRIVATE_CAUSE amqp://private/path")
				switch scenario {
				case "accepted":
					wantStatus, wantState, wantReason = 202, "accepted", ""
					return r, nil
				case "busy", "unavailable":
					r = rabbitmq.PublishResult{Outcome: rabbitmq.PublishNotStarted}
					wantState, wantReason = "not_published", "DEPENDENCY_UNAVAILABLE"
					if scenario == "busy" {
						wantStatus, wantReason, err = 429, "OVERLOADED", rabbitmq.ErrPublisherBusy
					}
				case "unroutable":
					r.Outcome, wantState, wantReason = rabbitmq.PublishUnroutable, "unroutable", "PUBLISH_UNROUTABLE"
				case "nacked":
					r.Outcome, wantState, wantReason = rabbitmq.PublishNacked, "nacked", "DEPENDENCY_UNAVAILABLE"
				case "unknown":
					r.Outcome = rabbitmq.PublishUnknown
				case "unknown_busy":
					r.Outcome, err = rabbitmq.PublishUnknown, rabbitmq.ErrPublisherBusy
				case "accepted_error":
				case "wrong_command":
					r.CommandID, err = "PRIVATE_COMMAND", nil
				case "missing_attempt":
					r.PublishAttemptID, err = "", nil
				case "private_attempt":
					r.PublishAttemptID, err = "PRIVATE_ATTEMPT", nil
				case "zero_connection":
					r.ConnectionEpoch, err = 0, nil
				case "zero_channel":
					r.ChannelEpoch, err = 0, nil
				case "zero_tag":
					r.DeliveryTag, err = 0, nil
				case "unknown_outcome":
					r.Outcome = "PRIVATE_OUTCOME"
				case "not_started_without_error":
					r, err = rabbitmq.PublishResult{Outcome: rabbitmq.PublishNotStarted}, nil
				case "not_started_after_tag":
					r.Outcome, err = rabbitmq.PublishNotStarted, rabbitmq.ErrPublisherBusy
				}
				return r, err
			})
			response, decoded := submitRequest(t, submissionRoute(t, pub, uint(42), time.Second), submissionBody, context.Background())
			if response.Code != wantStatus || decoded.Data.State != wantState || decoded.Data.ReasonCode != wantReason || calls != 1 {
				t.Fatal("incorrect publish classification", response.Code, decoded)
			}
			if scenario == "busy" && response.Header().Get("Retry-After") != "1" {
				t.Fatal("overload lacked retry advice")
			}
			if strings.Contains(response.Body.String(), "PRIVATE") || strings.Contains(response.Body.String(), "amqp://") {
				t.Fatal("response exposed publisher diagnostics", response.Body.String())
			}
		})
	}
}

func TestSubmitCommandDeadlinesCancellationAndNilDependencies(t *testing.T) {
	for _, duration := range []time.Duration{0, -1, time.Minute + 1} {
		if handler, err := SubmitCommand(nil, duration); handler != nil || !errors.Is(err, ErrSubmissionOptions) {
			t.Fatal("invalid submission deadline accepted")
		}
	}
	var typedNil submissionPublisher
	for _, p := range []CommandPublisher{nil, typedNil} {
		response, data := submitRequest(t, submissionRoute(t, p, uint(42), time.Second), submissionBody, context.Background())
		if response.Code != 503 || data.Data.State != "not_published" || data.Data.CommandID == "" {
			t.Fatal("missing publisher was not a pre-publish dependency failure", data)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := submissionRoute(t, submissionPublisher(func(context.Context, messaging.Command, time.Time) (rabbitmq.PublishResult, error) {
		t.Fatal("pre-cancelled request published")
		return rabbitmq.PublishResult{}, nil
	}), uint(42), time.Second)
	if response, data := submitRequest(t, r, submissionBody, ctx); response.Code != 503 || data.Data.State != "not_published" {
		t.Fatal("pre-cancelled request became unknown", data)
	}
	parent, cancelParent := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelParent()
	pub := submissionPublisher(func(ctx context.Context, c messaging.Command, at time.Time) (rabbitmq.PublishResult, error) {
		deadline, _ := ctx.Deadline()
		if remaining := time.Until(deadline); remaining > 30*time.Millisecond {
			t.Fatal("parent deadline extended")
		}
		r := submissionReceipt(t, c, at)
		<-ctx.Done()
		r.Outcome = rabbitmq.PublishUnknown
		return r, ctx.Err()
	})
	if response, data := submitRequest(t, submissionRoute(t, pub, uint(42), time.Second), submissionBody, parent); response.Code != 503 || data.Data.State != "publish_unknown" {
		t.Fatal("in-flight cancellation lost uncertainty", data)
	}
	// A complete, definite receipt is not downgraded just because cancellation
	// arrives before the HTTP writer runs. The client may lose this response.
	known, cancelKnown := context.WithCancel(context.Background())
	pub = submissionPublisher(func(_ context.Context, c messaging.Command, at time.Time) (rabbitmq.PublishResult, error) {
		r := submissionReceipt(t, c, at)
		cancelKnown()
		return r, nil
	})
	if response, data := submitRequest(t, submissionRoute(t, pub, uint(42), time.Second), submissionBody, known); response.Code != 202 || data.Data.State != "accepted" {
		t.Fatal("known confirmation was downgraded", data)
	}
}

var _ io.ReadCloser = (*countedSubmissionBody)(nil)

type submissionReadFunc func([]byte) (int, error)

func (f submissionReadFunc) Read(p []byte) (int, error) { return f(p) }

func TestSubmitCommandExactBodyLimitAndCanceledRead(t *testing.T) {
	calls := 0
	pub := submissionPublisher(func(_ context.Context, c messaging.Command, at time.Time) (rabbitmq.PublishResult, error) {
		calls++
		return submissionReceipt(t, c, at), nil
	})
	r := submissionRoute(t, pub, uint(42), time.Second)
	exact := submissionBody + strings.Repeat(" ", messaging.MaxBodyBytes-len(submissionBody))
	if response, data := submitRequest(t, r, exact, context.Background()); response.Code != 202 || data.Data.State != "accepted" {
		t.Fatal("exact byte limit was rejected", data)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
	request.Body = io.NopCloser(submissionReadFunc(func([]byte) (int, error) { cancel(); return 0, io.ErrUnexpectedEOF }))
	request.ContentLength = -1
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	r.ServeHTTP(response, request)
	if response.Code != 503 || !strings.Contains(response.Body.String(), "DEPENDENCY_UNAVAILABLE") || calls != 1 {
		t.Fatal("canceled body read was misclassified as invalid input or published", response.Code, response.Body.String())
	}
}
