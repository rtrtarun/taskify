// Package domain holds Taskify's business types and rules: tasks, comments,
// the status state machine, and the events a change produces. It performs no
// I/O and imports nothing else from this project.
package domain

import (
	"errors"
	"fmt"
)

var (
	// ErrNotFound also covers a row that belongs to another tenant.
	ErrNotFound = errors.New("not found")
	// ErrVersionConflict means the caller's version of a task is stale.
	ErrVersionConflict = errors.New("version conflict")
	// ErrInvalidStateTransition means the status change is not in the matrix.
	ErrInvalidStateTransition = errors.New("invalid state transition")
	// ErrValidation means the input is well formed but not acceptable.
	ErrValidation = errors.New("validation failed")
	// ErrAlreadyExists means a row with the same identity exists.
	ErrAlreadyExists = errors.New("already exists")
	// ErrForbidden means the caller is known but may not take this action.
	ErrForbidden = errors.New("forbidden")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrValidation, fmt.Sprintf(format, args...))
}
