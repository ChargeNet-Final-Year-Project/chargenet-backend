package ocpi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

const LegacyVersion = "2.2.1"

type versionContextKey struct{}

func RequestVersion(r *http.Request) string {
	if v, ok := r.Context().Value(versionContextKey{}).(string); ok {
		return v
	}
	return VersionNumber
}

// LegacyAdapter translates only the supported, tax-free energy subset. It
// preserves the normal authentication/tenant handlers and never advertises
// Payments, Bookings or other unsupported modules.
func LegacyAdapter(next http.Handler, legacyRoot, currentRoot string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clone := r.Clone(context.WithValue(r.Context(), versionContextKey{}, LegacyVersion))
		clone.URL.Path = strings.Replace(r.URL.Path, legacyRoot, currentRoot, 1)
		response := &bufferedResponse{header: make(http.Header), status: 200}
		next.ServeHTTP(response, clone)
		for k, v := range response.header {
			w.Header()[k] = v
		}
		raw := bytes.ReplaceAll(response.body.Bytes(), []byte(currentRoot), []byte(legacyRoot))
		var doc any
		if json.Unmarshal(raw, &doc) == nil {
			legacyPrices(doc)
			raw, _ = json.Marshal(doc)
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(response.status)
		_, _ = w.Write(raw)
	})
}

type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *bufferedResponse) Header() http.Header         { return w.header }
func (w *bufferedResponse) WriteHeader(s int)           { w.status = s }
func (w *bufferedResponse) Write(p []byte) (int, error) { return w.body.Write(p) }
func legacyPrices(v any) {
	switch x := v.(type) {
	case map[string]any:
		if p, ok := x["before_taxes"]; ok {
			x["excl_vat"] = p
			delete(x, "before_taxes")
			delete(x, "taxes")
		}
		delete(x, "tax_included")
		if x["version"] == VersionNumber {
			x["version"] = LegacyVersion
		}
		for _, child := range x {
			legacyPrices(child)
		}
	case []any:
		for _, child := range x {
			legacyPrices(child)
		}
	}
}

func CountryAlpha2(country string) string {
	switch strings.ToUpper(country) {
	case "MYS", "MY":
		return "MY"
	case "SGP", "SG":
		return "SG"
	case "THA", "TH":
		return "TH"
	}
	return ""
}
func CountryAlpha3(country string) string {
	switch CountryAlpha2(country) {
	case "MY":
		return "MYS"
	case "SG":
		return "SGP"
	case "TH":
		return "THA"
	}
	return ""
}
func CountryTimeZone(country string) string {
	switch CountryAlpha2(country) {
	case "SG":
		return "Asia/Singapore"
	case "TH":
		return "Asia/Bangkok"
	}
	return "Asia/Kuala_Lumpur"
}
