package store

import "fmt"

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
