package store

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func newError(code, message string) error {
	return &Error{Code: code, Message: message}
}

func notFound(entity string) error {
	return newError("NOT_FOUND", fmt.Sprintf("%s was not found", entity))
}

// IsRetryable reports whether err is a PostgreSQL failure that rolled back the
// transaction because of contention rather than invalid input: a lock or
// statement timeout, a deadlock, or a serialization failure.
func IsRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "55P03", "57014", "40P01", "40001":
		return true
	}
	return false
}
