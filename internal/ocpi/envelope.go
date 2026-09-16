package ocpi

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// OCPI status codes. Only 1000 is a success; the rest are the
// failures ChargeNet can report. 2000 is an additive code — the card names
// 1000/2001/2003/3001 — covering a malformed or incomplete request body, the
// same way internal/api added REQUEST_IN_PROGRESS to the 2.7 table.
const (
	// StatusSuccess is the only success code in the spec.
	StatusSuccess = 1000
	// StatusClientError is a generic client error (bad JSON, bad fields).
	StatusClientError = 2000
	// StatusUnauthorized means the Authorization token is unknown or spent.
	StatusUnauthorized = 2001
	// StatusUnknownLocation is used when a push names a location we do not know.
	StatusUnknownLocation = 2003
	// StatusUnableToUseAPI is the server-side failure the spec defines for
	// "we could not process this".
	StatusUnableToUseAPI = 3001
)

// Response is the OCPI envelope every endpoint wraps its payload in:
//
//	{"data": …, "status_code": 1000, "status_message": "Success", "timestamp": "…Z"}
type Response struct {
	Data          any    `json:"data"`
	StatusCode    int    `json:"status_code"`
	StatusMessage string `json:"status_message"`
	Timestamp     string `json:"timestamp"`
}

// OK writes a success envelope. httpStatus lets a handler answer 201 vs 200
// without leaving the spec's 1000 behind.
func OK(w http.ResponseWriter, httpStatus int, data any) {
	writeResponse(w, httpStatus, Response{
		Data:          data,
		StatusCode:    StatusSuccess,
		StatusMessage: "Success",
		Timestamp:     timestamp(),
	})
}

// Fail writes a failure envelope. `data` is always an empty object: the spec
// allows module-specific detail there and we never send any.
func Fail(w http.ResponseWriter, httpStatus, statusCode int, message string) {
	writeResponse(w, httpStatus, Response{
		Data:          map[string]any{},
		StatusCode:    statusCode,
		StatusMessage: message,
		Timestamp:     timestamp(),
	})
}

func writeResponse(w http.ResponseWriter, httpStatus int, resp Response) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(httpStatus)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("ocpi: encode response: %v", err)
	}
}

// timestamp renders the envelope timestamp: RFC 3339, UTC, whole seconds.
func timestamp() string { return time.Now().UTC().Format(time.RFC3339) }
