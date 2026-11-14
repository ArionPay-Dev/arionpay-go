package arionpay

import "fmt"

// APIError is returned when ArionPay answers with an error (or cannot be reached: Status 0).
type APIError struct {
	Status  int            // HTTP status, 0 when ArionPay could not be reached
	Code    string         // ArionPay error code, when given
	Message string         // human-readable message
	Body    map[string]any // decoded error body ("retryAfterSec" added from a Retry-After header)
	cause   error
}

// Unwrap gives the network error behind a Status 0 error, so errors.Is(err, context.Canceled) works.
func (e *APIError) Unwrap() error { return e.cause }

func (e *APIError) Error() string {
	if e.Status == 0 {
		return e.Message
	}
	return fmt.Sprintf("ArionPay API error (%d): %s", e.Status, e.Message)
}

// SignatureError means a webhook must not be trusted (bad or stale signature, wrong secret, not an ArionPay event).
type SignatureError struct{ Message string }

func (e *SignatureError) Error() string { return e.Message }
