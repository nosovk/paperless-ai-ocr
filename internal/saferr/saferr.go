// Package saferr provides categorized errors with safe public messages.
package saferr

import (
	"fmt"
	"io"
)

// Category identifies an error class for metrics and logging.
type Category string

const (
	CategoryConfiguration Category = "configuration"
	CategoryPaperless     Category = "paperless"
	CategoryProvider      Category = "provider"
	CategoryValidation    Category = "validation"
	CategoryRendering     Category = "rendering"
	CategoryInternal      Category = "internal"
)

// Error contains a safe operator-facing message and an optional private cause.
type Error struct {
	category Category
	message  string
	cause    error
}

// New creates a categorized error containing only a safe public message.
func New(category Category, message string) error {
	return &Error{category: category, message: message}
}

// Wrap creates a categorized error while retaining cause for errors.Is and
// errors.As. The cause is never included in formatted output.
func Wrap(category Category, message string, cause error) error {
	return &Error{category: category, message: message, cause: cause}
}

// Error returns only categorized, operator-facing information.
func (err *Error) Error() string {
	return string(err.category) + ": " + err.message
}

// Format ensures all string and error formatting paths use only the safe
// operator-facing representation.
func (err *Error) Format(state fmt.State, verb rune) {
	switch verb {
	case 's', 'v':
		io.WriteString(state, err.Error())
	case 'q':
		fmt.Fprintf(state, "%q", err.Error())
	default:
		fmt.Fprintf(state, "%%!%c(*saferr.Error=%s)", verb, err.Error())
	}
}

// Category returns the error category.
func (err *Error) Category() Category {
	return err.category
}

// Unwrap exposes the private cause to errors.Is and errors.As.
func (err *Error) Unwrap() error {
	return err.cause
}
