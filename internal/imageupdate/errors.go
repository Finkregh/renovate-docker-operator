package imageupdate

import (
	"errors"
	"fmt"
)

// ErrorClass categorizes poll failures so callers can decide whether to
// retry, alert, or degrade. Instances are populated by classifyErr and
// carry the underlying cause for errors.Is/As unwrapping.
type ErrorClass string

const (
	// ClassNetwork covers transport-level failures: connection refused,
	// DNS, TLS, timeouts, unexpected non-2xx/4xx statuses.
	ClassNetwork ErrorClass = "network"

	// ClassAuth is bearer-token exchange failure or 401 from the realm
	// (private repo, no anonymous access).
	ClassAuth ErrorClass = "auth"

	// ClassParse is a malformed reference, missing Docker-Content-Digest,
	// JSON decode error, or a response body that exceeded the 1 MiB cap.
	ClassParse ErrorClass = "parse"

	// ClassNotFound is HTTP 404 from the registry.
	ClassNotFound ErrorClass = "not_found"
)

// ClassifiedError is the concrete type returned by classifyErr. Public
// so callers (poller, HTTP handler) can type-switch or errors.As it.
type ClassifiedError struct {
	Class ErrorClass
	Cause error
}

func (e *ClassifiedError) Error() string {
	return fmt.Sprintf("%s: %v", e.Class, e.Cause)
}

// Unwrap makes ClassifiedError transparent to errors.Is / errors.As on
// the underlying cause.
func (e *ClassifiedError) Unwrap() error {
	return e.Cause
}

// classifyErr wraps err with an ErrorClass. If err is already a
// *ClassifiedError, its class is preserved (we don't re-wrap).
func classifyErr(class ErrorClass, err error) error {
	if err == nil {
		return nil
	}
	var already *ClassifiedError
	if errors.As(err, &already) {
		return already
	}
	return &ClassifiedError{Class: class, Cause: err}
}
