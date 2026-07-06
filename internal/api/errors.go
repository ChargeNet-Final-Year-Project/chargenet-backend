package api

import (
	"encoding/json"
	"log"
	"net/http"
)

// ErrorCode values are the machine-readable codes from the API contract
type ErrorCode string

const (
	CodeValidation           ErrorCode = "VALIDATION_ERROR"
	CodeInvalidPaymentMethod ErrorCode = "INVALID_PAYMENT_METHOD"
	CodeUnauthorized         ErrorCode = "UNAUTHORIZED"
	CodeInvalidCredentials   ErrorCode = "INVALID_CREDENTIALS"
	CodeInsufficientBalance  ErrorCode = "INSUFFICIENT_BALANCE"
	CodeForbidden            ErrorCode = "FORBIDDEN"
	CodeStationNotFound      ErrorCode = "STATION_NOT_FOUND"
	CodeEVSENotFound         ErrorCode = "EVSE_NOT_FOUND"
	CodeVehicleNotFound      ErrorCode = "VEHICLE_NOT_FOUND"
	CodeSessionNotFound      ErrorCode = "SESSION_NOT_FOUND"
	CodeNoActiveSession      ErrorCode = "NO_ACTIVE_SESSION"
	CodeEmailTaken           ErrorCode = "EMAIL_TAKEN"
	CodeConnectorUnavailable ErrorCode = "CONNECTOR_UNAVAILABLE"
	CodeSessionAlreadyActive ErrorCode = "SESSION_ALREADY_ACTIVE"
	// CodeRequestInProgress is returned when a duplicate Idempotency-Key request
	// arrives while the original is still being processed. It is an additive
	// extension to the section 2.7 table, which does not cover this case.
	CodeRequestInProgress ErrorCode = "REQUEST_IN_PROGRESS"
	CodeIncompatibleConn  ErrorCode = "INCOMPATIBLE_CONNECTOR"
	CodeCPOUnreachable    ErrorCode = "CPO_UNREACHABLE"
	// CodeRateLimited is the portal login throttle: 5 failed
	// attempts per 5 minutes answer 429.
	CodeRateLimited ErrorCode = "RATE_LIMITED"
	CodeInternal    ErrorCode = "INTERNAL"
)

// apiErrorBody is the body of every non-2xx JSON response:
//
//	{"error":{"code":"UPPER_SNAKE","message":"human readable"}}
//
// Optional fields are only serialised when set.
type apiErrorBody struct {
	Code     ErrorCode         `json:"code"`
	Message  string            `json:"message"`
	Fields   map[string]string `json:"fields,omitempty"`
	Required *float64          `json:"required,omitempty"`
	Balance  *float64          `json:"balance,omitempty"`
}

type errorEnvelope struct {
	Error apiErrorBody `json:"error"`
}

// WriteJSON serialises v with the given status code.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("api: encode response: %v", err)
	}
}

// WriteError emits the standard error envelope.
func WriteError(w http.ResponseWriter, status int, code ErrorCode, message string) {
	WriteJSON(w, status, errorEnvelope{Error: apiErrorBody{Code: code, Message: message}})
}

// WriteFieldError emits VALIDATION_ERROR with per-field details (used by the
// portal registration forms).
func WriteFieldError(w http.ResponseWriter, message string, fields map[string]string) {
	WriteJSON(w, http.StatusBadRequest, errorEnvelope{Error: apiErrorBody{
		Code:    CodeValidation,
		Message: message,
		Fields:  fields,
	}})
}

// WriteInsufficientBalance emits 402 with the required amount and the current
// balance attached to the error object.
func WriteInsufficientBalance(w http.ResponseWriter, required, balance float64) {
	WriteJSON(w, http.StatusPaymentRequired, errorEnvelope{Error: apiErrorBody{
		Code:     CodeInsufficientBalance,
		Message:  "Wallet balance is too low to start charging.",
		Required: &required,
		Balance:  &balance,
	}})
}

// DecodeJSON reads a JSON request body into dst, returning a VALIDATION_ERROR
// friendly error on failure.
func DecodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}
