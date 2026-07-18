// Package ai defines the contract inkflow's importer uses to talk to an
// external AI backend that turns an uploaded PDF into a transcription
// and a short summary.
package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Provider runs OCR + summary on a PDF and returns the structured result.
// Implementations must be safe for concurrent use.
type Provider interface {
	Process(ctx context.Context, pdf io.Reader) (Result, error)
}

// Result is the structured output of a Provider call.
type Result struct {
	OCR     string
	Summary []string
}

// APIError describes a non-success HTTP response from an AI provider.
// Message is the provider's human-readable error message.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%d: %s", e.StatusCode, e.Message)
}

// IsRetryable reports whether err is worth retrying. Client errors are
// permanent except for rate limiting; server and unknown errors may be
// transient and are retried. Wrapped APIErrors are classified by status code.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return true
	}
	return apiErr.StatusCode == 429 || apiErr.StatusCode < 400 || apiErr.StatusCode >= 500
}
