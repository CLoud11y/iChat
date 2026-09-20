package database

import "errors"

var (
	ErrInvalidMessage        = errors.New("invalid message parameters")
	ErrMessageCursorNotFound = errors.New("message cursor not found")
	// ErrMessageEnqueueUnknown means RPUSH was invoked but no successful result
	// was observed. Even a final server rejection can follow a lost reply/retry.
	ErrMessageEnqueueUnknown = errors.New("legacy message enqueue outcome unknown")
)

const MaxMessagePageSize = 100
