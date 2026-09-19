package database

import "errors"

var (
	ErrInvalidMessage        = errors.New("invalid message parameters")
	ErrMessageCursorNotFound = errors.New("message cursor not found")
)

const MaxMessagePageSize = 100
