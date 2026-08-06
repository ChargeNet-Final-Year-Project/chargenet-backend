// Package simulation contains the pure charging model shared by the local
// gateway and OCPI emulators.
package simulation

import (
	"math"
	"math/rand"
	"strings"
)

// Fallbacks used when the driver has no vehicle on file.
const (
	VehicleAcceptKWAC = 11.0
	VehicleAcceptKWDC = 150.0

	// FallbackBatteryKWh is the assumed pack size for a driver without a
	// vehicle row.
	FallbackBatteryKWh = 60.0
)

// JitterPct is the ±1.5 % per-tick jitter the curve carries.
const JitterPct = 0.015

// Start SoC range for a new session.
const (
	MinStartSoC = 15.0
	MaxStartSoC = 35.0
)

// PowerKW returns the effective charging power in kW at state of charge soc
// (percent) for a connector whose instantaneous ceiling is capKW (see [CapKW]),
// following the three-segment curve of the API contract:
//
//	s < 10       cap * (0.6 + 0.04*s)          ramp in from 60 %
//	10 ≤ s ≤ 80  cap * (1.0 - 0.3*(s-10)/70)   gentle decline to 70 %
//	s > 80       cap * (0.7 - 0.6*(s-80)/20)   taper to 10 % at full
//
// rng adds the ±1.5 % per-tick jitter; pass nil for the ideal, deterministic
// curve (tests and the emulators' dry runs). The jittered value is clamped to
// [0, capKW] because a real connector cannot deliver more than its ceiling, and
// the charging model requires that power never exceeds capKW.
func PowerKW(soc, capKW float64, rng *rand.Rand) float64 {
	if capKW <= 0 {
		return 0
	}
	soc = math.Min(math.Max(soc, 0), 100)

	var factor float64
	switch {
	case soc < 10:
		factor = 0.6 + 0.04*soc
	case soc <= 80:
		factor = 1.0 - 0.3*(soc-10)/70
	default:
		factor = 0.7 - 0.6*(soc-80)/20
	}

	kw := capKW * factor
	if rng != nil {
		kw *= 1 + JitterPct*(2*rng.Float64()-1)
	}
	return math.Min(math.Max(kw, 0), capKW)
}

// CapKW is the instantaneous ceiling for a connector: the smaller of the
// connector's rated power and what the vehicle will accept. A non-positive
// vehicleAcceptKW means no vehicle-side limit.
func CapKW(connectorMaxKW, vehicleAcceptKW float64) float64 {
	if vehicleAcceptKW <= 0 {
		return connectorMaxKW
	}
	return math.Min(connectorMaxKW, vehicleAcceptKW)
}

// VehicleAcceptKW is the acceptance to assume for a connector of the given
// power type when the driver has no vehicle on file: 150 kW for DC, 11 kW for
// AC, anything else treated as DC.
func VehicleAcceptKW(powerType string) float64 {
	if strings.EqualFold(powerType, "AC") {
		return VehicleAcceptKWAC
	}
	return VehicleAcceptKWDC
}

// StartSoC draws the state of charge a session begins at, 15–35 %.
func StartSoC(rng *rand.Rand) float64 {
	if rng == nil {
		return (MinStartSoC + MaxStartSoC) / 2
	}
	return MinStartSoC + rng.Float64()*(MaxStartSoC-MinStartSoC)
}

// Round2 rounds a ringgit amount to two decimals.
//
// It mirrors store.Round2 on purpose: money is rounded only at the edges
// , and this package must stay free of the database import that
// store.Round2 brings with it (the same trade-off geo.Round1 makes).
func Round2(v float64) float64 {
	return math.Round(v*100) / 100
}
