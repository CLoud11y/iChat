package service

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"reflect"
	"time"

	"github.com/gin-gonic/gin"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/utils"
)

var ErrSubmissionOptions = errors.New("invalid command submission options")

// CommandPublisher is the confirmed publish capability, without DB/Redis or
// publisher lifecycle ownership. A call must honor its context and never retry.
type CommandPublisher interface {
	Publish(context.Context, messaging.Command, time.Time) (rabbitmq.PublishResult, error)
}

// SubmitCommand is independent of legacy SendMsg. Only a trusted middleware's
// typed uid can authorize it. HTTP server read/header timeouts belong to its
// eventual API owner; this handler bounds body memory and publisher execution.
func SubmitCommand(publisher CommandPublisher, timeout time.Duration) (gin.HandlerFunc, error) {
	if timeout <= 0 || timeout > time.Minute {
		return nil, ErrSubmissionOptions
	}
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		uid := c.GetUint("uid")
		if uid == 0 {
			utils.RespondAuthError(c, utils.ErrUnauthenticated)
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		receivedAt := time.Now()
		data := messaging.Submission{State: "not_published"}
		if ctx.Err() != nil {
			submitError(c, data, 503, "DEPENDENCY_UNAVAILABLE", "submission unavailable")
			return
		}
		contentType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if err != nil || contentType != "application/json" || c.GetHeader("Content-Encoding") != "" || c.Request.ContentLength > messaging.MaxBodyBytes {
			submitError(c, data, 400, "INVALID_REQUEST", "invalid send request")
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, messaging.MaxBodyBytes)
		body, err := io.ReadAll(c.Request.Body)
		if ctx.Err() != nil {
			submitError(c, data, 503, "DEPENDENCY_UNAVAILABLE", "submission unavailable")
			return
		}
		if err != nil {
			submitError(c, data, 400, "INVALID_REQUEST", "invalid send request")
			return
		}
		request, err := messaging.DecodeRequest(body)
		if err != nil {
			submitError(c, data, 400, "INVALID_REQUEST", "invalid send request")
			return
		}
		command, err := messaging.Normalize(uint64(uid), request)
		if err != nil {
			submitError(c, data, 400, "INVALID_REQUEST", "invalid send request")
			return
		}
		data.CommandID, data.ClientMessageID, data.PayloadHash = command.CommandID, command.ClientMessageID, command.PayloadHash
		if ctx.Err() != nil || nilCommandPublisher(publisher) {
			submitError(c, data, 503, "DEPENDENCY_UNAVAILABLE", "submission unavailable")
			return
		}
		result, publishErr := publisher.Publish(ctx, command, receivedAt)
		// Never echo arbitrary adapter metadata or diagnostic errors. Identity
		// always comes from our normalized request, even after a broken result.
		if messaging.ValidPublishAttemptID(result.PublishAttemptID) {
			data.PublishAttemptID = result.PublishAttemptID
		}
		complete := result.CommandID == command.CommandID && data.PublishAttemptID != "" && result.ConnectionEpoch != 0 && result.ChannelEpoch != 0 && result.DeliveryTag != 0
		switch {
		case result.Outcome == rabbitmq.PublishAccepted && publishErr == nil && complete:
			data.State = "accepted"
			c.JSON(http.StatusAccepted, messaging.SubmissionResponse{Code: 0, Msg: "accepted", Data: data})
		case result.Outcome == rabbitmq.PublishNotStarted && publishErr != nil && result.DeliveryTag == 0 && (result.CommandID == "" || result.CommandID == command.CommandID):
			if errors.Is(publishErr, rabbitmq.ErrPublisherBusy) {
				c.Header("Retry-After", "1")
				submitError(c, data, 429, "OVERLOADED", "submission overloaded")
			} else {
				submitError(c, data, 503, "DEPENDENCY_UNAVAILABLE", "submission unavailable")
			}
		case result.Outcome == rabbitmq.PublishUnroutable && publishErr != nil && complete:
			data.State = "unroutable"
			submitError(c, data, 503, "PUBLISH_UNROUTABLE", "command could not be routed")
		case result.Outcome == rabbitmq.PublishNacked && publishErr != nil && complete:
			data.State = "nacked"
			submitError(c, data, 503, "DEPENDENCY_UNAVAILABLE", "command was negatively acknowledged")
		default:
			// A failed or contradictory post-call result cannot prove absence.
			// Do not classify it by err alone, or turn unknown+busy into 429.
			data.State = "publish_unknown"
			submitError(c, data, 503, "PUBLISH_UNKNOWN", "publish outcome unknown")
		}
	}, nil
}

func submitError(c *gin.Context, data messaging.Submission, status int, reason, message string) {
	data.ReasonCode = reason
	c.AbortWithStatusJSON(status, messaging.SubmissionResponse{Code: -1, Msg: message, Data: data})
}

func nilCommandPublisher(publisher CommandPublisher) bool {
	if publisher == nil {
		return true
	}
	v := reflect.ValueOf(publisher)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
