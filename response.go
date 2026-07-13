// Package models holds data-transfer objects shared across the API layer:
// the response envelope here, plus (in later milestones) block, transaction,
// wallet, and log DTOs shaped for JSON output.
package models

import "time"

// APIResponse is the single, consistent JSON envelope returned by every
// endpoint in the service, success or failure. Clients can always check
// "success" and then look at either "data" or "error".
type APIResponse struct {
	Success   bool        `json:"success"`
	Data      interface{} `json:"data,omitempty"`
	Error     *APIError   `json:"error,omitempty"`
	Timestamp time.Time   `json:"timestamp"`
}

// APIError is a machine-readable error description.
type APIError struct {
	// Code is a short, stable, machine-readable identifier, e.g.
	// "invalid_address", "block_not_found", "upstream_rpc_error".
	Code string `json:"code"`
	// Message is a human-readable description safe to show to API
	// consumers (never includes internal error details or stack traces).
	Message string `json:"message"`
}

// Success wraps data in a successful APIResponse envelope.
func Success(data interface{}) APIResponse {
	return APIResponse{
		Success:   true,
		Data:      data,
		Timestamp: time.Now().UTC(),
	}
}

// Failure wraps a code/message pair in a failed APIResponse envelope.
func Failure(code, message string) APIResponse {
	return APIResponse{
		Success:   false,
		Error:     &APIError{Code: code, Message: message},
		Timestamp: time.Now().UTC(),
	}
}
