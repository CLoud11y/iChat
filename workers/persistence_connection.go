package workers

import "errors"

var ErrPersistenceConnection = errors.New("persistence consumer connection unavailable")

// NewPersistenceConnectionError marks a failed connection preparation phase,
// where no delivery has been admitted. Only connection/topology factories may
// use this boundary; handler/ACK/cleanup failures retain their own phases.
func NewPersistenceConnectionError(cause error) error {
	return &persistenceConnectionFailure{cause: cause}
}

type persistenceConnectionFailure struct{ cause error }

func (e *persistenceConnectionFailure) Error() string    { return ErrPersistenceConnection.Error() }
func (e *persistenceConnectionFailure) GoString() string { return e.Error() }
func (e *persistenceConnectionFailure) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrPersistenceConnection}
	}
	return []error{ErrPersistenceConnection, e.cause}
}
