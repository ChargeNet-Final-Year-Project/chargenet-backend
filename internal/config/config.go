// Package config parses process configuration from environment variables.
//
// It intentionally avoids third-party dependencies: a very small.env loader
// is included so `cp.env.example.env && go run./cmd/server` works without
// exporting variables by hand.
package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// Config holds every runtime knob.
type Config struct {
	ExchangeRateSource string
	Port               string
	DatabaseURL        string
	JWTSecret          string
	SimTimeScale       int
	PlatformFeePct     float64
	PublicBaseURL      string
	PortalOrigin       string // the CPO web portal's origin (CORS + cookie), e.g. http://localhost:5173
	MinStartBalance    float64
	Gateway            string // "ocpi" (default) or "local" (in-process simulator)
}

// Load reads configuration from the environment, falling back to the
// documented defaults. If a `.env` file exists in the working directory it is
// loaded first, but real environment variables always win.
func Load() Config {
	loadDotEnv(".env")

	return Config{
		ExchangeRateSource: getString("FX_RATE_SOURCE", ""),
		Port:               getString("PORT", "8080"),
		DatabaseURL:        getString("DATABASE_URL", "postgres://chargenet:chargenet@localhost:5432/chargenet?sslmode=disable"),
		JWTSecret:          getString("JWT_SECRET", "dev-secret-change-me"),
		SimTimeScale:       getInt("SIM_TIME_SCALE", 10),
		PlatformFeePct:     getFloat("PLATFORM_FEE_PCT", 5),
		PublicBaseURL:      strings.TrimRight(getString("PUBLIC_BASE_URL", "http://localhost:8080"), "/"),
		PortalOrigin:       strings.TrimRight(getString("PORTAL_ORIGIN", "http://localhost:5173"), "/"),
		MinStartBalance:    getFloat("MIN_START_BALANCE", 10.00),
		// "ocpi" is the default transport: the platform charges
		// through real OCPI commands. "local" keeps the in-process simulator
		// for debugging without the emulators running.
		Gateway: strings.ToLower(getString("GATEWAY", "ocpi")),
	}
}

// loadDotEnv populates the process environment from a KEY=VALUE file. Existing
// variables are never overwritten. Missing files are ignored.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
}

func getString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func getFloat(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return def
}
