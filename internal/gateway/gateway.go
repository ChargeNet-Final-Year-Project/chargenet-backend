// Package gateway separates session orchestration from OCPI and local-simulator transports.
package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/chargenet/chargenet-backend/internal/simulation"
)

// Gateway statuses mirror the store without importing its database dependencies.
const (
	StatusCharging  = "CHARGING"
	StatusCompleted = "COMPLETED"
	StatusFailed    = "FAILED"
)

// StartCmd is everything a gateway needs to run one session: who it belongs to,
// which EVSE it runs on, and the physics parameters it charges with.
type StartCmd struct {
	TariffCurrency string
	SessionID      string
	EVSEUID        string
	Params         simulation.Params
	// StartSoC is the state of charge the session begins at. the session manager draws it with
	// simulation.StartSoC (15–35 %).
	StartSoC float64
	// Resume continues a session from the state stored before a backend
	// restart. When set, StartSoC is
	// ignored and the stored state is the session's starting point.
	Resume *simulation.State
}

// Telemetry is one gateway event: either a live snapshot (Status CHARGING) or
// the terminal event of a session (Status COMPLETED or FAILED, with the reason
// it stopped).
type Telemetry struct {
	SessionID string
	State     simulation.State
	Status    string
	Reason    string
}

// Terminal reports whether t is the last event its session will ever emit.
func (t Telemetry) Terminal() bool {
	return t.Status == StatusCompleted || t.Status == StatusFailed
}

// CPOGateway routes commands and publishes telemetry for all sessions. Callers
// fan out its subscription by SessionID; contexts bound remote network operations.
type CPOGateway interface {
	// StartSession begins charging. It returns nil once the session is running
	// (ACCEPTED), or an [Error] carrying CPO_UNREACHABLE,
	// CONNECTOR_UNAVAILABLE or SESSION_ALREADY_ACTIVE.
	StartSession(ctx context.Context, cmd StartCmd) error
	// StopSession asks the CPO to stop charging and returns once the session
	// has emitted its terminal event.
	StopSession(ctx context.Context, sessionID string) error
	// UnlockConnector releases the cable on an EVSE.
	UnlockConnector(ctx context.Context, evseUID string) error
	// Subscribe returns the channel every session event is published to.
	Subscribe() <-chan Telemetry
}

// Gateway error codes map directly to the driver API's error contract.
const (
	CodeCPOUnreachable       = "CPO_UNREACHABLE"
	CodeConnectorUnavailable = "CONNECTOR_UNAVAILABLE"
	CodeSessionNotFound      = "SESSION_NOT_FOUND"
	CodeSessionAlreadyActive = "SESSION_ALREADY_ACTIVE"
	CodeValidation           = "VALIDATION_ERROR"
)

// Error is a gateway failure carrying the API error code for it.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Errorf builds an [Error] with a formatted message.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// CodeOf returns the API error code carried by err, or "" when err is not a
// gateway error — callers then fall back to INTERNAL.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// IsCode reports whether err carries the given API error code.
func IsCode(err error, code string) bool { return CodeOf(err) == code }
