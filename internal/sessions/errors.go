// Package sessions is the session orchestrator of the API contract: it
// owns everything that happens around a charge — the ordered start validation,
// the telemetry persistence, the auto-stop reasons the CPO cannot see (wallet
// and clock), settlement, and the SSE fan-out the mobile monitor listens to.
//
// It talks to the outside world through two seams only: [store.Store] for
// durable state and [gateway.CPOGateway] for the charge itself, so the local
// simulator and the OCPI CPO gateway are interchangeable.
package sessions

import (
	"errors"
	"fmt"

	"github.com/chargenet/chargenet-backend/internal/gateway"
)

// API error codes the orchestrator raises on top of the ones
// a gateway already reports.
const (
	CodeValidation            = "VALIDATION_ERROR"
	CodeInsufficientBalance   = "INSUFFICIENT_BALANCE"
	CodeIncompatibleConnector = "INCOMPATIBLE_CONNECTOR"
	CodeConnectorUnavailable  = "CONNECTOR_UNAVAILABLE"
	CodeSessionAlreadyActive  = "SESSION_ALREADY_ACTIVE"
	CodeSessionNotFound       = "SESSION_NOT_FOUND"
	CodeNoActiveSession       = "NO_ACTIVE_SESSION"
	CodeEVSENotFound          = "EVSE_NOT_FOUND"
	CodeVehicleNotFound       = "VEHICLE_NOT_FOUND"
	CodeCPOUnreachable        = "CPO_UNREACHABLE"
)

// Error is a domain failure carrying the machine-readable code the HTTP layer
// maps onto a status. Required and Balance are only set by
// INSUFFICIENT_BALANCE, whose 402 body the mobile pre-charge check renders.
type Error struct {
	Code     string
	Message  string
	Required *float64
	Balance  *float64
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Errorf builds an [Error] with a formatted message.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// CodeOf returns the API error code carried by err, or "" when err carries
// none (the caller then falls back to INTERNAL).
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return gateway.CodeOf(err)
}

// CodeIs reports whether err carries the given API error code.
func CodeIs(err error, code string) bool { return CodeOf(err) == code }

// fromGateway re-labels a gateway failure as a domain error without touching
// its code, so one mapping function covers both sources.
func fromGateway(err error) error {
	code := gateway.CodeOf(err)
	if code == "" {
		return err
	}
	switch code {
	case gateway.CodeSessionNotFound:
		code = CodeSessionNotFound
	case gateway.CodeConnectorUnavailable:
		code = CodeConnectorUnavailable
	case gateway.CodeSessionAlreadyActive:
		code = CodeSessionAlreadyActive
	case gateway.CodeCPOUnreachable:
		code = CodeCPOUnreachable
	case gateway.CodeValidation:
		code = CodeValidation
	}
	return &Error{Code: code, Message: err.Error()}
}
