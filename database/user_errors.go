package database

import (
	"errors"
	"fmt"
)

var (
	ErrStorageUnavailable = errors.New("storage dependency unavailable")
	ErrUserNotFound       = errors.New("user not found")
	ErrPhoneRegistered    = errors.New("phone is already registered")
)

func storageError(operation string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrStorageUnavailable, operation, err)
}
