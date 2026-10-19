package workers

import (
	"context"
	"errors"
	"io"
	"testing"
)

func TestPersistenceShutdownCancellationKeepsPhaseResponsibility(t *testing.T) {
	for _, stage := range []string{"context", "subscription", "storage"} {
		canceled := persistenceServiceError(ErrPersistenceServiceStart, stage, context.Canceled)
		if !supervisorCancellationOnly(canceled) || supervisorCancellationOnly(errors.Join(canceled, io.EOF)) {
			t.Fatal("setup cancellation lost its phase or masked another fault", stage)
		}
	}
	for _, err := range []error{
		persistenceServiceError(ErrPersistenceServiceClose, "subscription", context.DeadlineExceeded),
		persistenceServiceError(ErrPersistenceServiceStopped, "process", context.Canceled),
		&PersistenceError{Kind: AcknowledgementFailed, cause: context.Canceled},
		persistenceServiceError(ErrPersistenceServiceStart, "subscription", errors.New("unclassified")),
	} {
		if supervisorCancellationOnly(err) {
			t.Fatal("shutdown discarded processing/ACK/cleanup responsibility", err)
		}
	}
}
